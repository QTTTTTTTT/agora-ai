// Package totp implements the platform's 2FA primitives (P0-6).
//
// Why a thin wrapper over pquerna/otp
//
// The pquerna/otp library is the de-facto Go TOTP implementation
// and we use it directly for the math (RFC 6238). This package
// adds the bits the upstream library deliberately doesn't:
//
//   - Secret encryption at rest. The library hands you a base32
//     string; we wrap it in AES-GCM with a key sourced from
//     TOTP_ENCRYPTION_KEY so the database row alone is useless to
//     a leak. The repo persists the ciphertext; the handler thaws
//     it on each verification.
//
//   - Recovery codes. Single-use one-time codes generated at
//     enrolment, hashed with bcrypt before storage. We follow the
//     IBM-style 10×10-char alphanumeric layout (e.g. ABCD-EFGH-IJKL).
//
//   - Provisioning URI assembly. The library can build it but the
//     issuer/account label / digits / period defaults need to match
//     the persisted row, so we own the call site.
//
// Concurrency
//
// All exported functions are stateless and safe for concurrent use.
// The package holds NO global state — the encryption key is passed
// in by the caller (the auth handler) so unit tests can run without
// touching env vars.

package totp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pquerna/otp"
	pqotp "github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// ErrInvalidKey is returned by NewCipher when the supplied key is
// not 32 bytes (AES-256). Callers should surface this as an
// internal-error to the user — we cannot proceed without a valid
// key, and we don't want to silently fall back to plaintext.
var ErrInvalidKey = errors.New("totp: encryption key must be 32 bytes")

// ErrInvalidCiphertext is returned when Decrypt is called on a
// payload that is shorter than the AES-GCM nonce, or when the
// authentication tag fails. Both are bucketed together because the
// caller can't distinguish them and the right response is the same:
// reject the row.
var ErrInvalidCiphertext = errors.New("totp: encrypted secret is malformed or tampered")

// ErrInvalidCode is returned by Verify when the supplied code does
// not match the current TOTP window. The repo turns this into a
// 401 + bumped enrolment_attempts counter.
var ErrInvalidCode = errors.New("totp: code does not match")

// ---------------------------------------------------------------------------
// Secret generation
// ---------------------------------------------------------------------------

// EnrolmentParams controls the shape of a freshly minted secret.
// The defaults (6 digits, 30s period, SHA-1) match every popular
// authenticator app (Google Authenticator, Authy, 1Password, etc.).
type EnrolmentParams struct {
	// Issuer appears on the authenticator app's account row, e.g.
	// "FundAI". Required, non-empty.
	Issuer string
	// AccountName is the per-user label, typically the email. Required.
	AccountName string
	// Digits is 6, 7, or 8. Default 6.
	Digits int
	// Period is the rotation interval in seconds. Default 30.
	Period int
	// Algorithm is "SHA1" / "SHA256" / "SHA512". Default SHA1.
	Algorithm string
	// SecretSize is the byte length of the random secret BEFORE
	// base32-encoding. Default 20 (matches RFC 6238 recommendation).
	SecretSize uint
}

// Enrolment is the shape returned to the caller at setup time. The
// caller MUST persist EncryptedSecret + HashedRecoveryCodes and
// surface PlainSecret + ProvisioningURI + RecoveryCodes to the user
// EXACTLY ONCE — they are unrecoverable afterwards.
type Enrolment struct {
	// PlainSecret is the base32-encoded secret. Show it under a
	// "Can't scan? Enter this manually" affordance, then forget it.
	PlainSecret string
	// EncryptedSecret is what gets persisted in user_totp_secrets.
	// Always non-empty when err == nil.
	EncryptedSecret []byte
	// ProvisioningURI is the otpauth:// string consumed by the QR
	// renderer. Pass it straight to a QR library on the frontend.
	ProvisioningURI string
	// RecoveryCodes are the plaintext one-time codes shown to the
	// user once. Format: ABCD-EFGH (8 chars + dash for readability).
	RecoveryCodes []string
	// HashedRecoveryCodes are the bcrypt-hashed codes — what gets
	// persisted. Same length as RecoveryCodes; same order.
	HashedRecoveryCodes []string
	// Issuer / AccountName / Digits / Period / Algorithm round-trip
	// the params the caller supplied (with defaults applied) so the
	// repo can persist them and the verifier can rebuild the same
	// otp.Key without consulting env vars.
	Issuer      string
	AccountName string
	Digits      int
	Period      int
	Algorithm   string
}

// Enrol generates a fresh secret, encrypts it under the supplied
// AES-GCM cipher, and produces a fresh batch of recovery codes. The
// caller persists the encrypted secret + hashed recovery codes and
// shows the plaintext fields to the user once.
func Enrol(c cipher.AEAD, p EnrolmentParams) (*Enrolment, error) {
	if c == nil {
		return nil, ErrInvalidKey
	}
	if strings.TrimSpace(p.Issuer) == "" {
		return nil, fmt.Errorf("totp: issuer required")
	}
	if strings.TrimSpace(p.AccountName) == "" {
		return nil, fmt.Errorf("totp: account name required")
	}
	if p.Digits == 0 {
		p.Digits = 6
	}
	if p.Period == 0 {
		p.Period = 30
	}
	if p.Algorithm == "" {
		p.Algorithm = "SHA1"
	}
	if p.SecretSize == 0 {
		p.SecretSize = 20
	}

	algo, ok := parseAlgorithm(p.Algorithm)
	if !ok {
		return nil, fmt.Errorf("totp: unsupported algorithm %q", p.Algorithm)
	}
	digits, ok := parseDigits(p.Digits)
	if !ok {
		return nil, fmt.Errorf("totp: digits must be 6, 7, or 8 (got %d)", p.Digits)
	}

	key, err := pqotp.Generate(pqotp.GenerateOpts{
		Issuer:      p.Issuer,
		AccountName: p.AccountName,
		Period:      uint(p.Period),
		SecretSize:  p.SecretSize,
		Digits:      digits,
		Algorithm:   algo,
	})
	if err != nil {
		return nil, fmt.Errorf("totp: generate: %w", err)
	}

	encrypted, err := encrypt(c, []byte(key.Secret()))
	if err != nil {
		return nil, fmt.Errorf("totp: encrypt secret: %w", err)
	}

	plain, hashed, err := generateRecoveryCodes(10)
	if err != nil {
		return nil, fmt.Errorf("totp: recovery codes: %w", err)
	}

	return &Enrolment{
		PlainSecret:         key.Secret(),
		EncryptedSecret:     encrypted,
		ProvisioningURI:     key.URL(),
		RecoveryCodes:       plain,
		HashedRecoveryCodes: hashed,
		Issuer:              p.Issuer,
		AccountName:         p.AccountName,
		Digits:              p.Digits,
		Period:              p.Period,
		Algorithm:           p.Algorithm,
	}, nil
}

// VerifyParams holds the persisted shape needed to verify a code.
// All fields come straight off user_totp_secrets except Code, which
// the caller plumbs in from the request body.
type VerifyParams struct {
	EncryptedSecret []byte
	Code            string
	Digits          int
	Period          int
	Algorithm       string
	// At allows tests to pin a deterministic clock. Zero means
	// "now". Production callers leave this zero.
	At time.Time
}

// Verify decrypts the secret, then asks the upstream library
// whether the supplied code matches the current TOTP window. We
// allow ±1 step (= ±30s) of clock drift via Skew=1.
func Verify(c cipher.AEAD, p VerifyParams) error {
	if c == nil {
		return ErrInvalidKey
	}
	code := strings.TrimSpace(p.Code)
	if code == "" {
		return ErrInvalidCode
	}
	plain, err := decrypt(c, p.EncryptedSecret)
	if err != nil {
		return err
	}
	algo, ok := parseAlgorithm(p.Algorithm)
	if !ok {
		return fmt.Errorf("totp: unsupported algorithm %q", p.Algorithm)
	}
	digits, ok := parseDigits(p.Digits)
	if !ok {
		return fmt.Errorf("totp: digits must be 6, 7, or 8 (got %d)", p.Digits)
	}
	at := p.At
	if at.IsZero() {
		at = time.Now()
	}
	period := uint(p.Period)
	if period == 0 {
		period = 30
	}

	ok, err = pqotp.ValidateCustom(code, string(plain), at, pqotp.ValidateOpts{
		Period:    period,
		Skew:      1,
		Digits:    digits,
		Algorithm: algo,
	})
	if err != nil {
		return fmt.Errorf("totp: validate: %w", err)
	}
	if !ok {
		return ErrInvalidCode
	}
	return nil
}

// ---------------------------------------------------------------------------
// Recovery codes
// ---------------------------------------------------------------------------

// VerifyRecoveryCode returns the index of the matching hashed
// recovery code, or -1 when none match. Bcrypt comparisons are
// constant-time per-row but the for-loop adds a small linear
// component; that's an acceptable trade-off given the array size
// is bounded at 10. Callers MUST remove the matched code from the
// stored array (the repo does this in a single UPDATE).
func VerifyRecoveryCode(plaintext string, hashedCodes []string) int {
	plaintext = strings.TrimSpace(strings.ToUpper(plaintext))
	if plaintext == "" {
		return -1
	}
	for i, h := range hashedCodes {
		if bcrypt.CompareHashAndPassword([]byte(h), []byte(plaintext)) == nil {
			return i
		}
	}
	return -1
}

// generateRecoveryCodes produces n plaintext / hashed recovery code
// pairs. The plaintext is uppercase alphanumeric with a single dash
// in the middle for readability — a layout familiar from GitHub
// and other 2FA-providing services.
func generateRecoveryCodes(n int) ([]string, []string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // skip 0/1/I/O for legibility
	plain := make([]string, 0, n)
	hashed := make([]string, 0, n)
	for i := 0; i < n; i++ {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return nil, nil, err
		}
		// Map each byte into the alphabet by modulo. The bias is
		// tiny (alphabet length 32 divides 256 exactly) so this
		// is safe.
		raw := make([]byte, 8)
		for j, b := range buf {
			raw[j] = alphabet[int(b)%len(alphabet)]
		}
		code := string(raw[:4]) + "-" + string(raw[4:])
		h, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
		if err != nil {
			return nil, nil, err
		}
		plain = append(plain, code)
		hashed = append(hashed, string(h))
	}
	return plain, hashed, nil
}

// ---------------------------------------------------------------------------
// Cipher helpers
// ---------------------------------------------------------------------------

// NewCipher constructs an AES-256-GCM AEAD from a 32-byte key. The
// caller is expected to source the key from a secret manager / env
// var and reuse the cipher instance for the lifetime of the
// process — AEAD instances are safe for concurrent use.
func NewCipher(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// NewCipherFromHex is a convenience wrapper that decodes a 64-char
// hex string into 32 raw bytes and forwards to NewCipher. Returns
// ErrInvalidKey when the input doesn't decode or has the wrong
// length.
func NewCipherFromHex(hexKey string) (cipher.AEAD, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return nil, ErrInvalidKey
	}
	return NewCipher(raw)
}

func encrypt(c cipher.AEAD, plain []byte) ([]byte, error) {
	nonce := make([]byte, c.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	// Pack nonce || ciphertext so the row is self-describing.
	out := append(nonce, c.Seal(nil, nonce, plain, nil)...)
	return out, nil
}

func decrypt(c cipher.AEAD, payload []byte) ([]byte, error) {
	ns := c.NonceSize()
	if len(payload) < ns {
		return nil, ErrInvalidCiphertext
	}
	nonce, body := payload[:ns], payload[ns:]
	out, err := c.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

func parseAlgorithm(s string) (otp.Algorithm, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SHA1", "":
		return otp.AlgorithmSHA1, true
	case "SHA256":
		return otp.AlgorithmSHA256, true
	case "SHA512":
		return otp.AlgorithmSHA512, true
	}
	return otp.AlgorithmSHA1, false
}

func parseDigits(d int) (otp.Digits, bool) {
	switch d {
	case 0, 6:
		return otp.DigitsSix, true
	case 7:
		// pquerna/otp doesn't expose a 7-digit constant; fall
		// through to SIX so we never crash. The schema CHECK
		// constraint blocks this from happening anyway.
		return otp.DigitsSix, false
	case 8:
		return otp.DigitsEight, true
	}
	return otp.DigitsSix, false
}

// SecretBytes is exposed so tests / migration tools can inspect the
// raw bytes after decryption. Production callers should NOT use it.
func SecretBytes(c cipher.AEAD, encrypted []byte) ([]byte, error) {
	return decrypt(c, encrypted)
}

// EncodeSecret base32-encodes a raw secret without padding. Useful
// for tests that want to rebuild a Verify payload from a known
// secret. Production callers don't need this.
func EncodeSecret(raw []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
}
