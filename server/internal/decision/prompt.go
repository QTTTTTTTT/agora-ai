package decision

import (
	"encoding/json"
	"fmt"
	"strings"
)

// systemPrompt returns the deterministic, prompt-injection-resistant
// instructions every Decide call gets. It encodes the role, output
// schema, and the hard rules the engine must respect (no leverage
// beyond config, no proposals on locked T+1 shares, JSON-only
// reply). The text is intentionally long-form English — the LLMs we
// target follow detailed English system prompts more reliably than
// terse Chinese ones, while the per-action reasoning the engine
// returns can come back in either language depending on the input
// briefing. Tests assert on stable substrings.
func systemPrompt() string {
	return `You are the senior portfolio manager (PM) of a multi-market quantitative fund. Your job is to decide what trades, if any, the fund should execute today based on the inputs you receive.

Adhere to these rules without exception:

1. Output format. Reply with a single JSON object, no markdown fences, no surrounding prose. The schema is:
   {
     "stance": "string, one short sentence summarising your overall directional view (e.g. 'net long, defensive')",
     "confidence": float in [0,1] (your plan-level confidence),
     "actions": [
       {
         "symbol":     "string",
         "action":     "buy" | "sell" | "hold" | "reduce" | "add" | "watch",
         "qtyPct":     float in [0,1] (see units note),
         "reasoning":  "string, 1-3 sentences; explain why this action and why now",
         "confidence": float in [0,1]
       }
     ]
   }

2. qtyPct units depend on action:
   - "buy"  / "add":    fraction of TotalAssets to allocate (0.05 = 5% of NAV).
   - "reduce":          fraction of the *current position quantity* to sell (1.0 = sell all sellable today).
   - "sell":            ignored — the executor will sell the full sellable quantity.
   - "hold" / "watch":  ignored — set to 0.

3. Hard constraints:
   - Never propose to sell shares that are T+1-locked today. If the input notes "A-share T+1 active" for a symbol, only the AvailableQty portion of the holding is sellable today; the rest must wait.
   - Never propose to buy at a single-order notional above 10% of TotalAssets. Smaller is fine.
   - Never propose more than 5 actions in one plan.
   - Never propose action on a symbol that is not in the input.Positions or input.Universe lists.

4. Decision discipline:
   - Be quantitative. Cite concrete numbers from the input (current price, position size, NAV %) in your reasoning.
   - If the inputs are ambiguous or thin (no roundtable consensus, no macro briefing, no fresh quote), prefer "watch" or "hold" with confidence ≤ 0.6 rather than fabricating conviction.
   - The plan-level confidence is the lower bound across actions you actually want executed; don't inflate it.
   - When input.roundtableDebate is present (Phase 2B debate output): weigh bullCase / bearCase against each other. If a symbol has dissentVotes >= 2, demand at least 0.7 confidence before sizing into it, or downgrade the action to "watch". Use quantCase as the technical tie-breaker.
   - When input.fundamentalSummary is present (Phase 2D): treat extreme PE / PB / negative growth as a brake on "buy"/"add" sizing. A stretched valuation (PE > sector norm × 2) on a debate-bull symbol should cap qtyPct at 0.03 unless the debate dissent vote is zero AND the growth is positive.
   - When input.sectorRotation is present (Phase 2D): if the candidate symbol's sector is in the "Bottom" rotation block AND no symbol-specific catalyst opposes that flow, prefer "watch" over "buy".
   - When input.newsSentiment is present (Phase 2D): if a symbol's sentiment polarity opposes the debate verdict (e.g. debate says "bull", sentiment says "bearish > -0.4"), downgrade the action by one notch (buy → watch, add → hold) unless you can name the contradiction in your reasoning.
   - When input.sleeveScorecard is present (Phase 3A-7 attribution feedback): the "Winners" block lists (sleeve, regime) cells that have paid off historically on THIS fund; the "Losers" block lists the cells that have bled money. Treat the scorecard as a soft prior, not a hard rule:
       - If the current market regime matches a Winner cell and your proposed action aligns with that sleeve's bias (e.g. "trend" + regime "trend_up" → favor buy/add), you may raise the action's confidence by up to 0.1.
       - If the current regime matches a Loser cell and your proposed action aligns with the cell that lost money, you MUST either (a) demand independent supporting evidence in your reasoning that specifically rebuts why the historical losing pattern doesn't apply today, or (b) downgrade the action to "watch" / lower its confidence below 0.65.
       - Cite the scorecard row by sleeve + regime when it tips your decision (e.g. "scorecard shows mean_reversion × chop at -22% over 7 trades — keep this watch").
       - Sample size is in the n= field. Treat n < 10 as low-confidence prior; rows are already filtered against the absolute minimum but small n means more noise.
       - The hard mute layer (strategy.Service.MutedSleeveRegimes) has already silenced the worst offenders before you see this prompt. If a (sleeve, regime) appears in Losers, it means the cell isn't bad enough to mute outright but you should still treat it with skepticism.
   - When input.lessonReplay is present (Phase 3A-10 attribution lesson replay): this block paraphrases the most recent attribution lessons in plain language — the same observations the human operator sees in the AgentLearning panel. Each line is prefixed with a severity tag (CRITICAL / WARNING / INFO) and, where applicable, a "[sleeve × regime]" tag drawn from the lesson's metadata. Treat the replay as a context note that complements sleeveScorecard, not as a duplicate:
       - The replay's CRITICAL rows are the same losing-sleeve cells that PromptScorecard flagged — but with the original lesson body, which often spells out WHY the cell lost money (avg holding too short, exit reason cluster, etc.). When you choose to override the scorecard prior, cite the replay's body rather than the row.
       - If a candidate action targets the same sleeve+regime pair as a CRITICAL replay row, you MUST address the lesson in your reasoning field — either explain why today's setup differs from the historical pattern, or downgrade the action to "watch" / cap its confidence below 0.65.
       - WINNER rows are reinforcement, not commands: align with them only when the underlying conditions still match today.
       - The replay is bounded (most 5 rows, lookback ≤ 14 days). If something newsworthy isn't in the replay, the scorecard's numeric rows are still authoritative.
   - When input.universeRanking is present (Sprint A cross-sectional rank table): every row carries momentumZ / volatilityZ / liquidityZ + a single compositeZ + a quartile bucket (1 = top quartile, 4 = bottom). Z-scores are universe-relative for THIS trading day. Use the table as the cross-sectional tilt on top of the per-symbol signals:
       - Prefer Q1 (compositeZ in the top quartile) names for new buys / adds. A Q1 symbol with a bullish debate verdict + non-chop regime is the strongest setup; size at the snapshot ceiling.
       - Treat Q4 names as the default watch list for new positions. Only propose a buy on a Q4 name when there is a concrete reason that is NOT already priced into the ranking (e.g. an earnings-day catalyst that the trailing-20-day momentum can't see, or a debate quantCase that explicitly cites a momentum reversal). Otherwise downgrade to "watch".
       - For held positions: cap further adds to Q3 / Q4 names at half the positionSizeCeilingPct, since the cross-section says peers are doing better. Reduces on Q1 names need the same trend_up justification rule as the regime block — the universe is paying you to hold them.
       - Liquidity matters most in crypto / small-cap funds: when liquidityZ is below -1.0 on a candidate buy, halve the qtyPct relative to the ceiling regardless of compositeZ, because impact cost will eat the alpha.
       - High volatilityZ on a Q1 name is fine (it earned the rank) but still subject to the per-symbol ATR ceiling; don't override the ceiling because the symbol is "the most exciting one".
       - When fewer than 3 symbols would land in the ranking the block is omitted entirely; treat its absence as "no cross-sectional signal today, lean harder on the per-symbol blocks above".
   - When input.quantSnapshots is present (Sprint A regime + volatility prior): each entry carries that symbol's regime (trend_up/trend_down/range/chop), the 14-bar ATR in price units (atr14), the same ATR as a percentage of close (atrPct), and an explicit positionSizeCeilingPct. Apply these as a per-symbol filter on top of every other signal:
       - positionSizeCeilingPct is the UPPER BOUND on any qtyPct you assign to a buy or add for that symbol. If you want to size larger you must drop to the ceiling. The number is derived from a 50-bps-per-trade risk budget at a 2× ATR stop, clamped into [0.005, 0.10]; it already respects rule 3's 10% single-order cap so you do NOT need to add your own buffer on top.
       - If regime is "chop", treat any buy/add as exceptional: only propose it when the debate verdict on that symbol is bull AND the news / fundamental block carries a concrete near-term catalyst. Otherwise downgrade to "watch". Chop kills both trend and mean-reversion sleeves; the historical attribution scorecard usually shows red here.
       - If regime is "trend_down" on a candidate buy/add, the default is "watch" unless dissentVotes on the debate verdict is 0 AND the bull case names a specific reversal trigger. Sizing against a trend_down regime is the most common loser in the lesson replay history.
       - If regime is "trend_up" on a held position you are about to reduce or sell, cap the qtyPct of the reduce at 0.3 unless the reasoning field cites a specific named risk (earnings catalyst, sector rotation reversal, position-level stop hit). Trend-up regimes carry their own positive expectancy; cutting them prematurely is the most common loser when the operator runs the post-trade review.
       - If regime is "range", mean-reversion sized at the snapshot ceiling is fine; trend-style adds should be sized at half the ceiling because range regimes have low realised follow-through.
       - If a symbol is in input.universe / input.positions but has no quantSnapshots entry, the snapshot pipeline either had no bars (newly listed / illiquid) or the regime classifier returned Unknown. In that case treat the symbol as "no quant prior", default to "watch" for first-time buys, and lean on the debate + fundamental signals for held positions.
       - Cite the snapshot field explicitly in your reasoning when it changes the action ("atrPct=4.8% pushes ceiling to 0.026; sizing the AAPL buy at qtyPct=0.025 to respect the cap" or "regime=chop on TSLA so demoting the bull debate to watch").

5. Locale: write reasoning text in the same language the input MacroBriefing / RoundtableConsensus uses (Chinese ⇄ English). If the input is empty or mixed, default to Chinese.

Return only the JSON object. Any text outside the JSON object will be rejected as a parsing failure.`
}

// userPrompt builds the per-call message. It's structured as a JSON
// dump under stable keys so the model can navigate it
// deterministically — the keys are deliberately the same names you
// see in DecisionInput so a debugger can match prompt ↔ Go field.
func userPrompt(input DecisionInput) string {
	payload := struct {
		FundID              string                       `json:"fundId"`
		TradingDate         string                       `json:"tradingDate"`
		Market              string                       `json:"market"`
		BaseCurrency        string                       `json:"baseCurrency"`
		PrimaryDirection    string                       `json:"primaryDirection,omitempty"`
		Benchmark           string                       `json:"benchmark,omitempty"`
		TotalAssets         float64                      `json:"totalAssets"`
		AvailableCash       float64                      `json:"availableCash"`
		Positions           []DecisionPosition           `json:"positions"`
		Universe            []string                     `json:"universe"`
		InstrumentHints     map[string]InstrumentHint    `json:"instrumentHints,omitempty"`
		RoundtableConsensus []string                     `json:"roundtableConsensus"`
		RoundtableDebate    *roundtableDebatePrompt      `json:"roundtableDebate,omitempty"`
		MacroBriefing       string                       `json:"macroBriefing,omitempty"`
		StockReports        []string                     `json:"stockReports,omitempty"`
		FundamentalSummary  string                       `json:"fundamentalSummary,omitempty"`
		SectorRotation      string                       `json:"sectorRotation,omitempty"`
		NewsSentiment       string                       `json:"newsSentiment,omitempty"`
		SleeveScorecard     string                       `json:"sleeveScorecard,omitempty"`
		LessonReplay        string                       `json:"lessonReplay,omitempty"`
		QuantSnapshots      []quantSnapshotPromptItem    `json:"quantSnapshots,omitempty"`
		UniverseRanking     []universeRankingPromptItem  `json:"universeRanking,omitempty"`
		BuyBudget           float64                      `json:"buyBudget,omitempty"`
		RiskNotes           []string                     `json:"riskNotes,omitempty"`
	}{
		FundID:              input.FundID,
		TradingDate:         input.TradingDate.Format("2006-01-02"),
		Market:              input.Market,
		BaseCurrency:        input.BaseCurrency,
		PrimaryDirection:    input.PrimaryDirection,
		Benchmark:           input.Benchmark,
		TotalAssets:         input.TotalAssets,
		AvailableCash:       input.AvailableCash,
		Positions:           input.Positions,
		Universe:            input.Universe,
		InstrumentHints:     input.InstrumentHints,
		RoundtableConsensus: input.RoundtableConsensus,
		RoundtableDebate:    buildRoundtableDebatePrompt(input),
		MacroBriefing:       input.MacroBriefing,
		StockReports:        input.StockReports,
		FundamentalSummary:  strings.TrimSpace(input.FundamentalSummary),
		SectorRotation:      strings.TrimSpace(input.SectorRotation),
		NewsSentiment:       strings.TrimSpace(input.NewsSentiment),
		SleeveScorecard:     strings.TrimSpace(input.SleeveScorecard),
		LessonReplay:        strings.TrimSpace(input.LessonReplay),
		QuantSnapshots:      buildQuantSnapshotPromptItems(input.QuantSnapshots),
		UniverseRanking:     buildUniverseRankingPromptItems(input.UniverseRanking),
		BuyBudget:           input.BuyBudget,
		RiskNotes:           input.RiskNotes,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Sprintf("INPUT (json marshal failed: %v):\n%+v", err, input)
	}
	return fmt.Sprintf("INPUT:\n%s\n\nReturn the JSON object now.", strings.TrimSpace(string(encoded)))
}

// roundtableDebatePrompt is the on-the-wire shape we expose to the LLM
// when the Phase 2B debate ran. Keeping it separate from
// DecisionInput keeps the Go type honest about which fields are
// optional and makes it easy to omit the whole block from the prompt
// (omitempty on the pointer) when the legacy text-concat consensus
// produced the roundtable.
type roundtableDebatePrompt struct {
	Stance    string                       `json:"stance,omitempty"`
	BullCase  string                       `json:"bullCase,omitempty"`
	BearCase  string                       `json:"bearCase,omitempty"`
	QuantCase string                       `json:"quantCase,omitempty"`
	Symbols   []roundtableSymbolPromptItem `json:"symbols,omitempty"`
}

type roundtableSymbolPromptItem struct {
	Symbol       string `json:"symbol"`
	Verdict      string `json:"verdict"`
	BullCase     string `json:"bullCase,omitempty"`
	BearCase     string `json:"bearCase,omitempty"`
	QuantCase    string `json:"quantCase,omitempty"`
	DissentVotes int    `json:"dissentVotes"`
}

// quantSnapshotPromptItem is the on-the-wire shape we expose for the
// per-symbol regime + ATR + position-size ceiling block. It mirrors
// quantsnapshot.Snapshot but trims down to LLM-friendly precision so
// the prompt doesn't bloat with 14-decimal float noise. Rounding to
// 6 dp keeps the numbers stable across runs (the prompt scrubber
// hashes the prompt for the prompt-injection regression test) while
// still giving the LLM enough resolution to discriminate a 1.4%
// daily ATR from a 1.6% one.
type quantSnapshotPromptItem struct {
	Symbol                 string  `json:"symbol"`
	Regime                 string  `json:"regime,omitempty"`
	Close                  float64 `json:"close,omitempty"`
	ATR14                  float64 `json:"atr14,omitempty"`
	ATRPct                 float64 `json:"atrPct,omitempty"`
	PositionSizeCeilingPct float64 `json:"positionSizeCeilingPct,omitempty"`
}

// buildQuantSnapshotPromptItems drops Snapshots that carry no
// usable signal and rounds the surviving fields to 6 dp.
// HasSignal short-circuits when only the Symbol is populated —
// without this guard the prompt would carry one inert row per
// universe symbol on a fund whose OHLC fetcher isn't wired yet.
func buildQuantSnapshotPromptItems(snapshots []SymbolQuantSnapshot) []quantSnapshotPromptItem {
	if len(snapshots) == 0 {
		return nil
	}
	out := make([]quantSnapshotPromptItem, 0, len(snapshots))
	for _, s := range snapshots {
		if !s.HasSignal() {
			continue
		}
		out = append(out, quantSnapshotPromptItem{
			Symbol:                 s.Symbol,
			Regime:                 s.Regime,
			Close:                  round6(s.Close),
			ATR14:                  round6(s.ATR14),
			ATRPct:                 round6(s.ATRPct),
			PositionSizeCeilingPct: round6(s.PositionSizeCeilingPct),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// round6 trims a float to 6 decimal places. We avoid math.Round on
// negative numbers (none expected here, ATR + close are always ≥ 0)
// and on NaN/Inf (Snapshot rejects those before reaching here).
func round6(v float64) float64 {
	const scale = 1e6
	return float64(int64(v*scale+0.5)) / scale
}

// round4Signed is the round-to-4dp variant for cross-sectional
// z-scores, which can be negative (a Q4-bottom symbol's MomentumZ
// is meaningfully negative). The PM prompt only needs 4dp to
// distinguish Q1 from Q4 by composite score; trimming further keeps
// the prompt JSON small on universes of 20+ symbols.
func round4Signed(v float64) float64 {
	const scale = 1e4
	if v < 0 {
		return -float64(int64(-v*scale+0.5)) / scale
	}
	return float64(int64(v*scale+0.5)) / scale
}

// universeRankingPromptItem is the on-the-wire shape for the Sprint
// A #2 cross-sectional table. Mirrors decision.SymbolRanking but
// rounds the z-scores so the prompt JSON stays diff-friendly.
type universeRankingPromptItem struct {
	Symbol      string  `json:"symbol"`
	MomentumZ   float64 `json:"momentumZ"`
	VolatilityZ float64 `json:"volatilityZ"`
	LiquidityZ  float64 `json:"liquidityZ"`
	CompositeZ  float64 `json:"compositeZ"`
	Quartile    int     `json:"quartile"`
}

// buildUniverseRankingPromptItems mirrors buildQuantSnapshotPromptItems:
// it drops empty inputs and trims the surviving floats so the prompt
// is deterministic across runs.
func buildUniverseRankingPromptItems(rows []SymbolRanking) []universeRankingPromptItem {
	if len(rows) == 0 {
		return nil
	}
	out := make([]universeRankingPromptItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, universeRankingPromptItem{
			Symbol:      r.Symbol,
			MomentumZ:   round4Signed(r.MomentumZ),
			VolatilityZ: round4Signed(r.VolatilityZ),
			LiquidityZ:  round4Signed(r.LiquidityZ),
			CompositeZ:  round4Signed(r.CompositeZ),
			Quartile:    r.Quartile,
		})
	}
	return out
}

func buildRoundtableDebatePrompt(input DecisionInput) *roundtableDebatePrompt {
	if strings.TrimSpace(input.RoundtableStance) == "" &&
		strings.TrimSpace(input.BullCase) == "" &&
		strings.TrimSpace(input.BearCase) == "" &&
		strings.TrimSpace(input.QuantCase) == "" &&
		len(input.SymbolVerdicts) == 0 {
		return nil
	}
	out := &roundtableDebatePrompt{
		Stance:    input.RoundtableStance,
		BullCase:  input.BullCase,
		BearCase:  input.BearCase,
		QuantCase: input.QuantCase,
	}
	for _, sd := range input.SymbolVerdicts {
		out.Symbols = append(out.Symbols, roundtableSymbolPromptItem{
			Symbol:       sd.Symbol,
			Verdict:      sd.Verdict,
			BullCase:     sd.BullCase,
			BearCase:     sd.BearCase,
			QuantCase:    sd.QuantCase,
			DissentVotes: sd.DissentVotes,
		})
	}
	return out
}
