package main

import (
	"testing"
	"time"

	"github.com/fundai/server/internal/secrets"
)

// TestIssueAndVerifyWithRotatingKeyring proves the end-to-end F29 flow:
//  1. Token signed with the active key (k1) verifies cleanly.
//  2. After rotation (new ring with k2 active, k1 retained for verify
//     only), tokens issued under k1 still verify because k1 is in the
//     ring's verification set.
//  3. Once k1 is removed from the ring, tokens signed under k1 fail.
func TestIssueAndVerifyWithRotatingKeyring(t *testing.T) {
	ringV1, err := secrets.NewJWTKeyring([]secrets.JWTKey{
		{Kid: "k1", Secret: "thirty-two-character-secret-aaaa", Active: true},
	})
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	token, _, err := issueSessionTokenWithKid("user-1", ringV1.Active().Secret, ringV1.Active().Kid, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Phase A: original ring verifies.
	claims, err := validateJWTWithKeyring(token, ringV1)
	if err != nil || claims.Subject != "user-1" {
		t.Fatalf("v1 verify: %v claims=%+v", err, claims)
	}

	// Phase B: rotated ring keeps k1 around for verification only.
	ringV2, err := secrets.NewJWTKeyring([]secrets.JWTKey{
		{Kid: "k2", Secret: "thirty-two-character-secret-bbbb", Active: true},
		{Kid: "k1", Secret: "thirty-two-character-secret-aaaa"},
	})
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	claims, err = validateJWTWithKeyring(token, ringV2)
	if err != nil || claims.Subject != "user-1" {
		t.Fatalf("v2 verify: %v claims=%+v", err, claims)
	}

	// Phase C: k1 has been retired; previously-issued tokens MUST fail.
	ringV3, err := secrets.NewJWTKeyring([]secrets.JWTKey{
		{Kid: "k2", Secret: "thirty-two-character-secret-bbbb", Active: true},
	})
	if err != nil {
		t.Fatalf("v3: %v", err)
	}
	if _, err := validateJWTWithKeyring(token, ringV3); err == nil {
		t.Fatal("expected unknown-kid failure after retirement")
	}
}

// TestValidateJWTWithKeyringLegacyTokenNoKid covers the migration path:
// tokens issued before F29 have no kid header. They MUST still verify
// against any key in the ring so a deployment that turns on rotation
// doesn't kick every logged-in user.
func TestValidateJWTWithKeyringLegacyTokenNoKid(t *testing.T) {
	legacySecret := "legacy-thirty-two-character-aaaaa"
	token, _, err := issueSessionToken("user-1", legacySecret, time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	ring, err := secrets.NewJWTKeyring([]secrets.JWTKey{
		{Kid: "k_new", Secret: "thirty-two-character-secret-bbbb", Active: true},
		{Kid: "k_old", Secret: legacySecret},
	})
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	claims, err := validateJWTWithKeyring(token, ring)
	if err != nil || claims.Subject != "user-1" {
		t.Fatalf("legacy verify: %v claims=%+v", err, claims)
	}
}

// TestValidateJWTWithKeyringRejectsUnknownKid is the security-critical
// case: a token presenting a kid that isn't in the ring must NOT fall
// back to the active key. Otherwise an attacker who replays a stolen
// k1-signed token after rotation could trick the server into using k2.
func TestValidateJWTWithKeyringRejectsUnknownKid(t *testing.T) {
	token, _, err := issueSessionTokenWithKid("user-1", "thirty-two-character-secret-aaaa", "ghost", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	ring, err := secrets.NewJWTKeyring([]secrets.JWTKey{
		{Kid: "k_actual", Secret: "thirty-two-character-secret-aaaa", Active: true},
	})
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	if _, err := validateJWTWithKeyring(token, ring); err == nil {
		t.Fatal("expected unknown-kid failure")
	}
}

// TestValidateJWTWithKeyringNilRingFails proves the misconfiguration
// guard. A nil keyring would otherwise let every token through (or
// panic); we surface a clean error instead.
func TestValidateJWTWithKeyringNilRingFails(t *testing.T) {
	if _, err := validateJWTWithKeyring("any.token.here", nil); err == nil {
		t.Fatal("expected nil-keyring error")
	}
}
