package papertrading

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
)

// -------- canonicalisation determinism --------------------------------------

func TestCanonicaliseProducesSortedKeys(t *testing.T) {
	in := ProposeOrderInput{
		PortfolioID: "pf-1",
		Symbol:      "AAPL",
		Action:      "BUY",
		DecidedAt:   time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC),
	}
	b, err := canonicalise(in)
	if err != nil {
		t.Fatalf("canonicalise: %v", err)
	}
	s := string(b)
	// Keys should appear in alphabetical order.
	expectKeys := []string{`"action":`, `"decidedAt":`, `"portfolioId":`, `"symbol":`}
	last := -1
	for _, k := range expectKeys {
		idx := strings.Index(s, k)
		if idx <= last {
			t.Errorf("key %q out of order or missing in %q", k, s)
		}
		last = idx
	}
}

func TestCanonicaliseIsDeterministic(t *testing.T) {
	tw := 0.07
	in := ProposeOrderInput{
		PortfolioID:  "pf-1",
		Symbol:       "AAPL",
		Action:       "BUY",
		TargetWeight: &tw,
		AIReasoning:  map[string]any{"buffett": "BUY", "lynch": "HOLD"},
		DecidedAt:    time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC),
	}
	a, _ := canonicalise(in)
	b, _ := canonicalise(in)
	if string(a) != string(b) {
		t.Errorf("canonicalise not deterministic: %q vs %q", a, b)
	}
}

func TestCanonicaliseOmitsZeroOptionals(t *testing.T) {
	in := ProposeOrderInput{
		PortfolioID: "pf-1",
		Symbol:      "AAPL",
		Action:      "BUY",
		DecidedAt:   time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC),
	}
	b, _ := canonicalise(in)
	if strings.Contains(string(b), "targetWeight") {
		t.Errorf("targetWeight should be omitted when nil, got %q", b)
	}
}

func TestSha256HexStable(t *testing.T) {
	if got := sha256Hex([]byte("hello")); got != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("sha256 mismatch: %q", got)
	}
}

func TestValidActionMatrix(t *testing.T) {
	for _, a := range []string{"BUY", "SELL", "REBALANCE"} {
		if !validAction(a) {
			t.Errorf("%q should be valid", a)
		}
	}
	for _, a := range []string{"buy", "SHORT", "", "FOO"} {
		if validAction(a) {
			t.Errorf("%q should be invalid", a)
		}
	}
}

// -------- service integration via sqlmock -----------------------------------

func newService(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := repository.NewPaperTradingRepo(db)
	svc := New(repo, StubOTSClient{}, func() time.Time { return time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC) })
	return svc, mock, func() { db.Close() }
}

func TestServiceCreatePortfolioPropagates(t *testing.T) {
	svc, mock, cleanup := newService(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO paper_portfolios`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("pf-1", time.Now()))

	out, err := svc.CreatePortfolio(context.Background(), CreatePortfolioInput{
		Name: "Test", Strategy: "x", Market: "us_equity", InitialCapital: 100_000,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out.ID != "pf-1" {
		t.Errorf("id = %q", out.ID)
	}
}

func TestServiceProposeOrderInsertsAndStamps(t *testing.T) {
	svc, mock, cleanup := newService(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO paper_orders`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "decided_at"}).
			AddRow("order-1", time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)))
	// StubOTSClient returns ("submitted", "") which triggers UpdateOrderProof.
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE paper_orders`)).
		WithArgs("order-1", "", "submitted").
		WillReturnResult(sqlmock.NewResult(0, 1))

	tw := 0.07
	out, err := svc.ProposeOrder(context.Background(), ProposeOrderInput{
		PortfolioID:  "pf-1",
		Symbol:       "AAPL",
		Action:       "BUY",
		TargetWeight: &tw,
		AIReasoning:  map[string]any{"confidence": 0.85},
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if out.HashSignature == "" || len(out.HashSignature) != 64 {
		t.Errorf("expected 64-char hex hash, got %q", out.HashSignature)
	}
	if out.OTSStatus != "submitted" {
		t.Errorf("ots status = %q, want submitted", out.OTSStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestServiceProposeOrderRejectsInvalidAction(t *testing.T) {
	svc, _, cleanup := newService(t)
	defer cleanup()

	_, err := svc.ProposeOrder(context.Background(), ProposeOrderInput{
		PortfolioID: "pf-1", Symbol: "AAPL", Action: "SHORT_SELL",
	})
	if err == nil {
		t.Fatalf("expected error for SHORT_SELL action")
	}
}

func TestServiceNilSafe(t *testing.T) {
	var svc *Service
	_, err := svc.CreatePortfolio(context.Background(), CreatePortfolioInput{})
	if !errors.Is(err, ErrServiceUnconfigured) {
		t.Errorf("expected ErrServiceUnconfigured, got %v", err)
	}
	_, err = svc.ListPortfolios(context.Background())
	if !errors.Is(err, ErrServiceUnconfigured) {
		t.Errorf("expected ErrServiceUnconfigured on ListPortfolios, got %v", err)
	}
	_, err = svc.ProposeOrder(context.Background(), ProposeOrderInput{})
	if !errors.Is(err, ErrServiceUnconfigured) {
		t.Errorf("expected ErrServiceUnconfigured on ProposeOrder, got %v", err)
	}
}

func TestServiceSnapshotNAVChainsCorrectly(t *testing.T) {
	svc, mock, cleanup := newService(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO paper_nav_history`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO paper_holdings_snapshots`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE paper_portfolios`)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	dr := 0.02
	err := svc.SnapshotNAV(context.Background(), SnapshotNAVInput{
		PortfolioID:  "pf-1",
		SnapshotDate: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Nav:          102_000,
		DailyReturn:  &dr,
		CashBalance:  20_000,
		Holdings: map[string]HoldingPosition{
			"AAPL": {Shares: 100, MarketValue: 50_000, Weight: 0.49},
			"MSFT": {Shares: 200, MarketValue: 32_000, Weight: 0.31},
		},
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCanonicalPayloadIsValidJSON(t *testing.T) {
	in := ProposeOrderInput{
		PortfolioID: "pf-1", Symbol: "AAPL", Action: "BUY",
		DecidedAt: time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC),
	}
	b, _ := canonicalise(in)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("canonical payload should be valid JSON: %v\n%s", err, b)
	}
	if m["symbol"] != "AAPL" {
		t.Errorf("symbol round-trip failed: got %v", m["symbol"])
	}
}

func TestShouldRebalanceMonthlyUsesFirstAvailableTradingSlot(t *testing.T) {
	seen := map[string]struct{}{}
	days := []time.Time{
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),  // first available January trading day
		time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC),  // same month, skip
		time.Date(2026, 1, 16, 0, 0, 0, 0, time.UTC), // same month, skip
		time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC),  // first available February trading day
	}
	want := []bool{true, false, false, true}
	for i, day := range days {
		if got := shouldRebalanceMonthly(day, seen); got != want[i] {
			t.Fatalf("day %s rebalance=%v, want %v", day.Format("2006-01-02"), got, want[i])
		}
	}
}

func TestSimulateEqualWeightMonthlyUsesWholeShares(t *testing.T) {
	days := []time.Time{
		time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 1, 16, 0, 0, 0, 0, time.UTC),
	}
	closeBy := map[time.Time]map[string]float64{
		days[0]: {"AAA": 300, "BBB": 200},
		days[1]: {"AAA": 310, "BBB": 210},
	}
	_, operations := simulateEqualWeightMonthly(1000, []string{"AAA", "BBB"}, days, closeBy)
	if len(operations) == 0 {
		t.Fatalf("expected operations")
	}
	for _, op := range operations {
		if op.SharesAfter != math.Trunc(op.SharesAfter) {
			t.Fatalf("sharesAfter should be whole-share, got %v for %s", op.SharesAfter, op.Symbol)
		}
		if op.SharesChange != math.Trunc(op.SharesChange) {
			t.Fatalf("sharesChange should be whole-share, got %v for %s", op.SharesChange, op.Symbol)
		}
	}
}
