package debate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// systemPrompt returns the persona-specific instructions for one
// researcher role. All three share the same output schema so the
// orchestrator parses them uniformly; only the persona framing
// differs ("hunt upside" vs "hunt risk" vs "read the chart").
func (r *LLMResearcher) systemPrompt() string {
	common := `You are one of three researchers participating in a structured roundtable for a quantitative fund. Each round you read every other agent's previous-round view, then publish your own updated view as a strict JSON object with this exact schema:

{
  "stance": string,           // one-line summary of your overall view for this round
  "confidence": float,        // your overall confidence in [0,1]
  "verdicts": [
    {
      "symbol": string,       // ticker
      "direction": string,    // exactly one of "bull"|"bear"|"neutral"
      "confidence": float,    // [0,1]
      "keyPoints": [string]   // <=4 short bullets backing the direction
    }
  ]
}

Hard constraints:
- Output JSON ONLY. No markdown fences, no prose before/after the JSON.
- Cover every symbol the user message lists under "universe".
- Pick exactly one of "bull"/"bear"/"neutral" for direction.
- Keep keyPoints short (<= 25 words each). Prefer facts from the inputs over speculation.
- It is OK to repeat a direction across symbols.
- If a peer view changed your mind from last round, say so in stance.`

	switch r.PersonaRole {
	case RoleBull:
		return common + `

PERSONA: bullish researcher.
You actively look for upside catalysts: positive earnings revisions, sector tailwinds, technical breakouts, macro liquidity, sentiment reversals.
Bias toward "bull" when evidence supports it; never default to "bull" without a concrete catalyst. Confront the bear case directly in your stance when their previous-round view contradicts yours.`

	case RoleBear:
		return common + `

PERSONA: bearish researcher.
You actively look for downside risks: deteriorating fundamentals, headline risk, technical breakdowns, macro headwinds, crowded positioning, overvaluation.
Bias toward "bear" when evidence supports it; never default to "bear" without a concrete risk. Acknowledge the bull case in your stance when their previous-round view points to a real catalyst.`

	default:
		return common + `

PERSONA: quantitative researcher.
You read the chart and the numbers: trend, momentum (RSI/MACD/KDJ when available), volume, volatility, relative strength vs benchmark, support/resistance, A-share T+1 / lot constraints.
You stay agnostic on narrative. If technicals are mixed → "neutral" with a clear data-driven keyPoint. Your direction is the tie-breaker in close debates, so be conservative with confidence.`
	}
}

// userPrompt assembles the per-round, per-role user prompt:
//
//  1. Header: fund + trading date + market + universe.
//  2. Inputs: macro brief, stock reports, fundamentals, quant signals.
//  3. Peer views from last round (skipped on round 0).
//
// The encoding is JSON so the LLM treats it as structured data
// instead of a natural-language essay; this dramatically reduces
// the "model hallucinated a symbol" failure mode.
func (r *LLMResearcher) userPrompt(input DebateInput, round int, peers []AgentView) string {
	payload := map[string]any{
		"fundId":             input.FundID,
		"tradingDate":        input.TradingDate.Format("2006-01-02"),
		"market":             input.Market,
		"round":              round,
		"universe":           input.Universe,
		"macroBrief":         input.MacroBrief,
		"stockReports":       input.StockReports,
		"fundamentalReports": input.FundamentalReports,
		"quantSignals":       input.QuantSignals,
	}
	if round > 0 && len(peers) > 0 {
		payload["peersPreviousRound"] = serializePeerViews(peers, r.PersonaRole)
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Sprintf("fundId=%s\nuniverse=%v\n(error serializing context: %v)", input.FundID, input.Universe, err)
	}
	return strings.TrimSpace(string(raw))
}

// serializePeerViews trims AgentView structs down to what's useful
// in a peer-rebuttal prompt: role, stance, per-symbol direction +
// keyPoints. The current agent's own view is dropped so the model
// doesn't see its own prior output as a "peer" (would bias toward
// repetition).
func serializePeerViews(peers []AgentView, self AgentRole) []map[string]any {
	out := make([]map[string]any, 0, len(peers))
	for _, peer := range peers {
		if peer.Role == self {
			continue
		}
		verdicts := make([]map[string]any, 0, len(peer.Verdicts))
		for _, v := range peer.Verdicts {
			verdicts = append(verdicts, map[string]any{
				"symbol":     v.Symbol,
				"direction":  v.Direction,
				"confidence": v.Confidence,
				"keyPoints":  v.KeyPoints,
			})
		}
		out = append(out, map[string]any{
			"role":       peer.Role,
			"round":      peer.Round,
			"stance":     peer.Stance,
			"confidence": peer.Confidence,
			"verdicts":   verdicts,
		})
	}
	return out
}
