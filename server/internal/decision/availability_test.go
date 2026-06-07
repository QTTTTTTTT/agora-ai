package decision

import (
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/earnings"
	"github.com/fundai/server/internal/pead"
)

// ComputeUnavailableSymbols returns nil when every Universe / Position
// symbol shows up in at least one per-symbol signal block. The
// canonical "happy path" — wiring layer fully populated.
func TestComputeUnavailableSymbolsHappyPath(t *testing.T) {
	in := DecisionInput{
		Universe: []string{"AAPL", "MSFT"},
		QuantSnapshots: []SymbolQuantSnapshot{
			{Symbol: "AAPL", Regime: "trend_up", Close: 187.0, ATR14: 2.0, ATRPct: 1.0, PositionSizeCeilingPct: 0.05},
			{Symbol: "MSFT", Regime: "range", Close: 350.0, ATR14: 4.0, ATRPct: 1.1, PositionSizeCeilingPct: 0.04},
		},
	}
	got := ComputeUnavailableSymbols(in)
	if len(got) != 0 {
		t.Fatalf("expected no unavailable symbols, got %+v", got)
	}
}

// A symbol that the operator put in Universe but no per-symbol block
// covers must surface in the unavailable list with reason
// "no_signal_blocks".
func TestComputeUnavailableSymbolsFlagsNakedUniverseEntry(t *testing.T) {
	in := DecisionInput{
		Universe: []string{"AAPL", "GHOST"},
		QuantSnapshots: []SymbolQuantSnapshot{
			{Symbol: "AAPL", Regime: "trend_up", Close: 187.0, ATR14: 2.0, ATRPct: 1.0, PositionSizeCeilingPct: 0.05},
		},
	}
	got := ComputeUnavailableSymbols(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 unavailable symbol, got %+v", got)
	}
	if got[0].Symbol != "GHOST" {
		t.Errorf("expected GHOST, got %q", got[0].Symbol)
	}
	if got[0].Reason != UnavailableReasonNoSignalBlocks {
		t.Errorf("expected reason=%q, got %q", UnavailableReasonNoSignalBlocks, got[0].Reason)
	}
}

// Held positions on uncovered symbols are flagged the same way as
// universe entries — the LLM still must not estimate prices on
// them. Symbol case is normalised to upper-case so the audit log
// stays consistent.
func TestComputeUnavailableSymbolsFlagsHeldPositionsToo(t *testing.T) {
	in := DecisionInput{
		Universe: []string{"AAPL"},
		Positions: []DecisionPosition{
			{Symbol: "BABA", Quantity: 100, AvailableQty: 100, CurrentPrice: 75.0},
		},
		QuantSnapshots: []SymbolQuantSnapshot{
			{Symbol: "AAPL", Regime: "trend_up", Close: 187.0, ATR14: 2.0, ATRPct: 1.0, PositionSizeCeilingPct: 0.05},
		},
	}
	got := ComputeUnavailableSymbols(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 unavailable, got %+v", got)
	}
	if got[0].Symbol != "BABA" {
		t.Errorf("expected BABA, got %q", got[0].Symbol)
	}
}

// Coverage is the UNION across blocks: a symbol that has news
// catalysts but no quant snapshot must NOT appear as unavailable.
// Likewise a symbol that has only an earnings event must count as
// covered.
func TestComputeUnavailableSymbolsUnionAcrossBlocks(t *testing.T) {
	in := DecisionInput{
		Universe: []string{"AAPL", "MSFT", "TSLA", "NVDA"},
		QuantSnapshots: []SymbolQuantSnapshot{
			{Symbol: "AAPL", Regime: "trend_up", Close: 1, ATR14: 1, ATRPct: 1, PositionSizeCeilingPct: 0.05},
		},
		NewsCatalysts: []SymbolNewsCatalysts{
			{Symbol: "MSFT"},
		},
		IntradaySnapshots: []IntradayContext{
			{Symbol: "TSLA", TrendDirection: "range"},
		},
		EarningsCalendar: &EarningsCalendarSnapshot{
			HorizonDays: 14,
			PerSymbol: map[string]earnings.Event{
				"NVDA": {Symbol: "NVDA", EventDate: time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)},
			},
		},
	}
	got := ComputeUnavailableSymbols(in)
	if len(got) != 0 {
		t.Fatalf("expected zero unavailable, got %+v", got)
	}
}

// PEAD signals count as coverage. The block carries Symbol on each
// Signal entry; ComputeUnavailableSymbols must recognise that.
func TestComputeUnavailableSymbolsRecognisesPEADCoverage(t *testing.T) {
	in := DecisionInput{
		Universe: []string{"NFLX"},
		PEAD: &PEADSnapshot{
			LookbackDays: 60,
			Signals: []pead.Signal{
				{Symbol: "NFLX", EventDate: time.Now().Add(-72 * time.Hour), DaysSinceEvent: 3},
			},
		},
	}
	got := ComputeUnavailableSymbols(in)
	if len(got) != 0 {
		t.Fatalf("expected zero unavailable (PEAD covers NFLX), got %+v", got)
	}
}

// Empty Universe + empty Positions must short-circuit to nil; the
// helper must never return an empty non-nil slice that bloats the
// prompt JSON via omitempty edge cases.
func TestComputeUnavailableSymbolsEmptyInput(t *testing.T) {
	got := ComputeUnavailableSymbols(DecisionInput{})
	if got != nil {
		t.Fatalf("expected nil on empty input, got %+v (len=%d)", got, len(got))
	}
}

// The output is sorted alphabetically by symbol so successive calls
// on the same input emit identical JSON. Lower-case input is upper-
// cased so audit logs grep cleanly.
func TestComputeUnavailableSymbolsSortedAndUppercased(t *testing.T) {
	in := DecisionInput{
		Universe: []string{"zzz", "aaa", "mmm"},
	}
	got := ComputeUnavailableSymbols(in)
	if len(got) != 3 {
		t.Fatalf("expected 3 unavailable, got %+v", got)
	}
	if got[0].Symbol != "AAA" || got[1].Symbol != "MMM" || got[2].Symbol != "ZZZ" {
		t.Errorf("expected alphabetical AAA, MMM, ZZZ, got %v", []string{got[0].Symbol, got[1].Symbol, got[2].Symbol})
	}
}

// The same symbol appearing in BOTH Universe AND Positions must
// appear at most once in the unavailable list — coverage is checked
// per distinct symbol, not per slot.
func TestComputeUnavailableSymbolsDeduplicatesUniversePositionOverlap(t *testing.T) {
	in := DecisionInput{
		Universe: []string{"GHOST"},
		Positions: []DecisionPosition{
			{Symbol: "GHOST", Quantity: 10, AvailableQty: 10},
		},
	}
	got := ComputeUnavailableSymbols(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 unavailable (deduped), got %+v", got)
	}
	if got[0].Symbol != "GHOST" {
		t.Errorf("expected GHOST, got %q", got[0].Symbol)
	}
}

// systemPrompt must document the unavailableSymbols rail so the
// LLM cannot accidentally regress to "estimate from training memory"
// behaviour. We pin enough substrings to detect a rule-block deletion
// but not so many that minor wording tweaks break the test.
func TestSystemPromptDocumentsUnavailableSymbolsRule(t *testing.T) {
	prompt := systemPrompt()
	required := []string{
		"input.unavailableSymbols",
		"data unavailable",
		"Never estimate, guess, or fabricate",
		"no_signal_blocks",
	}
	for _, frag := range required {
		if !strings.Contains(prompt, frag) {
			t.Errorf("systemPrompt() missing %q — the unavailableSymbols anti-hallucination rule has regressed", frag)
		}
	}
}

// userPrompt must serialise UnavailableSymbols under the
// documented JSON key, including the reason tag, so the LLM can cite
// it in reasoning.
func TestUserPromptIncludesUnavailableSymbolsBlock(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL", "GHOST"},
		QuantSnapshots: []SymbolQuantSnapshot{
			{Symbol: "AAPL", Regime: "trend_up", Close: 187.0, ATR14: 2.0, ATRPct: 1.0, PositionSizeCeilingPct: 0.05},
		},
		UnavailableSymbols: []UnavailableSymbol{
			{Symbol: "GHOST", Reason: UnavailableReasonNoSignalBlocks},
		},
	})
	if !strings.Contains(prompt, `"unavailableSymbols"`) {
		t.Errorf("user prompt missing unavailableSymbols key:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"symbol": "GHOST"`) {
		t.Errorf("GHOST symbol not in unavailableSymbols block:\n%s", prompt)
	}
	if !strings.Contains(prompt, `"reason": "no_signal_blocks"`) {
		t.Errorf("reason tag not surfaced:\n%s", prompt)
	}
}

// When UnavailableSymbols is nil / empty the prompt must omit the
// key entirely so the standard happy-path prompt stays slim.
func TestUserPromptOmitsUnavailableSymbolsWhenEmpty(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:      "f1",
		TradingDate: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:    []string{"AAPL"},
	})
	if strings.Contains(prompt, `"unavailableSymbols"`) {
		t.Errorf("empty UnavailableSymbols should not appear in prompt:\n%s", prompt)
	}
}

// W16-3 — system prompt must teach the PM how to use macroBriefing.
// Pre-fix the block was passed through but had no dedicated rule,
// only a passing mention in the locale clause. These pinned
// substrings force a CI failure if the rule block is ever deleted.
func TestSystemPromptDocumentsMacroBriefingRule(t *testing.T) {
	prompt := systemPrompt()
	required := []string{
		"input.macroBriefing",
		"FUND-LEVEL, not symbol-level",
		"Macro NEVER overrides a hard guardrail",
		"contradicts the roundtableDebate stance",
	}
	for _, frag := range required {
		if !strings.Contains(prompt, frag) {
			t.Errorf("systemPrompt() missing %q — the macroBriefing rule has regressed", frag)
		}
	}
}

// W16-3 — userPrompt must trim whitespace-only macroBriefing the
// same way it trims the other free-text blocks (FundamentalSummary,
// SectorRotation, NewsSentiment, …). Otherwise a string of pure
// spaces would survive omitempty and bloat the prompt with a noise
// row.
func TestUserPromptTrimsMacroBriefingWhitespace(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:        "f1",
		TradingDate:   time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:      []string{"AAPL"},
		MacroBriefing: "   \n\t   ",
	})
	if strings.Contains(prompt, `"macroBriefing"`) {
		t.Errorf("whitespace-only macroBriefing leaked into prompt:\n%s", prompt)
	}
}

// Non-empty macroBriefing surfaces verbatim under the documented
// JSON key so the LLM can read it.
func TestUserPromptIncludesMacroBriefing(t *testing.T) {
	prompt := userPrompt(DecisionInput{
		FundID:        "f1",
		TradingDate:   time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC),
		Universe:      []string{"AAPL"},
		MacroBriefing: "Fed pause; tech bid.",
	})
	if !strings.Contains(prompt, `"macroBriefing": "Fed pause; tech bid."`) {
		t.Errorf("macroBriefing not surfaced verbatim:\n%s", prompt)
	}
}
