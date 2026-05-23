package decision

// Sprint C #3 — decision input fingerprint observability.
//
// Why this exists. The PM prompt has become a wide, multi-block
// input (Sprint A + B + C). When the LLM returns "all watch" or
// the upstream rejects every plan it is hard to tell from the
// audit trail which signal blocks were actually in scope on the
// failing call — empty quantSnapshots? missing cooldowns? a 0-NAV
// exposure block silently omitted? The original
// "decisions-stuck-as-rejected" incident lost three days of
// triage to exactly that question.
//
// The Trace type below is a tiny, plain-data fingerprint of one
// DecisionInput: for each signal block it captures "was it
// present" and "how many rows did it carry". The wiring layer
// logs it via slog.Info on every PM call so the audit trail can
// be grepped by signal presence. The Trace is also small enough
// (~ 200 bytes serialised JSON) that it can be embedded in the
// plan's discussion snapshot for per-plan diagnosis.
//
// The fingerprint is deterministic and side-effect free — same
// DecisionInput ↦ same Trace. Tests exercise both the per-field
// presence detection and the SlogAttrs renderer.

import (
	"log/slog"
)

// SignalsPresence is the boolean side of the fingerprint: for
// each signal block we record whether the DecisionInput carried
// data the prompt would actually render. Empty slices / nil
// pointers / zero-NAV snapshots all evaluate false, matching the
// rules the prompt builder uses to omit the block.
type SignalsPresence struct {
	RoundtableStance   bool `json:"roundtable_stance"`
	BullCase           bool `json:"bull_case"`
	BearCase           bool `json:"bear_case"`
	QuantCase          bool `json:"quant_case"`
	SymbolVerdicts     bool `json:"symbol_verdicts"`
	FundamentalSummary bool `json:"fundamental_summary"`
	SectorRotation     bool `json:"sector_rotation"`
	NewsSentiment      bool `json:"news_sentiment"`
	SleeveScorecard    bool `json:"sleeve_scorecard"`
	LessonReplay       bool `json:"lesson_replay"`
	InstrumentHints    bool `json:"instrument_hints"`
	QuantSnapshots     bool `json:"quant_snapshots"`
	UniverseRanking    bool `json:"universe_ranking"`
	Cooldowns          bool `json:"cooldowns"`
	RiskBudget         bool `json:"risk_budget"`
	NewsCatalysts      bool `json:"news_catalysts"`
	EarningsCalendar   bool `json:"earnings_calendar"`
	Exposure           bool `json:"exposure"`
	Correlations       bool `json:"correlations"`
}

// SignalCounts captures the row-count side of the fingerprint —
// enough to tell "we had quantSnapshots but they were empty"
// from "we had quantSnapshots for 12 names".
type SignalCounts struct {
	Universe          int `json:"universe"`
	Positions         int `json:"positions"`
	InstrumentHints   int `json:"instrument_hints"`
	QuantSnapshots    int `json:"quant_snapshots"`
	UniverseRanking   int `json:"universe_ranking"`
	Cooldowns         int `json:"cooldowns"`
	NewsCatalysts     int `json:"news_catalysts"`
	EarningsCalendar  int `json:"earnings_calendar"`
	ExposureBreaches  int `json:"exposure_breaches"`
	CorrelationsHigh  int `json:"correlations_high"`
	CorrCandidates    int `json:"corr_candidates"`
}

// Trace is the prompt-call fingerprint. Designed to be cheap to
// log (slog) and easy to embed in audit snapshots.
type Trace struct {
	FundID      string          `json:"fund_id"`
	PMAgentID   string          `json:"pm_agent_id,omitempty"`
	TradingDate string          `json:"trading_date"`
	Signals     SignalsPresence `json:"signals_present"`
	Counts      SignalCounts    `json:"counts"`
}

// Fingerprint distils a DecisionInput into a Trace. Pure: same
// input ↦ same output, no time / RNG / I/O dependencies.
func Fingerprint(in DecisionInput) Trace {
	t := Trace{
		FundID:    in.FundID,
		PMAgentID: in.PMAgentID,
		Counts: SignalCounts{
			Universe:         len(in.Universe),
			Positions:        len(in.Positions),
			InstrumentHints:  len(in.InstrumentHints),
			QuantSnapshots:   len(in.QuantSnapshots),
			UniverseRanking:  len(in.UniverseRanking),
			Cooldowns:        len(in.Cooldowns),
			NewsCatalysts:    len(in.NewsCatalysts),
			ExposureBreaches: len(in.Exposure.Breaches),
		},
		Signals: SignalsPresence{
			RoundtableStance:   in.RoundtableStance != "",
			BullCase:           in.BullCase != "",
			BearCase:           in.BearCase != "",
			QuantCase:          in.QuantCase != "",
			SymbolVerdicts:     len(in.SymbolVerdicts) > 0,
			FundamentalSummary: in.FundamentalSummary != "",
			SectorRotation:     in.SectorRotation != "",
			NewsSentiment:      in.NewsSentiment != "",
			SleeveScorecard:    in.SleeveScorecard != "",
			LessonReplay:       in.LessonReplay != "",
			InstrumentHints:    len(in.InstrumentHints) > 0,
			QuantSnapshots:     len(in.QuantSnapshots) > 0,
			UniverseRanking:    len(in.UniverseRanking) > 0,
			Cooldowns:          len(in.Cooldowns) > 0,
			RiskBudget:         in.RiskBudget != nil,
			NewsCatalysts:      len(in.NewsCatalysts) > 0,
			EarningsCalendar:   in.EarningsCalendar != nil && in.EarningsCalendar.HasSignal(),
			Exposure:           in.Exposure.HasSignal(),
			Correlations:       in.Correlations != nil && in.Correlations.HasSignal(),
		},
	}
	if in.EarningsCalendar != nil {
		t.Counts.EarningsCalendar = len(in.EarningsCalendar.PerSymbol)
	}
	if !in.TradingDate.IsZero() {
		// RFC-3339 keeps the trace JSON parseable by any
		// downstream log consumer. Truncate the precision to
		// seconds — sub-second on a trading-date stamp is
		// always 0 anyway, and avoids the zone-offset suffix
		// drifting across hosts.
		t.TradingDate = in.TradingDate.UTC().Format("2006-01-02T15:04:05Z")
	}
	if t.Signals.Correlations && in.Correlations != nil {
		t.Counts.CorrelationsHigh = len(in.Correlations.HighCorrPairs)
		t.Counts.CorrCandidates = len(in.Correlations.CandidateSummaries)
	}
	return t
}

// SlogAttrs renders the Trace as a flat slog attribute list. The
// shape is intentionally flat (no nested groups) so it survives
// JSON / logfmt / text encoders uniformly and stays grep-able.
// Calling code does:
//
//	slog.Info("decision_input_fingerprint", trace.SlogAttrs()...)
func (t Trace) SlogAttrs() []any {
	return []any{
		slog.String("fund_id", t.FundID),
		slog.String("pm_agent_id", t.PMAgentID),
		slog.String("trading_date", t.TradingDate),
		// Counts.
		slog.Int("count_universe", t.Counts.Universe),
		slog.Int("count_positions", t.Counts.Positions),
		slog.Int("count_instrument_hints", t.Counts.InstrumentHints),
		slog.Int("count_quant_snapshots", t.Counts.QuantSnapshots),
		slog.Int("count_universe_ranking", t.Counts.UniverseRanking),
		slog.Int("count_cooldowns", t.Counts.Cooldowns),
		slog.Int("count_news_catalysts", t.Counts.NewsCatalysts),
		slog.Int("count_earnings_calendar", t.Counts.EarningsCalendar),
		slog.Int("count_exposure_breaches", t.Counts.ExposureBreaches),
		slog.Int("count_correlations_high", t.Counts.CorrelationsHigh),
		slog.Int("count_corr_candidates", t.Counts.CorrCandidates),
		// Presence flags.
		slog.Bool("p_roundtable_stance", t.Signals.RoundtableStance),
		slog.Bool("p_bull_case", t.Signals.BullCase),
		slog.Bool("p_bear_case", t.Signals.BearCase),
		slog.Bool("p_quant_case", t.Signals.QuantCase),
		slog.Bool("p_symbol_verdicts", t.Signals.SymbolVerdicts),
		slog.Bool("p_fundamental_summary", t.Signals.FundamentalSummary),
		slog.Bool("p_sector_rotation", t.Signals.SectorRotation),
		slog.Bool("p_news_sentiment", t.Signals.NewsSentiment),
		slog.Bool("p_sleeve_scorecard", t.Signals.SleeveScorecard),
		slog.Bool("p_lesson_replay", t.Signals.LessonReplay),
		slog.Bool("p_instrument_hints", t.Signals.InstrumentHints),
		slog.Bool("p_quant_snapshots", t.Signals.QuantSnapshots),
		slog.Bool("p_universe_ranking", t.Signals.UniverseRanking),
		slog.Bool("p_cooldowns", t.Signals.Cooldowns),
		slog.Bool("p_risk_budget", t.Signals.RiskBudget),
		slog.Bool("p_news_catalysts", t.Signals.NewsCatalysts),
		slog.Bool("p_earnings_calendar", t.Signals.EarningsCalendar),
		slog.Bool("p_exposure", t.Signals.Exposure),
		slog.Bool("p_correlations", t.Signals.Correlations),
	}
}

// PresentBlocks returns the order-stable list of signal-block
// names that were present in the input. Designed for "audit
// ribbon" strings the wiring layer can stamp into plan reasoning
// ("PM signal blocks: quantSnapshots, ranking, exposure").
//
// Manual append rather than reflection so a future signal
// addition stays compile-time visible.
func (t Trace) PresentBlocks() []string {
	var out []string
	if t.Signals.RoundtableStance {
		out = append(out, "roundtableStance")
	}
	if t.Signals.BullCase {
		out = append(out, "bullCase")
	}
	if t.Signals.BearCase {
		out = append(out, "bearCase")
	}
	if t.Signals.QuantCase {
		out = append(out, "quantCase")
	}
	if t.Signals.SymbolVerdicts {
		out = append(out, "symbolVerdicts")
	}
	if t.Signals.FundamentalSummary {
		out = append(out, "fundamentalSummary")
	}
	if t.Signals.SectorRotation {
		out = append(out, "sectorRotation")
	}
	if t.Signals.NewsSentiment {
		out = append(out, "newsSentiment")
	}
	if t.Signals.SleeveScorecard {
		out = append(out, "sleeveScorecard")
	}
	if t.Signals.LessonReplay {
		out = append(out, "lessonReplay")
	}
	if t.Signals.InstrumentHints {
		out = append(out, "instrumentHints")
	}
	if t.Signals.QuantSnapshots {
		out = append(out, "quantSnapshots")
	}
	if t.Signals.UniverseRanking {
		out = append(out, "universeRanking")
	}
	if t.Signals.Cooldowns {
		out = append(out, "cooldowns")
	}
	if t.Signals.RiskBudget {
		out = append(out, "riskBudget")
	}
	if t.Signals.NewsCatalysts {
		out = append(out, "newsCatalysts")
	}
	if t.Signals.EarningsCalendar {
		out = append(out, "earningsCalendar")
	}
	if t.Signals.Exposure {
		out = append(out, "exposure")
	}
	if t.Signals.Correlations {
		out = append(out, "correlations")
	}
	return out
}

// AbsentBlocks is the inverse of PresentBlocks: every signal
// block in the canonical vocabulary that was NOT included in
// the decision input. Sprint D #1 uses both lists to drive the
// fundai_decision_input_blocks_total counter (so dashboards
// can compute presence rate per block, not just hits).
//
// Order matches PresentBlocks so the union of the two equals
// the fixed signal vocabulary in a stable, reviewable order.
// Adding a new signal requires touching both methods + the
// SignalsPresence struct.
func (t Trace) AbsentBlocks() []string {
	var out []string
	if !t.Signals.RoundtableStance {
		out = append(out, "roundtableStance")
	}
	if !t.Signals.BullCase {
		out = append(out, "bullCase")
	}
	if !t.Signals.BearCase {
		out = append(out, "bearCase")
	}
	if !t.Signals.QuantCase {
		out = append(out, "quantCase")
	}
	if !t.Signals.SymbolVerdicts {
		out = append(out, "symbolVerdicts")
	}
	if !t.Signals.FundamentalSummary {
		out = append(out, "fundamentalSummary")
	}
	if !t.Signals.SectorRotation {
		out = append(out, "sectorRotation")
	}
	if !t.Signals.NewsSentiment {
		out = append(out, "newsSentiment")
	}
	if !t.Signals.SleeveScorecard {
		out = append(out, "sleeveScorecard")
	}
	if !t.Signals.LessonReplay {
		out = append(out, "lessonReplay")
	}
	if !t.Signals.InstrumentHints {
		out = append(out, "instrumentHints")
	}
	if !t.Signals.QuantSnapshots {
		out = append(out, "quantSnapshots")
	}
	if !t.Signals.UniverseRanking {
		out = append(out, "universeRanking")
	}
	if !t.Signals.Cooldowns {
		out = append(out, "cooldowns")
	}
	if !t.Signals.RiskBudget {
		out = append(out, "riskBudget")
	}
	if !t.Signals.NewsCatalysts {
		out = append(out, "newsCatalysts")
	}
	if !t.Signals.EarningsCalendar {
		out = append(out, "earningsCalendar")
	}
	if !t.Signals.Exposure {
		out = append(out, "exposure")
	}
	if !t.Signals.Correlations {
		out = append(out, "correlations")
	}
	return out
}
