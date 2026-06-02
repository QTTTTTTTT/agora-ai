package recon

import (
	"testing"
	"time"
)

func newSnapshot() *InternalSnapshot {
	return &InternalSnapshot{
		FundID:   "fund-1",
		AsOfDate: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
}

func newStatement() *Statement {
	return &Statement{
		FundID:        "fund-1",
		StatementDate: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Source:        SourceMock,
	}
}

// Round trip: identical position lists ⇒ no breaks.
func TestEngine_AllAligned_NoBreaks(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()

	stmt.Positions = []StatementPosition{{Symbol: "AAPL", Quantity: 100, AvgCost: 175.50, Currency: "USD"}}
	stmt.Cash = []StatementCash{{Currency: "USD", Balance: 5000}}
	snap.Positions = []InternalPosition{{Symbol: "aapl", Quantity: 100, AvgCost: 175.50, Currency: "usd"}}
	snap.Cash = []InternalCash{{Currency: "USD", Balance: 5000}}

	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 0 {
		t.Errorf("unexpected breaks: %+v", res.Breaks)
	}
}

func TestEngine_PositionMissingInternal(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	stmt.Positions = []StatementPosition{{Symbol: "TSLA", Quantity: 50, AvgCost: 250, Currency: "USD"}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 1 {
		t.Fatalf("breaks = %+v", res.Breaks)
	}
	if res.Breaks[0].Type != BreakPositionMissingInternal {
		t.Errorf("type = %s", res.Breaks[0].Type)
	}
	if res.Breaks[0].Severity != SeverityCritical {
		t.Errorf("severity = %s", res.Breaks[0].Severity)
	}
}

func TestEngine_PositionMissingBroker_NonZero(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	snap.Positions = []InternalPosition{{Symbol: "GOOG", Quantity: 10, AvgCost: 100, Currency: "USD"}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 1 {
		t.Fatalf("breaks = %+v", res.Breaks)
	}
	if res.Breaks[0].Type != BreakPositionMissingBroker || res.Breaks[0].Severity != SeverityCritical {
		t.Errorf("got %+v", res.Breaks[0])
	}
}

func TestEngine_PositionMissingBroker_ZeroQty_Info(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	// Internal qty rounds to 0 (closed position dust); shouldn't fire critical.
	snap.Positions = []InternalPosition{{Symbol: "GOOG", Quantity: 0.0000001}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 1 || res.Breaks[0].Severity != SeverityInfo {
		t.Errorf("got %+v", res.Breaks)
	}
}

func TestEngine_PositionQuantityMismatch_LargeDriftCritical(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	stmt.Positions = []StatementPosition{{Symbol: "AAPL", Quantity: 100, AvgCost: 175}}
	snap.Positions = []InternalPosition{{Symbol: "AAPL", Quantity: 110, AvgCost: 175}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 1 {
		t.Fatalf("breaks = %+v", res.Breaks)
	}
	if res.Breaks[0].Type != BreakPositionQuantityMismatch {
		t.Errorf("type = %s", res.Breaks[0].Type)
	}
	if res.Breaks[0].Severity != SeverityCritical {
		t.Errorf("severity = %s, want critical (10/100 = 10%%)", res.Breaks[0].Severity)
	}
}

func TestEngine_PositionQuantityMismatch_SmallDriftWarning(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	stmt.Positions = []StatementPosition{{Symbol: "AAPL", Quantity: 1000}}
	snap.Positions = []InternalPosition{{Symbol: "AAPL", Quantity: 1001}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 1 || res.Breaks[0].Severity != SeverityWarning {
		t.Errorf("got %+v", res.Breaks)
	}
}

func TestEngine_PositionAvgCostMismatch_RespectsToleranceBand(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	// 0.005% of 200 = 0.01; AvgCostAbs floor = 0.0001. We use the
	// max → 0.01. A drift of 0.005 should NOT fire.
	stmt.Positions = []StatementPosition{{Symbol: "AAPL", Quantity: 1, AvgCost: 200.000}}
	snap.Positions = []InternalPosition{{Symbol: "AAPL", Quantity: 1, AvgCost: 200.005}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 0 {
		t.Errorf("expected no breaks (within band), got %+v", res.Breaks)
	}

	// Drift of 0.02 should fire.
	snap.Positions[0].AvgCost = 200.020
	res = e.Diff(stmt, snap)
	if len(res.Breaks) != 1 || res.Breaks[0].Type != BreakPositionAvgCostMismatch {
		t.Errorf("got %+v", res.Breaks)
	}
}

func TestEngine_CashBalanceMismatch(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	stmt.Cash = []StatementCash{{Currency: "USD", Balance: 10000}}
	snap.Cash = []InternalCash{{Currency: "USD", Balance: 9985}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 1 {
		t.Fatalf("breaks = %+v", res.Breaks)
	}
	if res.Breaks[0].Type != BreakCashBalanceMismatch {
		t.Errorf("type = %s", res.Breaks[0].Type)
	}
}

func TestEngine_CashCurrencyMissingInternal(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	stmt.Cash = []StatementCash{{Currency: "CNY", Balance: 7100}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 1 || res.Breaks[0].Type != BreakCashCurrencyMissingInternal {
		t.Errorf("got %+v", res.Breaks)
	}
}

func TestEngine_TradeMatchedNoBreaks(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	stmt.Trades = []StatementTrade{
		{BrokerTradeID: "T1", Symbol: "AAPL", Side: "buy", Quantity: 50, Price: 175.5},
	}
	snap.Trades = []InternalTrade{
		{ExternalRef: "T1", Symbol: "AAPL", Side: "buy", Quantity: 50, Price: 175.5},
	}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 0 {
		t.Errorf("got %+v", res.Breaks)
	}
}

func TestEngine_TradeSideMismatch_Critical(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	stmt.Trades = []StatementTrade{
		{BrokerTradeID: "T1", Symbol: "AAPL", Side: "buy", Quantity: 50, Price: 175.5},
	}
	snap.Trades = []InternalTrade{
		{ExternalRef: "T1", Symbol: "AAPL", Side: "sell", Quantity: 50, Price: 175.5},
	}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 1 || res.Breaks[0].Type != BreakTradeSideMismatch || res.Breaks[0].Severity != SeverityCritical {
		t.Errorf("got %+v", res.Breaks)
	}
}

func TestEngine_TradeMissingInternal_Critical(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	stmt.Trades = []StatementTrade{{BrokerTradeID: "T1", Symbol: "AAPL", Side: "buy", Quantity: 50, Price: 175.5}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 1 || res.Breaks[0].Type != BreakTradeMissingInternal {
		t.Errorf("got %+v", res.Breaks)
	}
}

func TestEngine_TradeMissingBroker_Critical(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	snap.Trades = []InternalTrade{{ExternalRef: "T1", Symbol: "AAPL", Side: "buy", Quantity: 50, Price: 175.5}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 1 || res.Breaks[0].Type != BreakTradeMissingBroker {
		t.Errorf("got %+v", res.Breaks)
	}
}

func TestEngine_TradePriceMismatch_RespectsBand(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	stmt.Trades = []StatementTrade{{BrokerTradeID: "T1", Symbol: "AAPL", Side: "buy", Quantity: 50, Price: 175.5}}
	// Within band: 0.005% of 175.5 = 0.008775 → drift of 0.005 OK.
	snap.Trades = []InternalTrade{{ExternalRef: "T1", Symbol: "AAPL", Side: "buy", Quantity: 50, Price: 175.505}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 0 {
		t.Errorf("expected no breaks within band: %+v", res.Breaks)
	}
	// Drift of 0.50 should fire.
	snap.Trades[0].Price = 176.0
	res = e.Diff(stmt, snap)
	if len(res.Breaks) != 1 || res.Breaks[0].Type != BreakTradePriceMismatch {
		t.Errorf("got %+v", res.Breaks)
	}
}

func TestEngine_TradeMatch_BrokerOrderID_Fallback(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	// Broker only emits order id on the EOD; trade id is fresh.
	stmt.Trades = []StatementTrade{{
		BrokerTradeID: "EOD-1",
		BrokerOrderID: "ORD-42",
		Symbol:        "AAPL", Side: "buy", Quantity: 50, Price: 175.5,
	}}
	snap.Trades = []InternalTrade{{ExternalRef: "ORD-42", Symbol: "AAPL", Side: "buy", Quantity: 50, Price: 175.5}}
	res := e.Diff(stmt, snap)
	if len(res.Breaks) != 0 {
		t.Errorf("expected match by broker_order_id, got %+v", res.Breaks)
	}
}

func TestEngine_Counts_ByseverityCorrect(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	stmt := newStatement()
	snap := newSnapshot()
	stmt.Positions = []StatementPosition{
		{Symbol: "AAPL", Quantity: 100, AvgCost: 175}, // matches; OK
		{Symbol: "TSLA", Quantity: 50},                // missing internal → critical
	}
	snap.Positions = []InternalPosition{
		{Symbol: "AAPL", Quantity: 100, AvgCost: 175},
		{Symbol: "GOOG", Quantity: 10}, // missing broker → critical
	}
	res := e.Diff(stmt, snap)
	if res.Counts[SeverityCritical] != 2 {
		t.Errorf("critical = %d, want 2; res = %+v", res.Counts[SeverityCritical], res)
	}
}

func TestEngine_NilInputs_Safe(t *testing.T) {
	e := NewEngine(DefaultTolerances)
	if r := e.Diff(nil, nil); len(r.Breaks) != 0 {
		t.Errorf("nil/nil should return empty: %+v", r)
	}
}

func TestSeverityForQty_ZeroBroker(t *testing.T) {
	if got := severityForQty(1, 0); got != SeverityCritical {
		t.Errorf("got %s", got)
	}
}

func TestDiffPercent_DivByZeroNil(t *testing.T) {
	if diffPercent(1, 0) != nil {
		t.Error("expected nil on div by zero")
	}
}
