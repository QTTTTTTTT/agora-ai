package subscription

import (
	"strings"
	"testing"
)

func TestGetAPIKeyEncryptionSecretPrefersDedicatedSecret(t *testing.T) {
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", " model-secret ")
	t.Setenv("API_KEY_ENCRYPTION_SECRET", "legacy-secret")
	t.Setenv("JWT_SECRET", "jwt-secret-should-not-be-used")

	secret, err := getAPIKeyEncryptionSecret()
	if err != nil {
		t.Fatalf("expected secret, got %v", err)
	}
	if secret != "model-secret" {
		t.Fatalf("expected model secret, got %q", secret)
	}
}

func TestGetAPIKeyEncryptionSecretFallsBackToLegacyAlias(t *testing.T) {
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", "")
	t.Setenv("API_KEY_ENCRYPTION_SECRET", " legacy-secret ")
	t.Setenv("JWT_SECRET", "jwt-secret-should-not-be-used")

	secret, err := getAPIKeyEncryptionSecret()
	if err != nil {
		t.Fatalf("expected secret, got %v", err)
	}
	if secret != "legacy-secret" {
		t.Fatalf("expected legacy alias secret, got %q", secret)
	}
}

func TestGetAPIKeyEncryptionSecretRejectsJWTSecretFallback(t *testing.T) {
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", "")
	t.Setenv("API_KEY_ENCRYPTION_SECRET", "")
	t.Setenv("JWT_SECRET", strings.Repeat("j", 32))

	_, err := getAPIKeyEncryptionSecret()
	if err == nil || !strings.Contains(err.Error(), "MODEL_CONFIG_API_KEY_SECRET") {
		t.Fatalf("expected missing dedicated secret error, got %v", err)
	}
}

func TestEncryptAndDecryptAPIKeyUseDedicatedSecret(t *testing.T) {
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", strings.Repeat("m", 32))
	t.Setenv("API_KEY_ENCRYPTION_SECRET", "")
	t.Setenv("JWT_SECRET", strings.Repeat("j", 32))

	encrypted, err := encryptAPIKeyForStorage("sk-test")
	if err != nil {
		t.Fatalf("encrypt api key: %v", err)
	}
	decrypted, err := decryptAPIKeyFromStorage(encrypted)
	if err != nil {
		t.Fatalf("decrypt api key: %v", err)
	}
	if decrypted != "sk-test" {
		t.Fatalf("expected decrypted api key, got %q", decrypted)
	}
}

func TestResolveProviderBaseURLPrefersEnvAlias(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://anthropic.example/v1")
	baseURL, err := resolveProviderBaseURL("claude", nil)
	if err != nil {
		t.Fatalf("resolve provider base url: %v", err)
	}
	if baseURL != "https://anthropic.example/v1" {
		t.Fatalf("expected anthropic env base url, got %q", baseURL)
	}
}

func TestResolveProviderBaseURLSupportsGeminiAlias(t *testing.T) {
	t.Setenv("GEMINI_BASE_URL", "https://gemini.example/v1beta")
	baseURL, err := resolveProviderBaseURL("gemini", nil)
	if err != nil {
		t.Fatalf("resolve provider base url: %v", err)
	}
	if baseURL != "https://gemini.example/v1beta" {
		t.Fatalf("expected gemini env base url, got %q", baseURL)
	}
}

func TestResolveProviderProtocolDetectsGemini(t *testing.T) {
	if protocol := resolveProviderProtocol("gemini", "https://generativelanguage.googleapis.com/v1beta"); protocol != providerProtocolGemini {
		t.Fatalf("expected gemini protocol, got %q", protocol)
	}
}

func TestBuildConnectionTestRequestUsesGeminiGenerateContent(t *testing.T) {
	requestURL, body, headers, err := buildConnectionTestRequest("gemini", "https://generativelanguage.googleapis.com", "gemini-3.1-pro-preview", "gem-key")
	if err != nil {
		t.Fatalf("build connection test request: %v", err)
	}
	if requestURL != "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.1-pro-preview:generateContent" {
		t.Fatalf("unexpected gemini request url: %q", requestURL)
	}
	if headers["Authorization"] != "Bearer gem-key" {
		t.Fatalf("expected gemini authorization header, got %#v", headers)
	}
	if !strings.Contains(string(body), "maxOutputTokens") {
		t.Fatalf("expected gemini request body, got %s", string(body))
	}
}

func TestBuildGeminiGenerateContentURLAddsV1BetaWhenMissing(t *testing.T) {
	requestURL := buildGeminiGenerateContentURL("http://relay.example:8090", "gemini-3.1-pro-preview")
	if requestURL != "http://relay.example:8090/v1beta/models/gemini-3.1-pro-preview:generateContent" {
		t.Fatalf("unexpected gemini url with injected v1beta: %q", requestURL)
	}
}
