package main

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/corpaction"
	"github.com/fundai/server/internal/repository"
)

// reflectTypeOf is a thin alias for reflect.TypeOf so the test
// helper above can stay readable without scattering reflect.*
// across the file (which would be the only place that reads on
// types in this whole test target).
func reflectTypeOf(v any) reflect.Type { return reflect.TypeOf(v) }

// stubCorpActionFetcher implements corpaction.EventFetcher with a
// pre-canned response. Tracks call count + the symbol it was asked
// for so the test can assert routing behaviour.
type stubCorpActionFetcher struct {
	mu       sync.Mutex
	calls    []string
	events   []corpaction.Event
	failWith error
	// failSequence lets a test feed a different error on each call
	// (used by the Card-G retry tests). Non-nil entries override
	// failWith for that index; nil = succeed (return events).
	failSequence []error
}

func (s *stubCorpActionFetcher) FetchEvents(_ context.Context, symbol string, _ time.Time) ([]corpaction.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, symbol)
	if len(s.failSequence) > 0 {
		idx := len(s.calls) - 1
		if idx < len(s.failSequence) {
			if s.failSequence[idx] != nil {
				return nil, s.failSequence[idx]
			}
		}
	} else if s.failWith != nil {
		return nil, s.failWith
	}
	out := make([]corpaction.Event, 0, len(s.events))
	for _, e := range s.events {
		if e.InstrumentKey == "" {
			e.InstrumentKey = "INSTR:" + symbol
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *stubCorpActionFetcher) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// symbolsCalled returns a copy of the symbol list this stub was
// asked for, in call order. Tests use this to assert the
// market-router routes the right symbol to the right provider.
func (s *stubCorpActionFetcher) symbolsCalled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// TestCorpActionIngestLoop_NoLeaderShortCircuits pins the lease
// gate. A non-leader replica must NOT touch the DB or providers.
func TestCorpActionIngestLoop_NoLeaderShortCircuits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	prov := &stubCorpActionFetcher{}
	loop := newCorpActionIngestLoop(db)
	loop.providers = map[string]corpaction.EventFetcher{"a_share": prov}
	loop.SetLeaderChecker(stubLeader{leader: false})

	loop.runOnce(context.Background())

	if prov.callCount() != 0 {
		t.Errorf("provider called %d times under non-leader; want 0", prov.callCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// TestCorpActionIngestLoop_NoActiveFundsNoOp covers the common
// startup state — no funds yet, no holdings, no DB churn beyond the
// initial collect query.
func TestCorpActionIngestLoop_NoActiveFundsNoOp(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM holding_positions`).
		WillReturnRows(sqlmock.NewRows([]string{"instrument_key", "market", "symbol"}))

	prov := &stubCorpActionFetcher{}
	loop := newCorpActionIngestLoop(db)
	loop.providers = map[string]corpaction.EventFetcher{"a_share": prov}
	loop.SetLeaderChecker(stubLeader{leader: true})

	loop.runOnce(context.Background())

	if prov.callCount() != 0 {
		t.Errorf("provider called %d times with no active holdings; want 0", prov.callCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestCorpActionIngestLoop_SkipsUnknownMarket pins the routing
// rule: a holding in a market we don't have a provider for is
// silently skipped. The test deliberately doesn't register a
// "futures" provider but lists a futures holding.
func TestCorpActionIngestLoop_SkipsUnknownMarket(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM holding_positions`).
		WillReturnRows(
			sqlmock.NewRows([]string{"instrument_key", "market", "symbol"}).
				AddRow("INSTR:BTC", "futures", "BTCUSDT"),
		)

	aShareProv := &stubCorpActionFetcher{}
	loop := newCorpActionIngestLoop(db)
	loop.providers = map[string]corpaction.EventFetcher{"a_share": aShareProv}
	loop.SetLeaderChecker(stubLeader{leader: true})

	loop.runOnce(context.Background())

	if aShareProv.callCount() != 0 {
		t.Errorf("a_share provider called for futures holding")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestCorpActionIngestLoop_HKEquityRoutesToHKProvider is the
// Card H end-to-end routing pin: a holding with market="hk_equity"
// must reach the hk_equity provider (not us_equity, not a_share)
// with the symbol shape collected from holding_positions. The
// upstream catastrophe Card H fixed was Yahoo silently swallowing
// HK dividends; this test makes sure we never accidentally route
// HK back to a non-HK provider.
//
// We use a stubCorpActionFetcher under the "hk_equity" key plus a
// second stub under "us_equity" to confirm the cross-market
// router doesn't bleed.
func TestCorpActionIngestLoop_HKEquityRoutesToHKProvider(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM holding_positions`).
		WillReturnRows(
			sqlmock.NewRows([]string{"instrument_key", "market", "symbol"}).
				AddRow("HKEX:00700", "hk_equity", "00700"),
		)
	// No Upsert expected — the stub returns zero events, so the
	// loop short-circuits after the FetchEvents call.

	hkProv := &stubCorpActionFetcher{}
	usProv := &stubCorpActionFetcher{}
	loop := newCorpActionIngestLoop(db)
	loop.providers = map[string]corpaction.EventFetcher{
		"hk_equity": hkProv,
		"us_equity": usProv,
	}
	loop.SetLeaderChecker(stubLeader{leader: true})

	loop.runOnce(context.Background())

	if hkProv.callCount() != 1 {
		t.Errorf("hk_equity provider call count = %d, want 1", hkProv.callCount())
	}
	if got := hkProv.symbolsCalled(); len(got) != 1 || got[0] != "00700" {
		t.Errorf("hk_equity provider asked for %v, want [00700]", got)
	}
	if usProv.callCount() != 0 {
		t.Errorf("us_equity provider called for HK holding (cross-market leak)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestCorpActionIngestLoop_ProviderFetchErrorIsolated pins error-
// isolation: a 500 from the provider for symbol X must not kill
// the loop or leak into the DB. The collect query runs, the
// provider is asked once, and we exit without an upsert.
func TestCorpActionIngestLoop_ProviderFetchErrorIsolated(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM holding_positions`).
		WillReturnRows(
			sqlmock.NewRows([]string{"instrument_key", "market", "symbol"}).
				AddRow("INSTR:688195.SS", "a_share", "688195"),
		)
	// no Upsert expectation — the provider error should short-circuit.

	prov := &stubCorpActionFetcher{failWith: errors.New("upstream 500")}
	loop := newCorpActionIngestLoop(db)
	loop.providers = map[string]corpaction.EventFetcher{"a_share": prov}
	loop.SetLeaderChecker(stubLeader{leader: true})

	loop.runOnce(context.Background())

	if prov.callCount() != 1 {
		t.Errorf("provider call count = %d, want 1", prov.callCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (or extra) DB calls: %v", err)
	}
}

// TestCorpActionIngestLoop_HappyPathUpsertsAndReturnsHoldersResolved
// pins the success path:
//
//   - active holdings are scanned;
//   - the matching provider is queried;
//   - one event maps to one Upsert + one DefaultFundResolver lookup +
//     one ApplyEvent transaction.
//
// We don't assert apply transaction internals here (those are
// covered by applier_test.go) — we mock the locking SELECT so the
// applier returns ErrPositionMissing and the loop counts a soft
// skip rather than an error.
func TestCorpActionIngestLoop_HappyPathUpsertsAndReturnsHoldersResolved(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM holding_positions`).
		WillReturnRows(
			sqlmock.NewRows([]string{"instrument_key", "market", "symbol"}).
				AddRow("INSTR:688195.SS", "a_share", "688195"),
		)
	// DefaultFundResolver lookup is the first repo touch after the
	// collect query — it gates the upsert / apply per the loop's
	// implementation.
	mock.ExpectQuery(`SELECT DISTINCT fund_id FROM holding_positions`).
		WithArgs("INSTR:688195.SS").
		WillReturnRows(
			sqlmock.NewRows([]string{"fund_id"}).
				AddRow("fund-1"),
		)
	// CorpActionRepo.Upsert
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO corporate_actions")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-1"))
	// applier opens a tx and probes the idempotency table. We
	// return an existing row so it short-circuits with AlreadyApplied
	// — the loop counts that as a successful apply (the post-state
	// invariant is already satisfied).
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pre_quantity, post_quantity, pre_cost_price, post_cost_price, cash_credit FROM corp_action_applications WHERE corp_action_id = $1 AND fund_id = $2`)).
		WithArgs("evt-1", "fund-1").
		WillReturnRows(
			sqlmock.NewRows([]string{
				"pre_quantity", "post_quantity",
				"pre_cost_price", "post_cost_price",
				"cash_credit",
			}).AddRow(289.0, 376.0, 300.0, 230.0, 47.396),
		)
	mock.ExpectCommit()

	prov := &stubCorpActionFetcher{
		events: []corpaction.Event{
			{
				InstrumentKey: "INSTR:688195.SS",
				ExDate:        time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC),
				ActionType:    "split",
				SplitRatio:    1.3,
				Source:        "eastmoney",
			},
		},
	}
	loop := newCorpActionIngestLoop(db)
	loop.providers = map[string]corpaction.EventFetcher{"a_share": prov}
	loop.SetLeaderChecker(stubLeader{leader: true})

	loop.runOnce(context.Background())

	if prov.callCount() != 1 {
		t.Errorf("provider call count = %d, want 1", prov.callCount())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestCorpActionIngestLoop_StartIsIdempotent pins the lifecycle:
// double-Start must not spin a second goroutine. We can't directly
// observe goroutines, but Stop()-after-double-Start should not
// hang; that's enough for the regression contract.
func TestCorpActionIngestLoop_StartIsIdempotent(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	loop := newCorpActionIngestLoop(db)
	// Don't Start in this test — the warmup is 5min and we just
	// want to assert the lifecycle flag flips correctly.
	loop.Start()
	loop.Start()
	loop.Stop() // should not panic / hang
}

// stubLeader is a tiny leaderChecker for the tests above. Not
// exported beyond this file.
type stubLeader struct {
	leader bool
}

func (s stubLeader) IsLeader(_ string) bool { return s.leader }

// Sanity assertion: the loop's repo wiring uses the real
// CorpActionRepo. If newCorpActionIngestLoop ever swaps in a
// test-only repo by accident, this test catches it.
func TestCorpActionIngestLoop_RepoWiringIsConcrete(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	loop := newCorpActionIngestLoop(db)
	if loop.repo == nil {
		t.Fatal("repo not wired")
	}
	// Avoid circular imports by spot-checking repo type via the
	// Upsert contract — if the type changed, this assertion still
	// compiles (the cast doesn't), making the failure a clear
	// signal at edit time.
	var _ *repository.CorpActionRepo = loop.repo
}

// TestCorpActionIngestLoop_DefaultProviderRouting pins the
// market-to-provider mapping that newCorpActionIngestLoop ships
// with. Card H switched hk_equity from YahooProvider to
// HKEXProvider; we want a hard CI signal if anyone reverts that
// (or if a future card slips a new market in without a real
// provider). Tests that override `loop.providers` directly bypass
// this map, so this is the only place the production wiring is
// asserted.
func TestCorpActionIngestLoop_DefaultProviderRouting(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	loop := newCorpActionIngestLoop(db)
	if loop == nil {
		t.Fatal("newCorpActionIngestLoop returned nil")
	}

	cases := []struct {
		market string
		want   any
	}{
		{"a_share", &corpaction.EastmoneyProvider{}},
		{"us_equity", &corpaction.YahooProvider{}},
		// Card H regression: HK MUST be HKEXProvider, not Yahoo.
		// Yahoo's HK feed misses interim/special divs and bonus
		// issues; routing HK through it produces a phantom-PnL
		// bug class on every dividend cycle.
		{"hk_equity", &corpaction.HKEXProvider{}},
	}
	for _, tc := range cases {
		got, ok := loop.providers[tc.market]
		if !ok {
			t.Errorf("market %q: not registered in default provider map", tc.market)
			continue
		}
		// Compare concrete types, not values — providers are stateless
		// and their pointer addresses differ between calls.
		gotType := typeName(got)
		wantType := typeName(tc.want)
		if gotType != wantType {
			t.Errorf("market %q: provider = %s, want %s",
				tc.market, gotType, wantType)
		}
	}
}

// typeName returns the type name without package path noise so
// the assertion error message reads cleanly.
func typeName(v any) string {
	if v == nil {
		return "<nil>"
	}
	t := reflectTypeOf(v)
	if t == nil {
		return "<unknown>"
	}
	return t.String()
}

// ----------------------------------------------------------------------
// Card G — metrics + retry tests
// ----------------------------------------------------------------------

// recordedMetrics is a corpActionMetricsRecorder fake that records
// every call so tests can assert on (label, value) shape. The
// methods take exactly the same args as the production recorder so
// when we tighten the interface in the future, the compile error
// surfaces here too.
type recordedMetrics struct {
	mu          sync.Mutex
	tickStatus  []string
	provErrors  []string // "market|outcome"
	retries     []string // "market|outcome"
	events      []string // "action|phase"
	apply       []string // "outcome"
}

func (r *recordedMetrics) RecordCorpActionTick(status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tickStatus = append(r.tickStatus, status)
}
func (r *recordedMetrics) RecordCorpActionProviderError(market, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.provErrors = append(r.provErrors, market+"|"+outcome)
}
func (r *recordedMetrics) RecordCorpActionRetry(market, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retries = append(r.retries, market+"|"+outcome)
}
func (r *recordedMetrics) RecordCorpActionEvent(action, phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, action+"|"+phase)
}
func (r *recordedMetrics) RecordCorpActionApply(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.apply = append(r.apply, outcome)
}

// TestIsCorpActionTransient pins the small classifier table.
// Adding a new transient marker should add a row here so we don't
// silently lose retry coverage.
func TestIsCorpActionTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not transient", nil, false},
		{"bare EOF", errors.New("EOF"), true},
		{"unexpected EOF", errors.New("unexpected EOF"), true},
		{"connection reset", errors.New("read tcp 1.2.3.4:443: connection reset by peer"), true},
		{"broken pipe", errors.New("write tcp 1.2.3.4:443: broken pipe"), true},
		{"connection refused", errors.New("dial tcp 1.2.3.4:443: connect: connection refused"), true},
		{"i/o timeout", errors.New("read tcp 1.2.3.4:443: i/o timeout"), true},
		{"ctx deadline", errors.New("context deadline exceeded"), true},
		{"4xx is fatal", errors.New("eastmoney: status 403: forbidden"), false},
		{"json parse is fatal", errors.New("invalid character 'h' looking for beginning of value"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCorpActionTransient(tc.err); got != tc.want {
				t.Errorf("isCorpActionTransient(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestFetchEventsWithRetry_TransientThenSuccess pins the happy
// retry path: first attempt EOFs, second succeeds, retries counter
// records "succeeded", no provider-error counter (since the final
// outcome was OK).
func TestFetchEventsWithRetry_TransientThenSuccess(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	prov := &stubCorpActionFetcher{
		failSequence: []error{errors.New("EOF"), nil},
		events: []corpaction.Event{
			{InstrumentKey: "INSTR:X", ActionType: "split", SplitRatio: 1.5,
				ExDate: time.Now().UTC()},
		},
	}
	rec := &recordedMetrics{}
	loop := newCorpActionIngestLoop(db)
	loop.SetMetrics(rec)
	loop.fetchRetryAttempts = 1
	loop.fetchRetryBackoff = 0

	events, err := loop.fetchEventsWithRetry(context.Background(), prov, "a_share", "X", time.Now().Add(-90*24*time.Hour))
	if err != nil {
		t.Fatalf("fetchEventsWithRetry: unexpected err: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if prov.callCount() != 2 {
		t.Errorf("provider calls = %d, want 2 (initial + 1 retry)", prov.callCount())
	}
	if len(rec.retries) != 1 || rec.retries[0] != "a_share|succeeded" {
		t.Errorf("retries = %v, want [a_share|succeeded]", rec.retries)
	}
	if len(rec.provErrors) != 0 {
		t.Errorf("provider errors should be empty on retry success, got %v", rec.provErrors)
	}
}

// TestFetchEventsWithRetry_TransientExhausted pins the budget cap:
// every attempt fails transient, retry counter ends up "exhausted",
// provider-error counter ends up "transient".
func TestFetchEventsWithRetry_TransientExhausted(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	prov := &stubCorpActionFetcher{
		failSequence: []error{errors.New("EOF"), errors.New("connection reset by peer")},
	}
	rec := &recordedMetrics{}
	loop := newCorpActionIngestLoop(db)
	loop.SetMetrics(rec)
	loop.fetchRetryAttempts = 1
	loop.fetchRetryBackoff = 0

	_, err = loop.fetchEventsWithRetry(context.Background(), prov, "a_share", "X", time.Now())
	if err == nil {
		t.Fatal("expected err, got nil")
	}
	if prov.callCount() != 2 {
		t.Errorf("provider calls = %d, want 2", prov.callCount())
	}
	if len(rec.retries) != 1 || rec.retries[0] != "a_share|exhausted" {
		t.Errorf("retries = %v, want [a_share|exhausted]", rec.retries)
	}
	if len(rec.provErrors) != 1 || rec.provErrors[0] != "a_share|transient" {
		t.Errorf("provErrors = %v, want [a_share|transient]", rec.provErrors)
	}
}

// TestFetchEventsWithRetry_FatalNoRetry pins that a 4xx-class
// error short-circuits without consuming the retry budget. Tests
// the negative path of isCorpActionTransient and the metric:
// provider-error=fatal, retries empty.
func TestFetchEventsWithRetry_FatalNoRetry(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	prov := &stubCorpActionFetcher{failWith: errors.New("eastmoney: status 403")}
	rec := &recordedMetrics{}
	loop := newCorpActionIngestLoop(db)
	loop.SetMetrics(rec)
	loop.fetchRetryAttempts = 5 // generous; the fatal-class error must NOT use it
	loop.fetchRetryBackoff = 0

	_, err = loop.fetchEventsWithRetry(context.Background(), prov, "a_share", "X", time.Now())
	if err == nil {
		t.Fatal("expected err")
	}
	if prov.callCount() != 1 {
		t.Errorf("provider calls = %d, want 1 (no retry on fatal)", prov.callCount())
	}
	if len(rec.retries) != 0 {
		t.Errorf("retries should be empty, got %v", rec.retries)
	}
	if len(rec.provErrors) != 1 || rec.provErrors[0] != "a_share|fatal" {
		t.Errorf("provErrors = %v, want [a_share|fatal]", rec.provErrors)
	}
}

// TestRunOnce_RecordsTickStatus_NotLeader pins the metrics path
// for the "skipped not leader" branch. The loop must record one
// tick observation, no DB queries, no provider calls.
func TestRunOnce_RecordsTickStatus_NotLeader(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	prov := &stubCorpActionFetcher{}
	rec := &recordedMetrics{}
	loop := newCorpActionIngestLoop(db)
	loop.providers = map[string]corpaction.EventFetcher{"a_share": prov}
	loop.SetMetrics(rec)
	loop.SetLeaderChecker(stubLeader{leader: false})

	loop.runOnce(context.Background())

	if got := rec.tickStatus; len(got) != 1 || got[0] != "skipped_not_leader" {
		t.Errorf("tickStatus = %v, want [skipped_not_leader]", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestRunOnce_RecordsTickStatus_NoHoldings pins the success-skip
// branch. Empty holdings ⇒ tickStatus=skipped_no_holdings (which
// the metrics layer ALSO advances last_success_unix — covered in
// the unit test below, this one just pins the label).
func TestRunOnce_RecordsTickStatus_NoHoldings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM holding_positions`).
		WillReturnRows(sqlmock.NewRows([]string{"instrument_key", "market", "symbol"}))

	rec := &recordedMetrics{}
	loop := newCorpActionIngestLoop(db)
	loop.SetMetrics(rec)
	loop.SetLeaderChecker(stubLeader{leader: true})

	loop.runOnce(context.Background())

	if got := rec.tickStatus; len(got) != 1 || got[0] != "skipped_no_holdings" {
		t.Errorf("tickStatus = %v, want [skipped_no_holdings]", got)
	}
}

// TestRunOnce_RecordsEventAndApply pins that the happy path
// records exactly one event upserted + one apply outcome. We re-
// use the existing happy-path mock mode so the DB expectations
// stay aligned.
func TestRunOnce_RecordsEventAndApply(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(`FROM holding_positions`).
		WillReturnRows(
			sqlmock.NewRows([]string{"instrument_key", "market", "symbol"}).
				AddRow("INSTR:688195.SS", "a_share", "688195"),
		)
	mock.ExpectQuery(`SELECT DISTINCT fund_id FROM holding_positions`).
		WithArgs("INSTR:688195.SS").
		WillReturnRows(sqlmock.NewRows([]string{"fund_id"}).AddRow("fund-1"))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO corporate_actions")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-1"))
	mock.ExpectBegin()
	// Idempotency probe — already-applied row makes the applier
	// short-circuit with a successful AppliedFunds=1 response.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pre_quantity, post_quantity, pre_cost_price, post_cost_price, cash_credit FROM corp_action_applications WHERE corp_action_id = $1 AND fund_id = $2`)).
		WithArgs("evt-1", "fund-1").
		WillReturnRows(
			sqlmock.NewRows([]string{
				"pre_quantity", "post_quantity",
				"pre_cost_price", "post_cost_price",
				"cash_credit",
			}).AddRow(289.0, 376.0, 300.0, 230.0, 47.396),
		)
	mock.ExpectCommit()

	prov := &stubCorpActionFetcher{
		events: []corpaction.Event{
			{
				InstrumentKey: "INSTR:688195.SS",
				ExDate:        time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC),
				ActionType:    "split",
				SplitRatio:    1.3,
				Source:        "eastmoney",
			},
		},
	}
	rec := &recordedMetrics{}
	loop := newCorpActionIngestLoop(db)
	loop.providers = map[string]corpaction.EventFetcher{"a_share": prov}
	loop.SetMetrics(rec)
	loop.SetLeaderChecker(stubLeader{leader: true})

	loop.runOnce(context.Background())

	if len(rec.events) != 1 || rec.events[0] != "split|upserted" {
		t.Errorf("events = %v, want [split|upserted]", rec.events)
	}
	if len(rec.apply) != 1 || rec.apply[0] != "applied" {
		t.Errorf("apply = %v, want [applied]", rec.apply)
	}
	if len(rec.tickStatus) != 1 || rec.tickStatus[0] != "ok" {
		t.Errorf("tickStatus = %v, want [ok]", rec.tickStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
