package promotion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
)

// Resolver returns the active promotion when one exists.
func TestResolverPicksActivePromotion(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	repo := repository.NewPromotionRepo(db)
	svc := &Service{Repo: repo, NewID: func() string { return "i" }, Now: func() time.Time { return now }}
	r := NewResolver(svc, nil)
	r.TTL = 0 // disable cache for deterministic call counts

	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(promotionCols()).
			AddRow("p-1", "fund-1", "u-1", "job-1", "llm",
				[]byte(`{"temperature":0.3}`), []byte(`{}`), "active", 0, 0.5,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, now, now))

	sel, err := r.Resolve(context.Background(), "fund-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.EngineKind != "llm" || sel.PromotionID != "p-1" || sel.Source != "promotion-active" {
		t.Errorf("unexpected selection: %+v", sel)
	}
	if v, ok := sel.EngineParams["temperature"]; !ok || v != 0.3 {
		t.Errorf("engineParams temperature missing or wrong: %+v", sel.EngineParams)
	}
}

// When no active promotion exists, the resolver delegates to the
// DefaultLookup closure.
func TestResolverFallsBackToDefault(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	repo := repository.NewPromotionRepo(db)
	svc := &Service{Repo: repo, NewID: func() string { return "i" }, Now: func() time.Time { return now }}

	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(promotionCols()))

	called := 0
	r := NewResolver(svc, func(_ context.Context, fundID string) (EngineSelection, error) {
		called++
		if fundID != "fund-1" {
			t.Errorf("default lookup got %s", fundID)
		}
		return EngineSelection{EngineKind: "fallback"}, nil
	})
	r.TTL = 0

	sel, err := r.Resolve(context.Background(), "fund-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.EngineKind != "fallback" {
		t.Errorf("got %s, want fallback", sel.EngineKind)
	}
	if sel.Source != "default" {
		t.Errorf("source = %s, want default", sel.Source)
	}
	if called != 1 {
		t.Errorf("default lookup called %d times, want 1", called)
	}
}

// Service errors (DB down) shouldn't block decisions — the
// resolver falls back to the default and logs the failure.
func TestResolverDegradesToDefaultOnServiceError(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	repo := repository.NewPromotionRepo(db)
	svc := &Service{Repo: repo, NewID: func() string { return "i" }, Now: func() time.Time { return now }}

	// Returning an error from the query simulates DB down.
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("fund-1").
		WillReturnError(errors.New("connection refused"))

	r := NewResolver(svc, func(context.Context, string) (EngineSelection, error) {
		return EngineSelection{EngineKind: "fallback"}, nil
	})
	r.TTL = 0
	sel, err := r.Resolve(context.Background(), "fund-1")
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if sel.EngineKind != "fallback" {
		t.Errorf("got %s, want fallback (DB down)", sel.EngineKind)
	}
}

// Cache: second Resolve within TTL doesn't hit the DB.
func TestResolverCachesWithinTTL(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	repo := repository.NewPromotionRepo(db)
	svc := &Service{Repo: repo, NewID: func() string { return "i" }, Now: func() time.Time { return now }}

	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(promotionCols()).
			AddRow("p-1", "fund-1", "u-1", "job-1", "llm",
				[]byte(`{}`), []byte(`{}`), "active", 0, 0.5,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, now, now))

	r := NewResolver(svc, nil)
	r.TTL = 30 * time.Second

	if _, err := r.Resolve(context.Background(), "fund-1"); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// Second call should be served from cache — no further DB
	// expectation needed. If a DB call leaked, sqlmock would
	// fail at ExpectationsWereMet (no further expectations).
	sel, err := r.Resolve(context.Background(), "fund-1")
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if sel.Source[:6] != "cache:" {
		t.Errorf("source = %s, want cache:* prefix", sel.Source)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB hits: %v", err)
	}
}

// Invalidate drops the cache so the next Resolve refetches.
func TestResolverInvalidateForcesRefetch(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	repo := repository.NewPromotionRepo(db)
	svc := &Service{Repo: repo, NewID: func() string { return "i" }, Now: func() time.Time { return now }}

	cols := promotionCols()
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("p-1", "fund-1", "u-1", "job-1", "llm",
				[]byte(`{}`), []byte(`{}`), "active", 0, 0.5,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, now, now))
	mock.ExpectQuery(`SELECT id, fund_id`).WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("p-2", "fund-1", "u-1", "job-2", "fallback",
				[]byte(`{}`), []byte(`{}`), "active", 0, 0.5,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, now, now))

	r := NewResolver(svc, nil)
	first, _ := r.Resolve(context.Background(), "fund-1")
	r.Invalidate("fund-1")
	second, _ := r.Resolve(context.Background(), "fund-1")
	if first.PromotionID == second.PromotionID {
		t.Errorf("invalidate should force a fresh lookup; got same ID %s twice", first.PromotionID)
	}
}
