package repository

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestFingerprint_Stable(t *testing.T) {
	// SHA-256("sk-abc")[0:4] is deterministic.
	a := Fingerprint("sk-abc")
	b := Fingerprint("sk-abc")
	if a == "" {
		t.Fatalf("fingerprint empty")
	}
	if a != b {
		t.Fatalf("fingerprint unstable: %s vs %s", a, b)
	}
	if len(a) != 8 {
		t.Fatalf("fingerprint should be 8 hex chars, got %d (%q)", len(a), a)
	}
	if Fingerprint("") != "" {
		t.Fatalf("empty plaintext should return empty fingerprint")
	}
	if Fingerprint("sk-abc") == Fingerprint("sk-abd") {
		t.Fatalf("different plaintexts should produce different fingerprints")
	}
}

func TestEncryptKey_RoundTrip(t *testing.T) {
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", "test-secret-32-chars-long-enough-for-aes")
	enc, err := EncryptKey("sk-roundtrip-12345")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if enc == "" {
		t.Fatalf("encrypted is empty")
	}
	if enc == "sk-roundtrip-12345" {
		t.Fatalf("encrypted equals plaintext, AES is not running")
	}
	// The PlainAPIKey roundtrip relies on the same env var.
	row := &PlatformLLMProviderRow{APIKeyEncrypted: enc}
	pt, err := row.PlainAPIKey()
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if pt != "sk-roundtrip-12345" {
		t.Fatalf("roundtrip mismatch: got %q", pt)
	}
}

func TestEncryptKey_RejectsEmpty(t *testing.T) {
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", "test-secret-32-chars-long-enough-for-aes")
	if _, err := EncryptKey(""); err == nil {
		t.Fatalf("expected error on empty plaintext")
	}
}

func TestEncryptKey_RequiresSecret(t *testing.T) {
	t.Setenv("MODEL_CONFIG_API_KEY_SECRET", "")
	t.Setenv("API_KEY_ENCRYPTION_SECRET", "")
	if _, err := EncryptKey("sk-x"); err == nil {
		t.Fatalf("expected error when secret is unset")
	}
}

func TestPlatformLLMProviderRepo_Count(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlatformLLMProviderRepo(db)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM platform_llm_providers`)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	n, err := repo.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 7 {
		t.Fatalf("got %d want 7", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPlatformLLMProviderRepo_Get_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlatformLLMProviderRepo(db)
	id := uuid.New()
	mock.ExpectQuery(regexp.QuoteMeta(`FROM platform_llm_providers WHERE id =`)).
		WithArgs(id).
		WillReturnError(sql.ErrNoRows)
	_, err = repo.Get(context.Background(), id)
	if !errors.Is(err, ErrPlatformLLMProviderNotFound) {
		t.Fatalf("expected ErrPlatformLLMProviderNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPlatformLLMProviderRepo_Upsert_ValidationRejects(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlatformLLMProviderRepo(db)
	cases := []struct {
		name string
		in   UpsertInput
		want string
	}{
		{"bad provider", UpsertInput{Provider: "nope", Label: "x", ModelName: "y", BaseURL: "z", APIKeyPlaintext: "k"}, "invalid provider"},
		{"missing label", UpsertInput{Provider: "openai", ModelName: "y", BaseURL: "z", APIKeyPlaintext: "k"}, "label required"},
		{"missing model_name", UpsertInput{Provider: "openai", Label: "x", BaseURL: "z", APIKeyPlaintext: "k"}, "model_name required"},
		{"missing base_url", UpsertInput{Provider: "openai", Label: "x", ModelName: "y", APIKeyPlaintext: "k"}, "base_url required"},
		{"bad tier", UpsertInput{Provider: "openai", Label: "x", ModelName: "y", BaseURL: "z", APIKeyPlaintext: "k", ModelTier: "ultra"}, "invalid model_tier"},
		{"empty key on create", UpsertInput{Provider: "openai", Label: "x", ModelName: "y", BaseURL: "z"}, "api_key_plaintext required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repo.Upsert(context.Background(), tc.in)
			if err == nil || !containsCI(err.Error(), tc.want) {
				t.Fatalf("want error %q, got %v", tc.want, err)
			}
		})
	}
}

func TestPlatformLLMProviderRepo_Delete_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlatformLLMProviderRepo(db)
	id := uuid.New()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM platform_llm_providers WHERE id =`)).
		WithArgs(id).
		WillReturnResult(sqlmock.NewResult(0, 0))
	err = repo.Delete(context.Background(), id)
	if !errors.Is(err, ErrPlatformLLMProviderNotFound) {
		t.Fatalf("expected ErrPlatformLLMProviderNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPlatformLLMProviderRepo_SetDefault_AtomicSwap(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlatformLLMProviderRepo(db)
	id := uuid.New()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SET is_platform_default = FALSE`)).
		WithArgs(uuid.NullUUID{}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`SET is_platform_default = TRUE`)).
		WithArgs(id, uuid.NullUUID{}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := repo.SetDefault(context.Background(), id, uuid.NullUUID{}); err != nil {
		t.Fatalf("setdefault: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPlatformLLMProviderRepo_SetDefault_ClearOnly(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := NewPlatformLLMProviderRepo(db)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`SET is_platform_default = FALSE`)).
		WithArgs(uuid.NullUUID{}).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := repo.SetDefault(context.Background(), uuid.Nil, uuid.NullUUID{}); err != nil {
		t.Fatalf("setdefault clear: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func containsCI(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
