package userbyok

import "github.com/fundai/server/internal/subscription"

// encryptForTest exposes subscription.EncryptAPIKey to the test
// package without forcing every test to import subscription
// directly.
func encryptForTest(plaintext, secret string) (string, error) {
	return subscription.EncryptAPIKey(plaintext, secret)
}
