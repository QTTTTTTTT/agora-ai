package decision

// Sprint C #3 contract tests for the Fingerprint / Trace
// observability helper. The tests cover:
//   - Empty input fingerprints clean (no signals present,
//     all counts zero, trading date empty string).
//   - Each per-block signal sets its corresponding presence bit.
//   - Counts match the underlying slice / map lengths.
//   - PresentBlocks lists the populated signals in the
//     declared canonical order so log strings are stable.
//   - SlogAttrs renders the full attribute set without panicking
//     and exposes the fund id + each presence bit.
//   - Trading date is rendered in UTC RFC-3339 (seconds
//     precision); zero TradingDate produces an empty string.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Fingerprint of an empty input → no signals present, zero counts.
func TestFingerprintEmptyInput(t *testing.T) {
	got := Fingerprint(DecisionInput{})
	if got.Signals != (SignalsPresence{}) {
		t.Errorf("empty input: signals must be all-false, got %+v", got.Signals)
	}
	if got.Counts != (SignalCounts{}) {
		t.Errorf("empty input: counts must be all-zero, got %+v", got.Counts)
	}
	if got.TradingDate != "" {
		t.Errorf("empty TradingDate must render as empty string, got %q", got.TradingDate)
	}
	if len(got.PresentBlocks()) != 0 {
		t.Errorf("empty input: PresentBlocks must be empty, got %v", got.PresentBlocks())
	}
}

// All-signals input fingerprints with the full presence bitmap.
func TestFingerprintAllSignalsPresent(t *testing.T) {
	tradingDate := time.Date(2026, 5, 25, 14, 30, 0, 0, time.UTC)
	rb := RiskBudgetSnapshot{}
	corr := CorrelationSnapshot{SampleSize: 2, HighCorrPairs: []HighCorrPair{{Left: "A", Right: "B", Rho: 0.9}}, CandidateSummaries: []CorrCandidateSummary{{Symbol: "X"}}}
	exp := ExposureSnapshot{TotalAssets: 1000, PositionCount: 1, Breaches: []string{"BREACH: x"}}

	in := DecisionInput{
		FundID:             "f1",
		PMAgentID:          "pm-1",
		TradingDate:        tradingDate,
		Universe:           []string{"AAPL", "MSFT"},
		Positions:          []DecisionPosition{{Symbol: "AAPL"}},
		InstrumentHints:    map[string]InstrumentHint{"AAPL": {}},
		RoundtableStance:   "bullish",
		BullCase:           "growth re-acceleration",
		BearCase:           "valuation stretched",
		QuantCase:          "trend up regime",
		SymbolVerdicts:     []RoundtableSymbolVerdict{{Symbol: "AAPL"}},
		FundamentalSummary: "AAPL PE 28",
		SectorRotation:     "tech +2.1%",
		NewsSentiment:      "mood neutral",
		SleeveScorecard:    "core sleeve +8bps",
		LessonReplay:       "do not chase",
		QuantSnapshots:     []SymbolQuantSnapshot{{Symbol: "AAPL"}},
		UniverseRanking:    []SymbolRanking{{Symbol: "AAPL"}},
		Cooldowns:          []SymbolCooldown{{Symbol: "AAPL"}},
		RiskBudget:         &rb,
		NewsCatalysts:      []SymbolNewsCatalysts{{Symbol: "AAPL"}},
		Exposure:           exp,
		Correlations:       &corr,
	}
	got := Fingerprint(in)
	if got.FundID != "f1" {
		t.Errorf("FundID = %q, want f1", got.FundID)
	}
	if got.PMAgentID != "pm-1" {
		t.Errorf("PMAgentID = %q, want pm-1", got.PMAgentID)
	}
	if got.TradingDate != "2026-05-25T14:30:00Z" {
		t.Errorf("TradingDate = %q, want 2026-05-25T14:30:00Z", got.TradingDate)
	}

	want := SignalsPresence{
		RoundtableStance:   true,
		BullCase:           true,
		BearCase:           true,
		QuantCase:          true,
		SymbolVerdicts:     true,
		FundamentalSummary: true,
		SectorRotation:     true,
		NewsSentiment:      true,
		SleeveScorecard:    true,
		LessonReplay:       true,
		InstrumentHints:    true,
		QuantSnapshots:     true,
		UniverseRanking:    true,
		Cooldowns:          true,
		RiskBudget:         true,
		NewsCatalysts:      true,
		Exposure:           true,
		Correlations:       true,
	}
	if got.Signals != want {
		t.Errorf("Signals presence mismatch:\n got %+v\nwant %+v", got.Signals, want)
	}

	if got.Counts.Universe != 2 || got.Counts.Positions != 1 ||
		got.Counts.InstrumentHints != 1 || got.Counts.QuantSnapshots != 1 ||
		got.Counts.UniverseRanking != 1 || got.Counts.Cooldowns != 1 ||
		got.Counts.NewsCatalysts != 1 || got.Counts.ExposureBreaches != 1 ||
		got.Counts.CorrelationsHigh != 1 || got.Counts.CorrCandidates != 1 {
		t.Errorf("Counts mismatch: %+v", got.Counts)
	}
}

// PresentBlocks returns the canonical order; tests both ordering
// AND that disabled signals are excluded.
func TestPresentBlocksCanonicalOrder(t *testing.T) {
	rb := RiskBudgetSnapshot{}
	in := DecisionInput{
		QuantSnapshots: []SymbolQuantSnapshot{{Symbol: "A"}},
		RiskBudget:     &rb,
		SleeveScorecard: "x",
		Exposure: ExposureSnapshot{TotalAssets: 100, PositionCount: 1},
	}
	got := Fingerprint(in).PresentBlocks()
	want := []string{"sleeveScorecard", "quantSnapshots", "riskBudget", "exposure"}
	if len(got) != len(want) {
		t.Fatalf("PresentBlocks length: got %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("PresentBlocks[%d] = %q, want %q (full slice: %v)", i, got[i], name, got)
		}
	}
}

// SlogAttrs must render every presence flag + every count so a
// downstream log consumer can grep on a stable shape.
func TestSlogAttrsExposesAllFields(t *testing.T) {
	tr := Fingerprint(DecisionInput{
		FundID:      "abc",
		TradingDate: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	})
	attrs := tr.SlogAttrs()
	if len(attrs) < 15 {
		t.Fatalf("SlogAttrs unexpectedly small: %d items", len(attrs))
	}
	// Round-trip via a recording handler so we exercise the
	// actual slog interface rather than just slicing into the
	// raw attr list.
	buf := &recordingHandler{records: []slog.Record{}}
	logger := slog.New(buf)
	logger.Info("decision_input_fingerprint", attrs...)
	if len(buf.records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(buf.records))
	}
	rec := buf.records[0]
	keys := map[string]bool{}
	rec.Attrs(func(a slog.Attr) bool {
		keys[a.Key] = true
		return true
	})
	required := []string{
		"fund_id", "trading_date",
		"count_universe", "count_correlations_high",
		"p_quant_snapshots", "p_exposure", "p_correlations",
	}
	for _, k := range required {
		if !keys[k] {
			t.Errorf("required slog key %q missing from record (keys=%v)", k, keys)
		}
	}
}

// Trading date with a non-UTC zone collapses to UTC seconds in
// the rendered string (drift across hosts otherwise muddies the
// trace).
func TestTradingDateNormalisedToUTC(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	tradingDate := time.Date(2026, 5, 25, 22, 30, 0, 0, loc) // 14:30 UTC
	got := Fingerprint(DecisionInput{TradingDate: tradingDate})
	if got.TradingDate != "2026-05-25T14:30:00Z" {
		t.Errorf("TradingDate = %q, want 2026-05-25T14:30:00Z", got.TradingDate)
	}
}

// Nil-but-populated Correlations: empty snap with HasSignal=false
// should NOT set the Correlations presence flag.
func TestCorrelationsPresenceRespectsHasSignal(t *testing.T) {
	corr := CorrelationSnapshot{SampleSize: 1} // HasSignal=false
	in := DecisionInput{Correlations: &corr}
	got := Fingerprint(in)
	if got.Signals.Correlations {
		t.Error("HasSignal-false Correlations must not set the presence bit")
	}
	if got.Counts.CorrelationsHigh != 0 {
		t.Errorf("CorrelationsHigh count = %d, want 0", got.Counts.CorrelationsHigh)
	}
}

// PresentBlocks string must always be a strict subset of the 18
// supported signal names — guards against accidental free-text
// drift in callers that grep this list.
func TestPresentBlocksKnownVocabulary(t *testing.T) {
	known := map[string]struct{}{
		"roundtableStance":   {},
		"bullCase":           {},
		"bearCase":           {},
		"quantCase":          {},
		"symbolVerdicts":     {},
		"fundamentalSummary": {},
		"sectorRotation":     {},
		"newsSentiment":      {},
		"sleeveScorecard":    {},
		"lessonReplay":       {},
		"instrumentHints":    {},
		"quantSnapshots":     {},
		"universeRanking":    {},
		"cooldowns":          {},
		"riskBudget":         {},
		"newsCatalysts":      {},
		"exposure":           {},
		"correlations":       {},
	}
	// Construct an input that flips every signal so PresentBlocks
	// emits every supported entry exactly once.
	rb := RiskBudgetSnapshot{}
	corr := CorrelationSnapshot{SampleSize: 2, HighCorrPairs: []HighCorrPair{{Left: "A", Right: "B", Rho: 0.9}}}
	in := DecisionInput{
		Universe:           []string{"A"},
		InstrumentHints:    map[string]InstrumentHint{"A": {}},
		RoundtableStance:   "x",
		BullCase:           "x",
		BearCase:           "x",
		QuantCase:          "x",
		SymbolVerdicts:     []RoundtableSymbolVerdict{{}},
		FundamentalSummary: "x",
		SectorRotation:     "x",
		NewsSentiment:      "x",
		SleeveScorecard:    "x",
		LessonReplay:       "x",
		QuantSnapshots:     []SymbolQuantSnapshot{{}},
		UniverseRanking:    []SymbolRanking{{}},
		Cooldowns:          []SymbolCooldown{{}},
		RiskBudget:         &rb,
		NewsCatalysts:      []SymbolNewsCatalysts{{}},
		Exposure:           ExposureSnapshot{TotalAssets: 100, PositionCount: 1},
		Correlations:       &corr,
	}
	got := Fingerprint(in).PresentBlocks()
	if len(got) != len(known) {
		t.Errorf("PresentBlocks count = %d, want %d (%v)", len(got), len(known), got)
	}
	for _, name := range got {
		if _, ok := known[name]; !ok {
			t.Errorf("unknown block %q in PresentBlocks (joined: %q)", name, strings.Join(got, ","))
		}
	}
}

// AbsentBlocks is the inverse — on an empty input every block
// should be reported absent, and on an all-signals input the
// list should be empty.
func TestAbsentBlocksEmptyInputReportsAllAbsent(t *testing.T) {
	got := Fingerprint(DecisionInput{}).AbsentBlocks()
	// 18 canonical signal blocks (matches PresentBlocksKnownVocabulary).
	if len(got) != 18 {
		t.Errorf("empty input AbsentBlocks = %d entries, want 18 (%v)", len(got), got)
	}
}

// PresentBlocks + AbsentBlocks must partition the canonical
// signal vocabulary: no overlap, total = 18.
func TestPresentPlusAbsentBlocksPartitionVocabulary(t *testing.T) {
	rb := RiskBudgetSnapshot{}
	corr := CorrelationSnapshot{SampleSize: 2, HighCorrPairs: []HighCorrPair{{Left: "A", Right: "B", Rho: 0.9}}}
	for _, tc := range []struct {
		name string
		in   DecisionInput
	}{
		{"empty", DecisionInput{}},
		{"all_signals", DecisionInput{
			Universe:           []string{"A"},
			InstrumentHints:    map[string]InstrumentHint{"A": {}},
			RoundtableStance:   "x",
			BullCase:           "x",
			BearCase:           "x",
			QuantCase:          "x",
			SymbolVerdicts:     []RoundtableSymbolVerdict{{}},
			FundamentalSummary: "x",
			SectorRotation:     "x",
			NewsSentiment:      "x",
			SleeveScorecard:    "x",
			LessonReplay:       "x",
			QuantSnapshots:     []SymbolQuantSnapshot{{}},
			UniverseRanking:    []SymbolRanking{{}},
			Cooldowns:          []SymbolCooldown{{}},
			RiskBudget:         &rb,
			NewsCatalysts:      []SymbolNewsCatalysts{{}},
			Exposure:           ExposureSnapshot{TotalAssets: 100, PositionCount: 1},
			Correlations:       &corr,
		}},
		{"partial", DecisionInput{
			BullCase:       "x",
			BearCase:       "x",
			QuantSnapshots: []SymbolQuantSnapshot{{}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := Fingerprint(tc.in)
			present := tr.PresentBlocks()
			absent := tr.AbsentBlocks()
			if len(present)+len(absent) != 18 {
				t.Errorf("present(%d) + absent(%d) = %d, want 18 (present=%v absent=%v)",
					len(present), len(absent), len(present)+len(absent), present, absent)
			}
			seen := map[string]bool{}
			for _, b := range present {
				if seen[b] {
					t.Errorf("block %q appears twice in present", b)
				}
				seen[b] = true
			}
			for _, b := range absent {
				if seen[b] {
					t.Errorf("block %q appears in both present and absent (no partition)", b)
				}
				seen[b] = true
			}
		})
	}
}

// recordingHandler captures slog records to a slice so the test
// can inspect their attrs.
type recordingHandler struct {
	records []slog.Record
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }
