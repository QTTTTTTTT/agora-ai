package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestFromEnvPrefersEnvOverFile guards the documented precedence:
// when both VAR and VAR_FILE are set, the env var wins. If this is
// ever reversed, operators rotating secrets via env will see the file
// silently override their new value — a confusing footgun.
func TestFromEnvPrefersEnvOverFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("TEST_F29_SECRET", "from-env")
	t.Setenv("TEST_F29_SECRET_FILE", path)

	got, err := FromEnv("TEST_F29_SECRET")
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got != "from-env" {
		t.Errorf("expected env to win, got %q", got)
	}
}

// TestFromEnvReadsFileWhenEnvUnset confirms the _FILE fallback. Trim
// behaviour is verified separately: operators commonly use echo which
// appends a trailing newline.
func TestFromEnvReadsFileWhenEnvUnset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("TEST_F29_SECRET_FILE", path)

	got, err := FromEnv("TEST_F29_SECRET")
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if got != "from-file" {
		t.Errorf("expected from-file (trimmed), got %q", got)
	}
}

// TestFromEnvNilWhenNeitherSet verifies the no-credentials-configured
// path returns no error, leaving the decision of fatality to the caller.
func TestFromEnvNilWhenNeitherSet(t *testing.T) {
	got, err := FromEnv("DEFINITELY_NOT_SET_F29_XYZ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// TestFromEnvReportsFileReadError makes sure misconfigured paths
// produce a typed error rather than silently falling back to empty —
// otherwise a permissions issue could downgrade a prod deploy to using
// a default secret without anyone noticing.
func TestFromEnvReportsFileReadError(t *testing.T) {
	t.Setenv("TEST_F29_SECRET_FILE", "/path/that/does/not/exist/xyz")
	_, err := FromEnv("TEST_F29_SECRET")
	if err == nil {
		t.Fatal("expected error for unreadable file")
	}
}

// TestNewJWTKeyringRejectsDuplicateKid prevents a configuration where
// two keys have the same kid — verification would silently use the
// first match and operators rotating secrets wouldn't get the new key.
func TestNewJWTKeyringRejectsDuplicateKid(t *testing.T) {
	_, err := NewJWTKeyring([]JWTKey{
		{Kid: "k1", Secret: "a", Active: true},
		{Kid: "k1", Secret: "b"},
	})
	if err == nil {
		t.Fatal("expected duplicate kid error")
	}
}

// TestNewJWTKeyringRejectsMultipleActive prevents the dangerous case
// where two keys are flagged active — undefined which one signs new
// tokens, and a key rotation would silently fail.
func TestNewJWTKeyringRejectsMultipleActive(t *testing.T) {
	_, err := NewJWTKeyring([]JWTKey{
		{Kid: "k1", Secret: "a", Active: true},
		{Kid: "k2", Secret: "b", Active: true},
	})
	if err == nil {
		t.Fatal("expected multiple-active error")
	}
}

// TestNewJWTKeyringDefaultsFirstAsActive is the back-compat path:
// when nobody flags Active=true, the first key wins. This matches
// JSON-marshalling conventions where boolean defaults can be confusing.
func TestNewJWTKeyringDefaultsFirstAsActive(t *testing.T) {
	r, err := NewJWTKeyring([]JWTKey{
		{Kid: "k0", Secret: "a"},
		{Kid: "k1", Secret: "b"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Active().Kid != "k0" {
		t.Errorf("expected k0 as active, got %s", r.Active().Kid)
	}
}

// TestLookupKidReturnsFalseForUnknown is the security-critical case:
// an unknown kid must fail verification — never silently fall back to
// the active key.
func TestLookupKidReturnsFalseForUnknown(t *testing.T) {
	r, _ := NewJWTKeyring([]JWTKey{{Kid: "k1", Secret: "a", Active: true}})
	_, ok := r.LookupKid("nope")
	if ok {
		t.Fatal("expected unknown kid lookup to fail")
	}
}

// TestLoadJWTKeyringFromEnvFallsBackToLegacy verifies the migration
// path: deployments still using JWT_SECRET continue to work without
// any operator action. The synthesised key gets kid="default".
func TestLoadJWTKeyringFromEnvFallsBackToLegacy(t *testing.T) {
	t.Setenv("JWT_SECRETS_JSON", "")
	t.Setenv("JWT_SECRET", "single-key-secret")
	r, err := LoadJWTKeyringFromEnv()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	active := r.Active()
	if active.Kid != "default" || active.Secret != "single-key-secret" {
		t.Errorf("unexpected active key: %+v", active)
	}
}

// TestLoadJWTKeyringFromEnvJSONRoundTrip is the rotation use case:
// JWT_SECRETS_JSON gets parsed into a multi-key ring with the right
// active key.
func TestLoadJWTKeyringFromEnvJSONRoundTrip(t *testing.T) {
	t.Setenv("JWT_SECRETS_JSON", `[{"kid":"k1","secret":"sig","active":true},{"kid":"k0","secret":"old"}]`)
	t.Setenv("JWT_SECRET", "")
	r, err := LoadJWTKeyringFromEnv()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if r.Active().Kid != "k1" {
		t.Errorf("expected active kid k1, got %s", r.Active().Kid)
	}
	old, ok := r.LookupKid("k0")
	if !ok || old.Secret != "old" {
		t.Errorf("expected legacy k0 lookup, got %+v", old)
	}
}

// TestLoadJWTKeyringFromEnvErrorOnBoth is the misconfig fail-fast:
// when neither variable is set, the loader returns an error so the
// process refuses to start (rather than silently issuing tokens with
// an empty secret).
func TestLoadJWTKeyringFromEnvErrorOnBoth(t *testing.T) {
	t.Setenv("JWT_SECRETS_JSON", "")
	t.Setenv("JWT_SECRET", "")
	_, err := LoadJWTKeyringFromEnv()
	if err == nil {
		t.Fatal("expected error when no jwt config is set")
	}
}

// TestLoadJWTKeyringFromEnvBadJSON makes sure a typo in the env value
// is reported at startup with the offending field visible, not as a
// silent empty keyring.
func TestLoadJWTKeyringFromEnvBadJSON(t *testing.T) {
	t.Setenv("JWT_SECRETS_JSON", `[{"kid":"k1","secret":"a","active":true},,]`)
	_, err := LoadJWTKeyringFromEnv()
	if err == nil {
		t.Fatal("expected json parse error")
	}
	if !errors.Is(err, errors.New("")) && !contains(err.Error(), "JWT_SECRETS_JSON") {
		t.Errorf("expected env name in error, got %v", err)
	}
}

// TestConstantTimeEqualBytes guards the re-export. A regression that
// switched to bytes.Equal would re-introduce a timing oracle on the
// HMAC comparison.
func TestConstantTimeEqualBytes(t *testing.T) {
	if !ConstantTimeEqualBytes([]byte("abc"), []byte("abc")) {
		t.Error("expected equal")
	}
	if ConstantTimeEqualBytes([]byte("abc"), []byte("abd")) {
		t.Error("expected unequal")
	}
}

// TestEncryptionSecretReadsFromFile is the regression that proves the
// `_FILE` mount mode works for the model-config encryption secret.
// Six callers used to read MODEL_CONFIG_API_KEY_SECRET via os.Getenv
// directly, silently bypassing the file resolver and breaking every
// decrypt call on prod swarms that had moved to file-mounted secrets.
// Single call to EncryptionSecret() now routes all of them through
// FromEnv, so this one test guards the whole class.
func TestEncryptionSecretReadsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("aes-gcm-key-from-mounted-secret\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", "")
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET_FILE", path)
	t.Setenv("API_KEY_ENCRYPTION_SECRET", "")

	got, err := EncryptionSecret()
	if err != nil {
		t.Fatalf("EncryptionSecret: %v", err)
	}
	if got != "aes-gcm-key-from-mounted-secret" {
		t.Errorf("expected file contents (trimmed), got %q", got)
	}
}

// TestEncryptionSecretFallsBackToLegacyName covers the migration path:
// deployments still using API_KEY_ENCRYPTION_SECRET (older alias) keep
// working. Without this back-compat, upgrading to a release that uses
// EncryptionSecret() would break installs running on the older name.
func TestEncryptionSecretFallsBackToLegacyName(t *testing.T) {
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", "")
	t.Setenv("API_KEY_ENCRYPTION_SECRET", "legacy-name-value")

	got, err := EncryptionSecret()
	if err != nil {
		t.Fatalf("EncryptionSecret: %v", err)
	}
	if got != "legacy-name-value" {
		t.Errorf("expected fallback to legacy var, got %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
