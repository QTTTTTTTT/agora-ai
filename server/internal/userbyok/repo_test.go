package userbyok

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func withSecret(t *testing.T, fn func()) {
	t.Helper()
	prev := getEnv
	defer func() { getEnv = prev }()
	getEnv = func(key string) string {
		switch key {
		case "MODEL_CONFIG_API_KEY_SECRET":
			return "test-secret-do-not-use-in-prod"
		}
		return ""
	}
	fn()
}

func TestIsSupportedProvider(t *testing.T) {
	if !IsSupportedProvider("openai") {
		t.Error("openai should be supported")
	}
	if IsSupportedProvider("bogus") {
		t.Error("bogus should not be supported")
	}
}

func TestFingerprintDeterministic(t *testing.T) {
	a := Fingerprint("sk-abc12345xyz")
	b := Fingerprint("sk-abc12345xyz")
	if a != b {
		t.Errorf("expected stable fingerprint, got %s vs %s", a, b)
	}
	if Fingerprint("") != "" {
		t.Error("empty plaintext should yield empty fingerprint")
	}
}

func TestPreview(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"sk-abc123xyz999", "sk-a...z999"},
		{"short", "*****"},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := Preview(tc.in); got != tc.want {
			t.Errorf("Preview(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCreate_RejectsUnsupportedProvider(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRepo(db)
	withSecret(t, func() {
		_, err := r.Create(context.Background(), CreateRequest{
			UserID: "u1", Provider: "bogus", PlaintextAPIKey: "sk-abc",
		})
		if !errors.Is(err, ErrUnsupportedProvider) {
			t.Errorf("expected ErrUnsupportedProvider, got %v", err)
		}
	})
}

func TestCreate_RejectsEmptyKey(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRepo(db)
	withSecret(t, func() {
		_, err := r.Create(context.Background(), CreateRequest{
			UserID: "u1", Provider: "openai", PlaintextAPIKey: "   ",
		})
		if !errors.Is(err, ErrEmptyAPIKey) {
			t.Errorf("expected ErrEmptyAPIKey, got %v", err)
		}
	})
}

func TestCreate_RequiresEncryptionSecret(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRepo(db)
	prev := getEnv
	defer func() { getEnv = prev }()
	getEnv = func(string) string { return "" }
	_, err = r.Create(context.Background(), CreateRequest{
		UserID: "u1", Provider: "openai", PlaintextAPIKey: "sk-abc12345",
	})
	if !errors.Is(err, ErrEncryptionUnconfigured) {
		t.Errorf("expected ErrEncryptionUnconfigured, got %v", err)
	}
}

func TestCreate_FailsWhenActiveExists(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRepo(db)
	withSecret(t, func() {
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT EXISTS").WithArgs("u1", "openai").
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
		mock.ExpectRollback()

		_, err := r.Create(context.Background(), CreateRequest{
			UserID: "u1", Provider: "openai", PlaintextAPIKey: "sk-abc12345",
		})
		if !errors.Is(err, ErrAlreadyActive) {
			t.Errorf("expected ErrAlreadyActive, got %v", err)
		}
	})
}

func TestCreate_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRepo(db)
	withSecret(t, func() {
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT EXISTS").WithArgs("u1", "openai").
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO user_llm_keys")).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("key-1"))
		mock.ExpectCommit()

		k, err := r.Create(context.Background(), CreateRequest{
			UserID:                "u1",
			Provider:              "openai",
			PlaintextAPIKey:       "sk-abc12345xyz999",
			MonthlyBudgetCentsUSD: 1000,
		})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if k.APIKeyPreview != "sk-a...z999" {
			t.Errorf("bad preview: %q", k.APIKeyPreview)
		}
		if !strings.HasPrefix(k.APIKeyFingerprint, "") || len(k.APIKeyFingerprint) != 64 {
			t.Errorf("bad fingerprint: %q", k.APIKeyFingerprint)
		}
	})
}

func TestGetActiveForRouting_DecryptsKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRepo(db)
	withSecret(t, func() {
		secret, _ := getEncryptionSecret()
		encrypted, err := encryptForTest("sk-real-key-12345", secret)
		if err != nil {
			t.Fatal(err)
		}
		mock.ExpectQuery(regexp.QuoteMeta("FROM user_llm_keys")).
			WithArgs("u1", "openai").
			WillReturnRows(sqlmock.NewRows([]string{
				"id", "api_key_encrypted", "base_url", "model_name", "monthly_budget_cents_usd",
			}).AddRow("key-1", encrypted, "", "", 0))

		key, err := r.GetActiveForRouting(context.Background(), "u1", "openai")
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if key.PlaintextAPIKey != "sk-real-key-12345" {
			t.Errorf("bad plaintext: %q", key.PlaintextAPIKey)
		}
	})
}

func TestGetActiveForRouting_ReturnsNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRepo(db)
	withSecret(t, func() {
		mock.ExpectQuery(regexp.QuoteMeta("FROM user_llm_keys")).
			WithArgs("u1", "openai").
			WillReturnRows(sqlmock.NewRows([]string{
				"id", "api_key_encrypted", "base_url", "model_name", "monthly_budget_cents_usd",
			}))

		_, err := r.GetActiveForRouting(context.Background(), "u1", "openai")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})
}

func TestDelete_RevokesRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRepo(db)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE user_llm_keys")).
		WithArgs("k1", "u1", "user_requested").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.Delete(context.Background(), "u1", "k1", "user_requested"); err != nil {
		t.Errorf("unexpected: %v", err)
	}
}

func TestUpdateBudget_ClampsExtremes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewRepo(db)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE user_llm_keys")).
		WithArgs("k1", "u1", 1_000_000).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.UpdateBudget(context.Background(), "u1", "k1", 999_999_999); err != nil {
		t.Errorf("unexpected: %v", err)
	}
}
