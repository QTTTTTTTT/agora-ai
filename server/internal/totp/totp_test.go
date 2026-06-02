package totp

import (
	"errors"
	"strings"
	"testing"
	"time"

	pqotp "github.com/pquerna/otp/totp"
)

// fixedKey is a deterministic 32-byte key used by every test in
// this file. NEVER reuse it in production — it is checked in.
var fixedKey = []byte("0123456789abcdef0123456789abcdef")

func newTestCipher(t *testing.T) (cipher func() error, _ AEAD) {
	t.Helper()
	c, err := NewCipher(fixedKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return func() error { return nil }, c
}

// AEAD aliases the std crypto/cipher.AEAD so the helper function's
// return type stays compact.
type AEAD = interface {
	NonceSize() int
	Overhead() int
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}

func TestNewCipher_RejectsShortKey(t *testing.T) {
	if _, err := NewCipher([]byte("too-short")); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("err = %v, want ErrInvalidKey", err)
	}
	if _, err := NewCipher(nil); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("nil err = %v, want ErrInvalidKey", err)
	}
}

func TestNewCipherFromHex(t *testing.T) {
	if _, err := NewCipherFromHex("not-hex"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("not-hex err = %v, want ErrInvalidKey", err)
	}
	if _, err := NewCipherFromHex(strings.Repeat("ab", 32)); err != nil {
		t.Errorf("valid hex err = %v, want nil", err)
	}
}

// TestEnrol_RoundTrip pins the contract: a freshly enrolled user
// can verify their first code, the encrypted secret is opaque, and
// the recovery code list is bounded.
func TestEnrol_RoundTrip(t *testing.T) {
	_, c := newTestCipher(t)
	enr, err := Enrol(c, EnrolmentParams{
		Issuer:      "FundAI",
		AccountName: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}

	if enr.Digits != 6 || enr.Period != 30 || enr.Algorithm != "SHA1" {
		t.Errorf("defaults not applied: %+v", enr)
	}
	if !strings.HasPrefix(enr.ProvisioningURI, "otpauth://totp/") {
		t.Errorf("ProvisioningURI = %q, want otpauth:// prefix", enr.ProvisioningURI)
	}
	if !strings.Contains(enr.ProvisioningURI, "issuer=FundAI") {
		t.Errorf("ProvisioningURI missing issuer: %q", enr.ProvisioningURI)
	}
	if len(enr.RecoveryCodes) != 10 || len(enr.HashedRecoveryCodes) != 10 {
		t.Errorf("recovery codes len = %d/%d, want 10/10",
			len(enr.RecoveryCodes), len(enr.HashedRecoveryCodes))
	}
	for i, code := range enr.RecoveryCodes {
		if len(code) != 9 || code[4] != '-' {
			t.Errorf("code[%d]=%q malformed (want 4-4 alpha)", i, code)
		}
	}

	// Re-derive the current TOTP code from the plaintext secret
	// and confirm Verify accepts it.
	now := time.Date(2026, 5, 30, 14, 0, 0, 0, time.UTC)
	code, err := pqotp.GenerateCode(enr.PlainSecret, now)
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	if err := Verify(c, VerifyParams{
		EncryptedSecret: enr.EncryptedSecret,
		Code:            code,
		Digits:          enr.Digits,
		Period:          enr.Period,
		Algorithm:       enr.Algorithm,
		At:              now,
	}); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// And rejects a different code.
	if err := Verify(c, VerifyParams{
		EncryptedSecret: enr.EncryptedSecret,
		Code:            "000000",
		Digits:          enr.Digits,
		Period:          enr.Period,
		Algorithm:       enr.Algorithm,
		At:              now,
	}); !errors.Is(err, ErrInvalidCode) {
		t.Errorf("bad code err = %v, want ErrInvalidCode", err)
	}
}

// TestEnrol_Validation makes sure required fields are enforced.
func TestEnrol_Validation(t *testing.T) {
	_, c := newTestCipher(t)

	if _, err := Enrol(c, EnrolmentParams{AccountName: "x"}); err == nil {
		t.Errorf("missing issuer accepted")
	}
	if _, err := Enrol(c, EnrolmentParams{Issuer: "x"}); err == nil {
		t.Errorf("missing account accepted")
	}
	if _, err := Enrol(c, EnrolmentParams{Issuer: "x", AccountName: "y", Algorithm: "MD5"}); err == nil {
		t.Errorf("unsupported algorithm accepted")
	}
	if _, err := Enrol(nil, EnrolmentParams{Issuer: "x", AccountName: "y"}); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("nil cipher err = %v, want ErrInvalidKey", err)
	}
}

// TestEncryptDecrypt_RoundTripsArbitrary makes sure the helper
// preserves bytes through the AEAD seal+open.
func TestEncryptDecrypt_RoundTripsArbitrary(t *testing.T) {
	_, c := newTestCipher(t)
	cases := [][]byte{
		[]byte("hello"),
		[]byte(""),
		[]byte("\x00\x01\x02"),
		[]byte("longer payload " + strings.Repeat("x", 256)),
	}
	for _, p := range cases {
		ct, err := encrypt(c, p)
		if err != nil {
			t.Fatalf("encrypt(%q): %v", p, err)
		}
		got, err := decrypt(c, ct)
		if err != nil {
			t.Fatalf("decrypt(%q): %v", p, err)
		}
		if string(got) != string(p) {
			t.Errorf("round trip got %q, want %q", got, p)
		}
	}
}

// TestDecrypt_RejectsShortPayload exercises the bounds check.
func TestDecrypt_RejectsShortPayload(t *testing.T) {
	_, c := newTestCipher(t)
	if _, err := decrypt(c, []byte("short")); !errors.Is(err, ErrInvalidCiphertext) {
		t.Errorf("err = %v, want ErrInvalidCiphertext", err)
	}
}

// TestVerifyRecoveryCode_FindsMatchAndIgnoresOthers
//
// A recovery code's hashed form is matched against the persisted
// list; we expect index returned, normalisation (uppercase) and a
// negative for misses.
func TestVerifyRecoveryCode_FindsMatchAndIgnoresOthers(t *testing.T) {
	_, c := newTestCipher(t)
	enr, err := Enrol(c, EnrolmentParams{Issuer: "FundAI", AccountName: "u"})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}
	if len(enr.RecoveryCodes) == 0 {
		t.Fatalf("no recovery codes generated")
	}
	target := enr.RecoveryCodes[3]
	idx := VerifyRecoveryCode(target, enr.HashedRecoveryCodes)
	if idx != 3 {
		t.Errorf("index = %d, want 3", idx)
	}
	// Lowercase / whitespace must be normalised so the user can
	// paste with surrounding spaces.
	idx = VerifyRecoveryCode("  "+strings.ToLower(target)+"  ", enr.HashedRecoveryCodes)
	if idx != 3 {
		t.Errorf("normalised index = %d, want 3", idx)
	}
	if VerifyRecoveryCode("XXXX-YYYY", enr.HashedRecoveryCodes) != -1 {
		t.Errorf("unmatched code returned a hit")
	}
	if VerifyRecoveryCode("", enr.HashedRecoveryCodes) != -1 {
		t.Errorf("empty code returned a hit")
	}
}
