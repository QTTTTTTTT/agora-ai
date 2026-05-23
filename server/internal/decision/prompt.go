package decision

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
   - When input.qualityScores is present (Sprint E #3 cross-sectional quality factor): every row carries profitabilityZ / growthZ / safetyZ + a single compositeZ + a quartile bucket (1 = top, 4 = bottom) + componentsAvailable (1..3 — how many of the three sub-factors actually had data). Quality scores are FUNDAMENTAL, where universeRanking is PRICE-driven; the two are intentionally orthogonal. Use the table to layer quality on top of the momentum-based ranking:
       - The highest-conviction long setup is "Q1 universeRanking AND Q1 qualityScores" — the AQR / GMO "Quality at a Reasonable Price" overlay. When both are Q1 you may size at the per-symbol ceiling without requiring extra debate corroboration.
       - When universeRanking is Q1 but qualityScores is Q4, the rally is happening but the underlying business is junk — common late-cycle behaviour. Cap qtyPct at HALF the per-symbol ceiling and lean toward "watch" unless the bullCase explicitly names a catalyst (a turnaround, an acquisition rumour) that justifies the divergence.
       - When qualityScores is Q1 but universeRanking is Q4, the business is sound but the market is punishing it — common deep-value setup. You may take a half-ceiling position only if the regime is range / chop (the trend hasn't broken) AND the bearCase doesn't carry a concrete deterioration thesis. Otherwise treat as "watch".
       - Use the sub-factor decomposition to color the verdict: a positive compositeZ driven purely by safetyZ (low debt) is NOT the same as one driven by profitabilityZ (real ROE). When citing the quality block in your reasoning, name which sub-factor is doing the work ("qualityScores compositeZ=+1.1 driven mostly by profitabilityZ=+1.5; growth and safety roughly neutral").
       - componentsAvailable=1 means the score is built on a single sub-factor — discount the signal accordingly. A "Q1 quality" verdict built only on safetyZ is materially weaker than one built on all three.
       - QualityScores are slow-moving (fundamentals change quarterly). Don't expect day-over-day shifts in this block; if you see one, you're looking at a coverage change (a new symbol joined the universe with strong / weak data) rather than a real fundamental move.
       - When the block is absent treat it as "no fundamental coverage today, lean on the FundamentalSummary text and universeRanking alone". Do NOT infer "low quality" from absence.
   - When input.quantSnapshots is present (Sprint A regime + volatility prior): each entry carries that symbol's regime (trend_up/trend_down/range/chop), the 14-bar ATR in price units (atr14), the same ATR as a percentage of close (atrPct), and an explicit positionSizeCeilingPct. Apply these as a per-symbol filter on top of every other signal:
       - positionSizeCeilingPct is the UPPER BOUND on any qtyPct you assign to a buy or add for that symbol. If you want to size larger you must drop to the ceiling. The number is derived from a 50-bps-per-trade risk budget at a 2× ATR stop, clamped into [0.005, 0.10]; it already respects rule 3's 10% single-order cap so you do NOT need to add your own buffer on top.
       - If regime is "chop", treat any buy/add as exceptional: only propose it when the debate verdict on that symbol is bull AND the news / fundamental block carries a concrete near-term catalyst. Otherwise downgrade to "watch". Chop kills both trend and mean-reversion sleeves; the historical attribution scorecard usually shows red here.
       - If regime is "trend_down" on a candidate buy/add, the default is "watch" unless dissentVotes on the debate verdict is 0 AND the bull case names a specific reversal trigger. Sizing against a trend_down regime is the most common loser in the lesson replay history.
       - If regime is "trend_up" on a held position you are about to reduce or sell, cap the qtyPct of the reduce at 0.3 unless the reasoning field cites a specific named risk (earnings catalyst, sector rotation reversal, position-level stop hit). Trend-up regimes carry their own positive expectancy; cutting them prematurely is the most common loser when the operator runs the post-trade review.
       - If regime is "range", mean-reversion sized at the snapshot ceiling is fine; trend-style adds should be sized at half the ceiling because range regimes have low realised follow-through.
       - If a symbol is in input.universe / input.positions but has no quantSnapshots entry, the snapshot pipeline either had no bars (newly listed / illiquid) or the regime classifier returned Unknown. In that case treat the symbol as "no quant prior", default to "watch" for first-time buys, and lean on the debate + fundamental signals for held positions.
       - Cite the snapshot field explicitly in your reasoning when it changes the action ("atrPct=4.8% pushes ceiling to 0.026; sizing the AAPL buy at qtyPct=0.025 to respect the cap" or "regime=chop on TSLA so demoting the bull debate to watch").
   - When input.cooldowns is present (Sprint B event-driven re-entry locks): every row tells you that THIS fund executed a fill on that symbol within the cooldown window (default 24h) and the symbol is now locked from re-entry. Apply these as a hard veto on flipping the same name:
       - If a symbol appears in cooldowns, the default action MUST be "watch". Override only when there is a concrete extreme catalyst that wasn't available at the time of the last fill (e.g. an after-hours earnings miss, an M&A announcement, a regulatory halt notice) — and you must name the catalyst in your reasoning field.
       - When you override, you may propose "reduce" (not "add" / "buy") if the cooldown.lastFillSide is "buy" AND the catalyst is bearish, or "buy" only if lastFillSide is "sell" AND the catalyst is structurally bullish. Same-side re-entry (a second buy on a name you just bought, or a second sell on a name you just sold) is almost always wrong inside the cooldown window and the burden of proof is on the override.
       - hoursRemaining tells you how tight the lock is. If hoursRemaining > 12 the lock is still strong (we're in the first half of the window) and the bar for override is highest; if hoursRemaining < 6 the lock is about to expire and you may stand pat with a "watch" + a one-line note that the lock will clear by tomorrow.
       - The auto-execute gateway does NOT enforce cooldown — it is your responsibility to honour it in the plan. Symbols not in the cooldowns block are unconstrained by this rule.
       - Cite the cooldown row explicitly when it changes your action ("AAPL filled 8h ago (buy), hoursRemaining=16 → forcing watch; no fresh catalyst since the entry").
   - When input.riskBudget is present (Sprint B dynamic risk budget): the block carries the fund's realised annualised vol, the configured vol target, the resulting volScalar (clamp(target/realised, 0.5, 2.0)), the running peak/current NAV pair, the drawdownPct, the ddScalar (clamp(1 - dd/ceiling, 0.4, 1.0)), and the effectivePerTradeRiskPct = basePerTradeRiskPct × volScalar × ddScalar. Use this snapshot to right-size every buy / add:
       - Treat effectivePerTradeRiskPct as the FUND-LEVEL R-per-trade budget for today. When you choose a qtyPct for a buy / add it should be consistent with this R (no need to do the ATR math yourself — the per-symbol positionSizeCeilingPct already incorporates the baseline R; if effectiveR < base you must scale qtyPct DOWN proportionally to (effectiveR / baseR)).
       - If volScalar > 1.0 (realised vol below target) the fund is under-deploying. You may size at the per-symbol ceiling and consider one extra position from the high-quality watchlist, BUT only when the regime + ranking + cooldown signals all align. The vol overlay does NOT change your discipline on chop / Q4 / cooldown names.
       - If volScalar < 1.0 (realised vol above target) the fund is over-its-skis. Cap every qtyPct at the per-symbol ceiling × volScalar and prefer "reduce" actions on high-volatilityZ holders before any new buy is considered.
       - If ddScalar < 1.0 (the fund is in drawdown) the throttle is engaged. Drop your typical position-count target by one (e.g. instead of 4 new buys, pick 3) and demand at least 0.7 confidence on any buy / add you do propose. When ddScalar is at the 0.4 floor (≥ ddCeiling drawdown) the default action is "watch" across the board unless a candidate has BOTH Q1 ranking AND a debate bullCase with zero dissent.
       - The riskBudget snapshot is fund-wide, not per-symbol. Symbols you've decided to "reduce" / "sell" for risk reasons aren't constrained by this rule — the throttle is about NEW exposure.
       - Cite the snapshot when it materially changes sizing ("riskBudget.ddScalar=0.55 forces qtyPct=0.025 instead of the 0.05 ATR ceiling; fund is in 12% drawdown").
   - When input.newsCatalysts is present (Sprint B per-symbol catalyst recall): each entry is one universe / position symbol with 1..K (default 3) recent news hits, ordered most-recent first. hoursOld tells you how stale a hit is; publishedAt is the RFC-3339 timestamp; source and language let you weigh credibility / locale relevance. Use the block as the contextual gate that supplements the debate verdict:
       - A hit with hoursOld <= 48 is "fresh" — treat it as material new information. If a fresh hit explicitly contradicts the debate bullCase (a buy candidate with a fresh earnings miss / downgrade / litigation headline), downgrade the action by one notch (buy → watch, add → hold) unless you can name a concrete reason the headline is already priced in.
       - A fresh hit that REINFORCES the debate verdict (buy candidate + fresh positive guidance / contract win / regulatory approval) lets you keep your sizing at the per-symbol ceiling — but never above it. News alone is never sufficient to override the riskBudget or quantSnapshot ceilings; it only restores conviction within the existing budget.
       - Stale hits (hoursOld > 48) are background context. Use them to discount the freshness of any "breaking" claim in your own reasoning, not as the trigger for an action.
       - When a symbol has no entry in newsCatalysts but does appear in input.universe / input.positions, treat it as "no actionable catalyst window" — fall back entirely on the debate / fundamental / sector / quant blocks. Absence here is NOT a signal in itself.
       - The block is symbol-level, not fund-level: a fresh macro headline on AAPL does NOT change your TSLA sizing unless TSLA also has its own newsCatalysts entry referencing the same theme.
       - Cite the hit explicitly when it changes your action ("AAPL: 3h Reuters 'Q4 guidance cut' → downgrading the bull buy to watch").
   - When input.earningsCalendar is present (Sprint E #2 scheduled earnings catalysts): the block carries the next upcoming earnings release per symbol inside horizonDays (default 14), with daysUntil (0=today, 1=tomorrow, …), timeOfDay (bmo/amc/unknown), and the source provider. Earnings dates are HARD catalysts in a way news is not — they are scheduled, the gap is structural, and the IV / borrow / liquidity around the date all shift before the report:
       - Never OPEN a fresh long position (action=buy on a name with current weight = 0) on a symbol with daysUntil <= 2 unless the debate's bullCase ALSO references the earnings event by name AND the news / fundamental block carries a concrete numeric catalyst (specific guidance, channel checks, an explicit pre-print). Default action in that window is "watch" with the reasoning citing the earnings date.
       - For ADDS to existing positions (action=add) inside the daysUntil <= 2 window: halve qtyPct vs the per-symbol ceiling. The catalyst risk is asymmetric — a bad print is a -10% gap, a good one is +5%.
       - For REDUCES / SELLS on names with daysUntil <= 2: prefer them. The catalyst is the most common single-day P&L outlier in long-only funds; trimming risk into a known event is the AQR / Renaissance default.
       - timeOfDay=amc means the price-impacting open is daysUntil+1 (the report drops after today's close and the gap is tomorrow morning). Adjust the gating window accordingly when relevant.
       - When a symbol you'd otherwise buy has daysUntil in (2, 7], you may proceed but at most 0.5× the per-symbol ceiling. The risk grows non-linearly into the date; this halving keeps you in the trade without paying full freight on the catalyst.
       - When a symbol does NOT appear in earningsCalendar but does appear in input.universe / input.positions, treat it as "no scheduled catalyst in horizon" — the block carries no negative signal by its silence.
       - Cite the row when it changes your action ("AAPL earnings T+1 AMC → downgrading buy to watch; planned entry resumes T+3 unless the bear case has been refreshed by the print").
   - When input.exposure is present (Sprint C portfolio concentration check): the block carries the current per-symbol / per-sector weights, the top-3 cluster weight, cash%, the configured caps (singleNameCap, sectorCap, top3Cap, cashFloorPct), and a Breaches list. Treat the snapshot as a hard fund-level guardrail — concentration breaches are the single most common cause of catastrophic drawdowns in long-only funds:
       - Every entry in breaches MUST be honoured. If a breach line names a symbol or sector, you must NOT propose "buy" or "add" on that symbol or any other symbol in the same sector for this plan. The only valid actions on a breaching bucket are "watch", "hold", "reduce", "sell".
       - When a candidate buy would push a bucket over its cap (current + your proposed qtyPct > cap), refuse the buy or shrink qtyPct so the post-trade weight stays ≤ cap. The arithmetic: post-buy weight ≈ singleName.weight + qtyPct; do not propose qtyPct values that would make that sum exceed singleNameCap.
       - Sector caps work the same way but on the aggregated sector weight: if input.exposure.sectorWeights shows tech=0.45 and sectorCap=0.50, any new tech buy must keep tech weight ≤ 0.50.
       - top3Weight + top3Cap captures cluster risk. When top3Weight > top3Cap (or your proposed buy would push it past) the default action is "watch" on any name that would join the top-3 — even if the per-symbol bucket has room.
       - cashPct vs cashFloorPct: when cashPct < cashFloorPct (even before any new buy), the only buy you may propose this session is one funded by an equal-or-larger "reduce" elsewhere in the same plan. If the plan only contains "buy" actions you must demote them all to "watch" with a one-line note that the cash floor is being honoured. cashFloorPct=0 means "no floor enforced" and this rule doesn't apply.
       - When the breaches list is empty AND no candidate buy would create a fresh breach, the exposure block contributes no friction; size as the other blocks indicate.
       - Cite the breach line verbatim in your reasoning when it caps an action ("exposure breach 'BREACH: sector=tech weight=52.0% > cap=50.0%' forces TSLA watch instead of buy").
   - When input.correlations is present (Sprint C pairwise correlation matrix): the block carries highCorrPairs (|rho| >= highCorrThreshold, default 0.7), candidateSummaries (each non-held universe symbol's worst correlation against any held name), and heldCluster (avg / max pairwise inside the held set). Correlation is the missing dimension on top of per-symbol R and sector caps — two "different" names can still create a single hidden bet:
       - For each candidate buy, look up its row in candidateSummaries. If maxAbsRho >= highCorrThreshold AND maxRho is POSITIVE, the candidate is effectively a duplicate of the named target (maxAbsTarget). Either refuse the buy or halve qtyPct vs the per-symbol ceiling, citing the correlation. Negative correlation at the same magnitude is a hedge — do NOT halve; you may keep ceiling sizing because the candidate diversifies risk away from the target.
       - When heldCluster.avgPairwise >= 0.6 (the held book is already tightly correlated), prefer candidates whose maxRho is < 0.5 OR negative; reject otherwise-attractive buys that would tighten the cluster further.
       - When heldCluster.maxPairwise >= highCorrThreshold, name the (maxLeft, maxRight) pair in your reasoning when proposing any reduce on either side — the cluster is already concentrated and trimming one of the two reduces effective book risk by more than the position size suggests.
       - highCorrPairs is a flat per-pair list. Use it as background colour: a Q1 candidate that appears in highCorrPairs alongside a held name should be sized with the same correlation-aware halving rule as the candidate summary case.
       - The block is intentionally lookback-bounded (default 60 daily bars); correlations decay over time so a fresh negative shock (a sector rotation, an earnings cluster) may not yet show up. Treat the matrix as a soft prior, not a forecast.
       - Cite the relevant correlation row when it materially changes sizing ("correlations.candidateSummary AMD maxRho=0.82 vs NVDA → halving the AMD buy qtyPct from 0.05 to 0.025").
   - When input.pairSpreads is present (Sprint E #4 pair-spread monitor): the block carries the top-K high-correlation pairs (sorted descending by |spreadZ|) with their last log(left/right) spread, the rolling mean / stdev over the same lookback window the correlation block used (default 60 daily bars), the spreadZ itself, the upstream rho, and the configured zThreshold (default 2.0). The block is layered ON TOP of the correlation matrix: correlations tells you what's TIGHT, pairSpreads tells you what's currently EXTENDED on top of that tightness:
       - |spreadZ| < 1: the pair is trading near its long-run ratio. No special action — both legs participate normally according to the per-symbol blocks.
       - |spreadZ| in [1, 2): mild divergence. When sizing a NEW position on either leg, cap qtyPct at the per-symbol ceiling (do not size up) and prefer the leg with the more favourable z-sign for the side: a NEGATIVE spreadZ means the LEFT leg is cheap → mild preference for adding LEFT or reducing RIGHT (if held); positive means LEFT is rich → mild preference for adding RIGHT or reducing LEFT.
       - |spreadZ| ≥ zThreshold (default 2.0): 2-σ divergence — actionable. The cheap leg (negative z) is a candidate for ADD at the per-symbol ceiling; the rich leg (positive z) is a candidate for REDUCE on existing holdings even when the per-symbol blocks alone wouldn't justify it. Mean reversion of a 2σ pairs trade is one of the highest-Sharpe academic anomalies known, but ONLY when both legs are otherwise sound — never use pairSpreads alone to override a hard guardrail (cooldown, exposure breach, fresh earnings catalyst contradicting the bullCase).
       - Long-only constraint: this fund cannot short. A "rich leg" reduce only makes sense if the leg is actually held; otherwise the only actionable signal in a divergence is "ADD the cheap leg" — and even that requires the cheap leg's per-symbol verdict to be neutral-or-better. A 2σ divergence on a clearly broken company (bear quantCase + fresh negative earnings catalyst) is NOT a buy.
       - Don't double-fire: if a candidate buy is already supported by the per-symbol blocks (Q1 ranking, Q1 quality, supportive debate), a divergent pairSpread is corroboration — don't size BEYOND the per-symbol ceiling. The pairs signal complements the per-symbol R, never bypasses it.
       - When pairSpreads is present but every entry has |spreadZ| < zThreshold, the prompt block IS still rendered (so you can see the most-extended observed pair) but you should NOT treat it as actionable — it's context.
       - When pairSpreads is absent treat it as "no extended pairs in the watched correlation universe today" — silence is not a buy or sell signal.
       - Cite the row when it changes an action ("pairSpreads NVDA/AMD spreadZ=-2.3, rho=0.85 → adding the cheap AMD leg at full ceiling and trimming NVDA from full size to half ceiling on next rebalance").

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
		QualityScores       []qualityScorePromptItem     `json:"qualityScores,omitempty"`
		Cooldowns           []cooldownPromptItem         `json:"cooldowns,omitempty"`
		RiskBudget          *riskBudgetPromptItem        `json:"riskBudget,omitempty"`
		NewsCatalysts       []newsCatalystPromptItem     `json:"newsCatalysts,omitempty"`
		EarningsCalendar    *earningsCalendarPromptItem  `json:"earningsCalendar,omitempty"`
		Exposure            *exposurePromptItem          `json:"exposure,omitempty"`
		Correlations        *correlationsPromptItem      `json:"correlations,omitempty"`
		PairSpreads         *pairSpreadsPromptItem       `json:"pairSpreads,omitempty"`
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
		QualityScores:       buildQualityScorePromptItems(input.QualityScores),
		Cooldowns:           buildCooldownPromptItems(input.Cooldowns),
		RiskBudget:          buildRiskBudgetPromptItem(input.RiskBudget),
		NewsCatalysts:       buildNewsCatalystPromptItems(input.NewsCatalysts),
		EarningsCalendar:    buildEarningsCalendarPromptItem(input.EarningsCalendar),
		Exposure:            buildExposurePromptItem(input.Exposure),
		Correlations:        buildCorrelationsPromptItem(input.Correlations),
		PairSpreads:         buildPairSpreadsPromptItem(input.PairSpreads),
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

// qualityScorePromptItem is the on-the-wire shape for the Sprint
// E #3 cross-sectional quality factor table. Mirrors
// decision.SymbolQualityScore but rounds the z-scores so the
// prompt JSON stays diff-friendly across runs.
//
// componentsAvailable lets the LLM discount a Q1 verdict that's
// built on only a single sub-factor — see the system prompt's
// "componentsAvailable=1 means …" rule.
type qualityScorePromptItem struct {
	Symbol              string  `json:"symbol"`
	ProfitabilityZ      float64 `json:"profitabilityZ"`
	GrowthZ             float64 `json:"growthZ"`
	SafetyZ             float64 `json:"safetyZ"`
	CompositeZ          float64 `json:"compositeZ"`
	Quartile            int     `json:"quartile,omitempty"`
	ComponentsAvailable int     `json:"componentsAvailable"`
}

// buildQualityScorePromptItems is the analogue of
// buildUniverseRankingPromptItems for the quality block. Returns
// nil on an empty input so the prompt omits the block entirely.
func buildQualityScorePromptItems(rows []SymbolQualityScore) []qualityScorePromptItem {
	if len(rows) == 0 {
		return nil
	}
	out := make([]qualityScorePromptItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, qualityScorePromptItem{
			Symbol:              r.Symbol,
			ProfitabilityZ:      round4Signed(r.ProfitabilityZ),
			GrowthZ:             round4Signed(r.GrowthZ),
			SafetyZ:             round4Signed(r.SafetyZ),
			CompositeZ:          round4Signed(r.CompositeZ),
			Quartile:            r.Quartile,
			ComponentsAvailable: r.ComponentsAvailable,
		})
	}
	return out
}

// cooldownPromptItem is the on-the-wire shape for the Sprint B #1
// per-symbol re-entry lock. Mirrors decision.SymbolCooldown but
// renders the timestamps as RFC-3339 strings (the LLM handles
// RFC-3339 much better than Go's default time encoding) and rounds
// the hour counts to a single decimal so the prompt reads "filled
// 8.3h ago" rather than "8.27845h".
type cooldownPromptItem struct {
	Symbol         string  `json:"symbol"`
	LastFillSide   string  `json:"lastFillSide,omitempty"`
	LastFillAt     string  `json:"lastFillAt,omitempty"`
	BlockedUntil   string  `json:"blockedUntil,omitempty"`
	HoursSinceFill float64 `json:"hoursSinceFill"`
	HoursRemaining float64 `json:"hoursRemaining"`
}

// buildCooldownPromptItems renders a slice of SymbolCooldown into
// the prompt-facing shape. Drops entries with blank Symbol so a
// malformed Lock can't poison the prompt — the cooldown.Service
// already filters these out, but defence in depth is cheap.
func buildCooldownPromptItems(locks []SymbolCooldown) []cooldownPromptItem {
	if len(locks) == 0 {
		return nil
	}
	out := make([]cooldownPromptItem, 0, len(locks))
	for _, l := range locks {
		if strings.TrimSpace(l.Symbol) == "" {
			continue
		}
		item := cooldownPromptItem{
			Symbol:         l.Symbol,
			LastFillSide:   l.LastFillSide,
			HoursSinceFill: roundTenth(l.HoursSinceFill),
			HoursRemaining: roundTenth(l.HoursRemaining),
		}
		if !l.LastFillAt.IsZero() {
			item.LastFillAt = l.LastFillAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		if !l.BlockedUntil.IsZero() {
			item.BlockedUntil = l.BlockedUntil.UTC().Format("2006-01-02T15:04:05Z")
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// roundTenth trims a positive duration-in-hours to one decimal
// place. The cooldown service never produces negative hours, so we
// skip the signed-rounder path universeRanking uses.
func roundTenth(v float64) float64 {
	if v < 0 {
		v = 0
	}
	const scale = 10.0
	return float64(int64(v*scale+0.5)) / scale
}

// riskBudgetPromptItem is the on-the-wire shape for the Sprint B
// #2 dynamic risk-budget throttle. Mirrors riskbudget.Snapshot but
// rounds the percentage fields to 4 dp so the prompt JSON stays
// diff-friendly across runs.
type riskBudgetPromptItem struct {
	Window                   string  `json:"window"`
	SampleSize               int     `json:"sampleSize"`
	BasePerTradeRiskPct      float64 `json:"basePerTradeRiskPct"`
	RealisedVolAnnualized    float64 `json:"realisedVolAnnualized"`
	VolTargetAnnualized      float64 `json:"volTargetAnnualized"`
	VolScalar                float64 `json:"volScalar"`
	PeakNAV                  float64 `json:"peakNav"`
	CurrentNAV               float64 `json:"currentNav"`
	DrawdownPct              float64 `json:"drawdownPct"`
	DDCeilingPct             float64 `json:"ddCeilingPct"`
	DDScalar                 float64 `json:"ddScalar"`
	EffectivePerTradeRiskPct float64 `json:"effectivePerTradeRiskPct"`
}

// buildRiskBudgetPromptItem returns nil when the snapshot is nil so
// the prompt simply omits the riskBudget block (matching every
// other optional prompt-side type). Floats are rounded to 4 dp;
// NAV figures stay at 2 dp because dollar precision is meaningful
// at fund scale.
func buildRiskBudgetPromptItem(snap *SymbolRiskBudgetAlias) *riskBudgetPromptItem {
	if snap == nil {
		return nil
	}
	return &riskBudgetPromptItem{
		Window:                   snap.Window,
		SampleSize:               snap.SampleSize,
		BasePerTradeRiskPct:      round4Signed(snap.BasePerTradeRiskPct),
		RealisedVolAnnualized:    round4Signed(snap.RealisedVolAnnualized),
		VolTargetAnnualized:      round4Signed(snap.VolTargetAnnualized),
		VolScalar:                round4Signed(snap.VolScalar),
		PeakNAV:                  round2(snap.PeakNAV),
		CurrentNAV:               round2(snap.CurrentNAV),
		DrawdownPct:              round4Signed(snap.DrawdownPct),
		DDCeilingPct:             round4Signed(snap.DDCeilingPct),
		DDScalar:                 round4Signed(snap.DDScalar),
		EffectivePerTradeRiskPct: round4Signed(snap.EffectivePerTradeRiskPct),
	}
}

// SymbolRiskBudgetAlias re-exposes RiskBudgetSnapshot under a local
// name so the build helper above doesn't need to import riskbudget
// directly — keeps the prompt.go file's import list minimal. The
// alias is a type alias (=), not a definition, so it costs nothing
// at runtime and the wiring layer / tests can use either name.
type SymbolRiskBudgetAlias = RiskBudgetSnapshot

// round2 trims a non-negative float (NAV) to 2 dp. Negative inputs
// are clamped to 0 — NAVs cannot go negative in a long-only fund
// and the riskbudget service won't pass negatives anyway, but we
// stay defensive.
func round2(v float64) float64 {
	if v < 0 {
		v = 0
	}
	const scale = 100.0
	return float64(int64(v*scale+0.5)) / scale
}

// newsCatalystPromptItem is the on-the-wire shape for the Sprint B
// #3 per-symbol catalyst block. We collapse the Hit slice into a
// flat structure with the title / source / publishedAt / hoursOld
// fields the PM system prompt references — the summary is included
// when present so the LLM can see the actual catalyst, not just a
// headline.
//
// Why no URL: the marketdata fetchers don't always populate URL
// (Sina's older payloads, for example). The PM never needs to open
// the URL, and including it bloats the prompt for no payoff.
// Language is preserved so the PM can ignore CN-only items in an
// EN-mode fund (or vice versa).
type newsCatalystPromptItem struct {
	Symbol string           `json:"symbol"`
	Hits   []newsHitPromptItem `json:"hits"`
}

type newsHitPromptItem struct {
	Title       string  `json:"title"`
	Summary     string  `json:"summary,omitempty"`
	Source      string  `json:"source,omitempty"`
	Language    string  `json:"language,omitempty"`
	PublishedAt string  `json:"publishedAt"`
	HoursOld    float64 `json:"hoursOld"`
}

// buildNewsCatalystPromptItems renders SymbolNewsCatalysts for the
// prompt. Drops empty / blank-symbol entries and entries with no
// hits — these can only appear when an upstream fetcher returns a
// degenerate payload, but we stay defensive so the prompt JSON is
// always well-formed.
//
// Summaries are truncated to keep the prompt small: a 200-char
// snippet is enough for the PM to decide whether the catalyst is
// material; longer than that and the prompt JSON bloats with no
// signal gain. The truncation appends "…" to flag the cut.
func buildNewsCatalystPromptItems(catalysts []SymbolNewsCatalysts) []newsCatalystPromptItem {
	if len(catalysts) == 0 {
		return nil
	}
	out := make([]newsCatalystPromptItem, 0, len(catalysts))
	for _, c := range catalysts {
		if strings.TrimSpace(c.Symbol) == "" || len(c.Hits) == 0 {
			continue
		}
		hits := make([]newsHitPromptItem, 0, len(c.Hits))
		for _, h := range c.Hits {
			title := strings.TrimSpace(h.Title)
			if title == "" {
				continue
			}
			published := ""
			if !h.PublishedAt.IsZero() {
				published = h.PublishedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			hits = append(hits, newsHitPromptItem{
				Title:       title,
				Summary:     truncateSummary(strings.TrimSpace(h.Summary), 200),
				Source:      strings.TrimSpace(h.Source),
				Language:    strings.TrimSpace(h.Language),
				PublishedAt: published,
				HoursOld:    roundTenth(h.HoursOld),
			})
		}
		if len(hits) == 0 {
			continue
		}
		out = append(out, newsCatalystPromptItem{Symbol: c.Symbol, Hits: hits})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// truncateSummary keeps the prompt JSON small. The cutoff is on a
// rune boundary so we don't slice a multi-byte UTF-8 character in
// half (Chinese characters are 3 bytes each — a naive byte cut
// would produce invalid UTF-8 and break the JSON encoder).
func truncateSummary(s string, max int) string {
	if max <= 0 || s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// exposurePromptItem is the on-the-wire shape for the Sprint C
// #1 portfolio exposure check. Mirrors exposure.Snapshot but
// trims to the fields the PM system prompt actually consumes —
// the raw TotalAssets / AvailableCash are already at the top of
// the prompt under their own keys, so we skip them here to keep
// the block focused.
type exposurePromptItem struct {
	PositionCount int                       `json:"positionCount"`
	CashPct       float64                   `json:"cashPct"`
	CashFloorPct  float64                   `json:"cashFloorPct,omitempty"`
	SingleNameCap float64                   `json:"singleNameCap"`
	SectorCap     float64                   `json:"sectorCap"`
	Top3Cap       float64                   `json:"top3Cap"`
	Top3Weight    float64                   `json:"top3Weight"`
	SingleName    []exposureSinglePromptItem  `json:"singleName,omitempty"`
	SectorWeights []exposureSectorPromptItem  `json:"sectorWeights,omitempty"`
	Breaches      []string                  `json:"breaches,omitempty"`
}

type exposureSinglePromptItem struct {
	Symbol string  `json:"symbol"`
	Weight float64 `json:"weight"`
	Breach bool    `json:"breach,omitempty"`
}

type exposureSectorPromptItem struct {
	Sector string  `json:"sector"`
	Weight float64 `json:"weight"`
	Breach bool    `json:"breach,omitempty"`
}

// correlationsPromptItem is the on-the-wire shape for the Sprint
// C #2 pairwise correlation block. We mirror the underlying
// CorrelationSnapshot but rename a couple of fields to read more
// naturally in the prompt JSON.
type correlationsPromptItem struct {
	Window             string                       `json:"window"`
	SampleSize         int                          `json:"sampleSize"`
	HighCorrThreshold  float64                      `json:"highCorrThreshold"`
	HighCorrPairs      []correlationsPairPromptItem `json:"highCorrPairs,omitempty"`
	CandidateSummaries []correlationsCandidatePromptItem `json:"candidateSummaries,omitempty"`
	HeldCluster        *correlationsClusterPromptItem `json:"heldCluster,omitempty"`
}

type correlationsPairPromptItem struct {
	Left  string  `json:"left"`
	Right string  `json:"right"`
	Rho   float64 `json:"rho"`
}

type correlationsCandidatePromptItem struct {
	Symbol       string  `json:"symbol"`
	MaxRho       float64 `json:"maxRho"`
	MaxAbsRho    float64 `json:"maxAbsRho"`
	MaxAbsTarget string  `json:"maxAbsTarget"`
}

type correlationsClusterPromptItem struct {
	HeldCount   int     `json:"heldCount"`
	AvgPairwise float64 `json:"avgPairwise"`
	MaxPairwise float64 `json:"maxPairwise"`
	MaxLeft     string  `json:"maxLeft"`
	MaxRight    string  `json:"maxRight"`
}

// buildCorrelationsPromptItem renders a CorrelationSnapshot for
// the prompt. Returns nil when the snapshot has no signal so the
// prompt simply omits the block. The signed rho values flow
// through round4Signed so the prompt sees stable, diff-friendly
// numbers across runs.
func buildCorrelationsPromptItem(snap *CorrelationSnapshot) *correlationsPromptItem {
	if snap == nil || !snap.HasSignal() {
		return nil
	}
	out := &correlationsPromptItem{
		Window:            snap.Window,
		SampleSize:        snap.SampleSize,
		HighCorrThreshold: round4Signed(snap.HighCorrThreshold),
	}
	if len(snap.HighCorrPairs) > 0 {
		out.HighCorrPairs = make([]correlationsPairPromptItem, 0, len(snap.HighCorrPairs))
		for _, p := range snap.HighCorrPairs {
			out.HighCorrPairs = append(out.HighCorrPairs, correlationsPairPromptItem{
				Left:  p.Left,
				Right: p.Right,
				Rho:   round4Signed(p.Rho),
			})
		}
	}
	if len(snap.CandidateSummaries) > 0 {
		out.CandidateSummaries = make([]correlationsCandidatePromptItem, 0, len(snap.CandidateSummaries))
		for _, c := range snap.CandidateSummaries {
			out.CandidateSummaries = append(out.CandidateSummaries, correlationsCandidatePromptItem{
				Symbol:       c.Symbol,
				MaxRho:       round4Signed(c.MaxRho),
				MaxAbsRho:    round4Signed(c.MaxAbsRho),
				MaxAbsTarget: c.MaxAbsTarget,
			})
		}
	}
	if snap.HeldCluster != nil {
		out.HeldCluster = &correlationsClusterPromptItem{
			HeldCount:   snap.HeldCluster.HeldCount,
			AvgPairwise: round4Signed(snap.HeldCluster.AvgPairwise),
			MaxPairwise: round4Signed(snap.HeldCluster.MaxPairwise),
			MaxLeft:     snap.HeldCluster.MaxLeft,
			MaxRight:    snap.HeldCluster.MaxRight,
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Pair spreads block (Sprint E #4)
// ---------------------------------------------------------------------------

// pairSpreadsPromptItem mirrors PairSpreadSnapshot. The pairs
// slice is sorted descending by |spreadZ| upstream so the LLM
// sees the most-extended pairs first — same convention the
// correlations block uses for HighCorrPairs.
type pairSpreadsPromptItem struct {
	Window       string                   `json:"window"`
	LookbackBars int                      `json:"lookbackBars"`
	ZThreshold   float64                  `json:"zThreshold"`
	Pairs        []pairSpreadRowPromptItem `json:"pairs,omitempty"`
}

// pairSpreadRowPromptItem is one row in the table. All four
// floats go through round4Signed so the prompt is byte-stable
// across runs with the same input (audit-pipeline invariant).
type pairSpreadRowPromptItem struct {
	Left       string  `json:"left"`
	Right      string  `json:"right"`
	Rho        float64 `json:"rho"`
	Spread     float64 `json:"spread"`
	SpreadMean float64 `json:"spreadMean"`
	SpreadStd  float64 `json:"spreadStd"`
	SpreadZ    float64 `json:"spreadZ"`
}

// buildPairSpreadsPromptItem renders a PairSpreadSnapshot for
// the prompt. Returns nil when the snapshot has no signal
// (every |z| below threshold) so the prompt simply omits the
// block. The signal gate is intentional: the spread numbers
// for in-band pairs are not actionable; surfacing them just
// distracts the LLM from the catalysts it can act on.
func buildPairSpreadsPromptItem(snap *PairSpreadSnapshot) *pairSpreadsPromptItem {
	if snap == nil || !snap.HasSignal() {
		return nil
	}
	out := &pairSpreadsPromptItem{
		Window:       snap.Window,
		LookbackBars: snap.LookbackBars,
		ZThreshold:   round4Signed(snap.ZThreshold),
		Pairs:        make([]pairSpreadRowPromptItem, 0, len(snap.PairsByAbsZ)),
	}
	for _, p := range snap.PairsByAbsZ {
		out.Pairs = append(out.Pairs, pairSpreadRowPromptItem{
			Left:       p.Left,
			Right:      p.Right,
			Rho:        round4Signed(p.Rho),
			Spread:     round4Signed(p.Spread),
			SpreadMean: round4Signed(p.SpreadMean),
			SpreadStd:  round4Signed(p.SpreadStd),
			SpreadZ:    round4Signed(p.SpreadZ),
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Earnings calendar block (Sprint E #2)
// ---------------------------------------------------------------------------

// earningsCalendarPromptItem mirrors EarningsCalendarSnapshot but
// flattens the per-symbol map into a stable-sorted slice so the
// LLM sees the upcoming events in date-then-symbol order. Going
// through a slice (rather than the underlying map) keeps the
// prompt JSON byte-identical across runs with the same input —
// important for the prompt-diff audit pipeline.
type earningsCalendarPromptItem struct {
	HorizonDays int                       `json:"horizonDays"`
	Events      []earningsEventPromptItem `json:"events,omitempty"`
}

// earningsEventPromptItem is the per-symbol row in the calendar.
// Date is rendered as YYYY-MM-DD because the time-of-day
// shading lives in `timeOfDay`; the LLM should compare
// `daysUntil` against the trading-date "today" to decide whether
// the event sits inside the dangerous T+0 / T+1 / T+2 window.
type earningsEventPromptItem struct {
	Symbol    string `json:"symbol"`
	Market    string `json:"market,omitempty"`
	Date      string `json:"date"`
	TimeOfDay string `json:"timeOfDay"`
	DaysUntil int    `json:"daysUntil"`
	Source    string `json:"source,omitempty"`
}

// buildEarningsCalendarPromptItem renders an
// EarningsCalendarSnapshot for the prompt. Returns nil when the
// snapshot has no signal so the prompt simply omits the block.
//
// DaysUntil is computed from snap.AsOf so the LLM doesn't have
// to do the arithmetic itself: a 0 means "today", 1 means
// "tomorrow", etc. The downstream system-prompt rule treats
// daysUntil <= 2 as the "no fresh long" zone unless a concrete
// catalyst case overrides.
func buildEarningsCalendarPromptItem(snap *EarningsCalendarSnapshot) *earningsCalendarPromptItem {
	if snap == nil || !snap.HasSignal() {
		return nil
	}
	events := snap.SortedEvents()
	out := &earningsCalendarPromptItem{
		HorizonDays: snap.HorizonDays,
		Events:      make([]earningsEventPromptItem, 0, len(events)),
	}
	asOfDate := snap.AsOf.UTC()
	asOfDay := time.Date(asOfDate.Year(), asOfDate.Month(), asOfDate.Day(), 0, 0, 0, 0, time.UTC)
	for _, e := range events {
		eventDay := e.EventDate.UTC()
		eventDay = time.Date(eventDay.Year(), eventDay.Month(), eventDay.Day(), 0, 0, 0, 0, time.UTC)
		daysUntil := int(eventDay.Sub(asOfDay).Hours() / 24)
		out.Events = append(out.Events, earningsEventPromptItem{
			Symbol:    e.Symbol,
			Market:    e.Market,
			Date:      e.EventDate.UTC().Format("2006-01-02"),
			TimeOfDay: string(e.TimeOfDay),
			DaysUntil: daysUntil,
			Source:    e.Source,
		})
	}
	return out
}

// buildExposurePromptItem renders an ExposureSnapshot for the
// prompt. Returns nil when the snapshot has no signal (zero NAV
// / no holdings / no cash) so the prompt simply omits the block.
//
// Caps that round to 1.0 are omitted because they mean "no cap";
// rendering "100%" would mislead the LLM into thinking there's a
// hard guard when there isn't.
func buildExposurePromptItem(snap ExposureSnapshot) *exposurePromptItem {
	if !snap.HasSignal() {
		return nil
	}
	item := &exposurePromptItem{
		PositionCount: snap.PositionCount,
		CashPct:       round4Signed(snap.CashPct),
		CashFloorPct:  round4Signed(snap.CashFloorPct),
		SingleNameCap: round4Signed(snap.SingleNameCap),
		SectorCap:     round4Signed(snap.SectorCap),
		Top3Cap:       round4Signed(snap.Top3Cap),
		Top3Weight:    round4Signed(snap.Top3Weight),
		Breaches:      snap.Breaches,
	}
	if len(snap.SingleName) > 0 {
		item.SingleName = make([]exposureSinglePromptItem, 0, len(snap.SingleName))
		for _, sn := range snap.SingleName {
			item.SingleName = append(item.SingleName, exposureSinglePromptItem{
				Symbol: sn.Symbol,
				Weight: round4Signed(sn.Weight),
				Breach: sn.Breach,
			})
		}
	}
	if len(snap.SectorWeights) > 0 {
		item.SectorWeights = make([]exposureSectorPromptItem, 0, len(snap.SectorWeights))
		for _, sw := range snap.SectorWeights {
			item.SectorWeights = append(item.SectorWeights, exposureSectorPromptItem{
				Sector: sw.Sector,
				Weight: round4Signed(sw.Weight),
				Breach: sw.Breach,
			})
		}
	}
	return item
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
