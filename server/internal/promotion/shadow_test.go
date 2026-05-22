package promotion

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
)

// DefaultAgreement: same action+symbol+similar qty → agree.
// Different action or symbol → disagree. Two holds → agree
// regardless of symbol.
func TestDefaultAgreementRules(t *testing.T) {
	cases := []struct {
		name           string
		a, b           DecisionSnapshot
		wantAgreement bool
	}{
		{"matching buy", DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 100}, DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 105}, true},
		{"matching sell", DecisionSnapshot{Action: "sell", Symbol: "MSFT", Quantity: 50}, DecisionSnapshot{Action: "sell", Symbol: "MSFT", Quantity: 50}, true},
		{"different action", DecisionSnapshot{Action: "buy", Symbol: "AAPL"}, DecisionSnapshot{Action: "sell", Symbol: "AAPL"}, false},
		{"different symbol", DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 100}, DecisionSnapshot{Action: "buy", Symbol: "MSFT", Quantity: 100}, false},
		{"qty diff >10%", DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 100}, DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 200}, false},
		{"qty diff <=10%", DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 100}, DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 109}, true},
		{"both holds, diff symbols", DecisionSnapshot{Action: "hold", Symbol: "AAPL"}, DecisionSnapshot{Action: "hold", Symbol: "MSFT"}, true},
		{"case insensitive", DecisionSnapshot{Action: "BUY", Symbol: "aapl", Quantity: 100}, DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 100}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultAgreement(tc.a, tc.b); got != tc.wantAgreement {
				t.Errorf("got %v, want %v", got, tc.wantAgreement)
			}
		})
	}
}

// Record: writes a row with agreement derived from the configured
// AgreementFn. Idempotent via the underlying UPSERT.
func TestShadowComparatorRecord(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	repo := repository.NewPromotionRepo(db)
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	c := &ShadowComparator{
		Repo:  repo,
		NewID: func() string { return "diff-1" },
		Now:   func() time.Time { return now },
	}

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_shadow_diffs`)).
		WithArgs("diff-1", "p-1", now.Truncate(24*time.Hour), sqlmock.AnyArg(), sqlmock.AnyArg(), true, now).
		WillReturnResult(sqlmock.NewResult(0, 1))

	out, err := c.Record(context.Background(), "p-1", now,
		DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 100, EngineKind: "llm"},
		DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 100, EngineKind: "fallback"},
	)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !out.Agreement {
		t.Errorf("expected agreement true")
	}
}

// AgreementRatio: trivial case — 3 rows, 2 agree → 2/3.
func TestShadowComparatorAgreementRatio(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	repo := repository.NewPromotionRepo(db)
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	c := &ShadowComparator{
		Repo:  repo,
		NewID: func() string { return "i" },
		Now:   func() time.Time { return now },
	}

	mock.ExpectQuery(`SELECT id, promotion_id, trading_date`).
		WithArgs("p-1", 30).
		WillReturnRows(sqlmock.NewRows([]string{"id", "promotion_id", "trading_date", "shadow_decision", "active_decision", "agreement", "created_at"}).
			AddRow("d1", "p-1", now, []byte(`{}`), []byte(`{}`), true, now).
			AddRow("d2", "p-1", now.AddDate(0, 0, -1), []byte(`{}`), []byte(`{}`), false, now).
			AddRow("d3", "p-1", now.AddDate(0, 0, -2), []byte(`{}`), []byte(`{}`), true, now))

	ratio, n, err := c.AgreementRatio(context.Background(), "p-1", 30)
	if err != nil {
		t.Fatalf("AgreementRatio: %v", err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
	want := 2.0 / 3.0
	if ratio < want-1e-9 || ratio > want+1e-9 {
		t.Errorf("ratio = %f, want %f", ratio, want)
	}
}

// AgreementRatio: zero rows → (0, 0, nil) — no division by zero.
func TestShadowComparatorAgreementRatioEmpty(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	repo := repository.NewPromotionRepo(db)
	c := &ShadowComparator{
		Repo:  repo,
		NewID: func() string { return "i" },
		Now:   func() time.Time { return time.Now() },
	}
	mock.ExpectQuery(`SELECT id, promotion_id, trading_date`).
		WithArgs("p-1", 50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "promotion_id", "trading_date", "shadow_decision", "active_decision", "agreement", "created_at"}))
	ratio, n, err := c.AgreementRatio(context.Background(), "p-1", 0)
	if err != nil {
		t.Fatalf("AgreementRatio: %v", err)
	}
	if ratio != 0 || n != 0 {
		t.Errorf("got %f / %d, want 0/0", ratio, n)
	}
}

// Custom AgreementFn: operators can plug in a stricter rule.
func TestShadowComparatorCustomAgreement(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	repo := repository.NewPromotionRepo(db)
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	c := &ShadowComparator{
		Repo:  repo,
		NewID: func() string { return "x" },
		Now:   func() time.Time { return now },
		// Strict: require exact equality. Same inputs that
		// DefaultAgreement would accept now disagree because
		// confidence differs.
		Agree: func(a, b DecisionSnapshot) bool {
			return a == b
		},
	}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO promotion_shadow_diffs`)).
		WithArgs(sqlmock.AnyArg(), "p-1", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), false, now).
		WillReturnResult(sqlmock.NewResult(0, 1))

	out, _ := c.Record(context.Background(), "p-1", now,
		DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 100, Confidence: 0.7},
		DecisionSnapshot{Action: "buy", Symbol: "AAPL", Quantity: 100, Confidence: 0.5},
	)
	if out.Agreement {
		t.Errorf("strict comparator should NOT agree on different confidence")
	}
}
