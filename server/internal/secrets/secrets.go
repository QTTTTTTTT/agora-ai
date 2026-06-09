// Package secrets centralises how the server materialises configurable
// credentials (JWT signing keys, encryption keys, provider API tokens)
// from the environment.
//
// Two F29 ergonomics live here:
//
//  1. FromEnv / FromEnvWithDefault — read VAR or VAR_FILE. The _FILE
//     variant lets operators mount Kubernetes / Docker secrets as
//     files instead of exporting env vars to every child process.
//
//  2. JWTKeyring — multi-key signing/verification. The first key is
//     active (used to mint new tokens); the rest are kept for
//     verification only so a key rotation can roll forward without
//     invalidating already-issued tokens.
//
// All entropy / strength checks are owned by main.validateConfig; this
// package only handles materialisation.
package secrets

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// FromEnv returns the contents of VAR or VAR_FILE, whichever is set.
// Precedence: explicit env var wins over file. Returns empty string +
// nil error when neither is set (caller decides whether that's fatal).
//
// The file read trims trailing whitespace and newlines so operators can
// `echo "secret" > /run/secrets/jwt` without subtle truncation bugs.
func FromEnv(name string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, nil
	}
	if path := strings.TrimSpace(os.Getenv(name + "_FILE")); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("secrets: read %s from %q: %w", name, path, err)
		}
		return strings.TrimRight(string(raw), " \t\r\n"), nil
	}
	return "", nil
}

// FromEnvWithDefault is the convenience wrapper that returns fallback
// when neither VAR nor VAR_FILE is set. Used for non-secret config
// fields that prefer this same _FILE pattern for consistency.
func FromEnvWithDefault(name, fallback string) string {
	v, err := FromEnv(name)
	if err != nil || v == "" {
		return fallback
	}
	return v
}

// JWTKey is one signing/verification key. Kid is the JWT header key id
// so verifiers can route a presented token to the right secret.
type JWTKey struct {
	Kid    string `json:"kid"`
	Secret string `json:"secret"`
	Active bool   `json:"active"`
}

// JWTKeyring stores the keyring state required for safe rotation:
//   - exactly one key is "active" (used for new signatures)
//   - all keys are valid for verification
//   - lookup is keyed on Kid so unknown-kid presentations fail fast
//
// Construction is locked at process start; rotation is a process
// restart, not a hot-reload. This trades operational simplicity for
// the small cost of an extra rolling deploy when a rotation lands.
type JWTKeyring struct {
	keys       []JWTKey
	activeIdx  int
	byKid      map[string]int
}

// LoadJWTKeyringFromEnv reads JWT_SECRETS_JSON or JWT_SECRETS_JSON_FILE.
// Format: [{"kid":"k1","secret":"...","active":true},{"kid":"k0","secret":"..."}]
//
// Falls back to a single-key ring built from JWT_SECRET / JWT_SECRET_FILE
// when JWT_SECRETS_JSON is empty, preserving 100% backward compatibility
// with deployments that haven't adopted rotation yet.
func LoadJWTKeyringFromEnv() (*JWTKeyring, error) {
	raw, err := FromEnv("JWT_SECRETS_JSON")
	if err != nil {
		return nil, err
	}
	if raw == "" {
		legacy, err := FromEnv("JWT_SECRET")
		if err != nil {
			return nil, err
		}
		if legacy == "" {
			return nil, errors.New("secrets: neither JWT_SECRETS_JSON nor JWT_SECRET is set")
		}
		return NewJWTKeyring([]JWTKey{{Kid: "default", Secret: legacy, Active: true}})
	}
	var keys []JWTKey
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil, fmt.Errorf("secrets: parse JWT_SECRETS_JSON: %w", err)
	}
	return NewJWTKeyring(keys)
}

// NewJWTKeyring validates and indexes a slice of keys. Exposed so tests
// can construct rings without environment plumbing.
//
// Validation rules (any violation returns an error so misconfiguration
// surfaces at startup rather than at first token verification):
//   - at least one key
//   - exactly one Active=true (if zero are flagged, the first is used)
//   - no duplicate Kid values
//   - no empty Kid or Secret
func NewJWTKeyring(keys []JWTKey) (*JWTKeyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("secrets: jwt keyring is empty")
	}
	byKid := make(map[string]int, len(keys))
	activeIdx := -1
	for i, k := range keys {
		if strings.TrimSpace(k.Kid) == "" {
			return nil, fmt.Errorf("secrets: jwt key #%d has empty kid", i)
		}
		if strings.TrimSpace(k.Secret) == "" {
			return nil, fmt.Errorf("secrets: jwt key %q has empty secret", k.Kid)
		}
		if _, dup := byKid[k.Kid]; dup {
			return nil, fmt.Errorf("secrets: jwt keyring has duplicate kid %q", k.Kid)
		}
		byKid[k.Kid] = i
		if k.Active {
			if activeIdx != -1 {
				return nil, fmt.Errorf("secrets: jwt keyring has multiple active keys (%q and %q)", keys[activeIdx].Kid, k.Kid)
			}
			activeIdx = i
		}
	}
	if activeIdx == -1 {
		activeIdx = 0
	}
	return &JWTKeyring{keys: append([]JWTKey(nil), keys...), activeIdx: activeIdx, byKid: byKid}, nil
}

// Active returns the key used to mint new tokens.
func (r *JWTKeyring) Active() JWTKey {
	return r.keys[r.activeIdx]
}

// LookupKid returns the key matching kid (used for verification).
// Returns ok=false when the kid is unknown — callers MUST NOT
// silently fall back to the active key, as that would let an attacker
// re-sign a stolen token with the wrong-but-currently-active key.
func (r *JWTKeyring) LookupKid(kid string) (JWTKey, bool) {
	if r == nil {
		return JWTKey{}, false
	}
	i, ok := r.byKid[kid]
	if !ok {
		return JWTKey{}, false
	}
	return r.keys[i], true
}

// All returns a defensive copy of every key. Used by validateConfig
// to apply entropy checks uniformly.
func (r *JWTKeyring) All() []JWTKey {
	out := make([]JWTKey, len(r.keys))
	copy(out, r.keys)
	return out
}

// LegacyVerificationKeys returns every secret regardless of kid. Used
// for tokens missing a kid header (issued by pre-rotation builds). The
// returned slice is read-only.
func (r *JWTKeyring) LegacyVerificationKeys() [][]byte {
	out := make([][]byte, len(r.keys))
	for i, k := range r.keys {
		out[i] = []byte(k.Secret)
	}
	return out
}

// ConstantTimeEqualBytes is a tiny re-export so callers don't have to
// import crypto/subtle just for HMAC comparison.
func ConstantTimeEqualBytes(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// EncryptionSecret returns the symmetric AES-GCM key used to encrypt
// stored LLM provider API tokens. The canonical name is
// MODEL_CONFIG_API_KEY_SECRET; API_KEY_ENCRYPTION_SECRET is the older
// alias kept so legacy deployments do not break on upgrade.
//
// Returns "" + nil when neither is set so callers can decide whether
// to error out or warn (different sites have different appetites:
// the encryption write path treats it as fatal, the BYOK feature
// gate treats it as a feature flag).
//
// All reads go through FromEnv so the _FILE mount-pattern works
// uniformly. Before this helper existed, six call sites read these
// vars via os.Getenv directly, which silently returned "" whenever
// the operator had switched to MOUNT_FILE mode — leaving file-mounted
// prod swarms decrypt-broken in subtle ways.
func EncryptionSecret() (string, error) {
	for _, name := range []string{"MODEL_CONFIG_API_KEY_SECRET", "API_KEY_ENCRYPTION_SECRET"} {
		v, err := FromEnv(name)
		if err != nil {
			return "", fmt.Errorf("secrets.EncryptionSecret: read %s: %w", name, err)
		}
		if s := strings.TrimSpace(v); s != "" {
			return s, nil
		}
	}
	return "", nil
}
