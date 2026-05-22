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

5. Locale: write reasoning text in the same language the input MacroBriefing / RoundtableConsensus uses (Chinese ⇄ English). If the input is empty or mixed, default to Chinese.

Return only the JSON object. Any text outside the JSON object will be rejected as a parsing failure.`
}

// userPrompt builds the per-call message. It's structured as a JSON
// dump under stable keys so the model can navigate it
// deterministically — the keys are deliberately the same names you
// see in DecisionInput so a debugger can match prompt ↔ Go field.
func userPrompt(input DecisionInput) string {
	payload := struct {
		FundID              string                    `json:"fundId"`
		TradingDate         string                    `json:"tradingDate"`
		Market              string                    `json:"market"`
		BaseCurrency        string                    `json:"baseCurrency"`
		PrimaryDirection    string                    `json:"primaryDirection,omitempty"`
		Benchmark           string                    `json:"benchmark,omitempty"`
		TotalAssets         float64                   `json:"totalAssets"`
		AvailableCash       float64                   `json:"availableCash"`
		Positions           []DecisionPosition        `json:"positions"`
		Universe            []string                  `json:"universe"`
		InstrumentHints     map[string]InstrumentHint `json:"instrumentHints,omitempty"`
		RoundtableConsensus []string                  `json:"roundtableConsensus"`
		RoundtableDebate    *roundtableDebatePrompt   `json:"roundtableDebate,omitempty"`
		MacroBriefing       string                    `json:"macroBriefing,omitempty"`
		StockReports        []string                  `json:"stockReports,omitempty"`
		FundamentalSummary  string                    `json:"fundamentalSummary,omitempty"`
		SectorRotation      string                    `json:"sectorRotation,omitempty"`
		NewsSentiment       string                    `json:"newsSentiment,omitempty"`
		SleeveScorecard     string                    `json:"sleeveScorecard,omitempty"`
		LessonReplay        string                    `json:"lessonReplay,omitempty"`
		BuyBudget           float64                   `json:"buyBudget,omitempty"`
		RiskNotes           []string                  `json:"riskNotes,omitempty"`
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
