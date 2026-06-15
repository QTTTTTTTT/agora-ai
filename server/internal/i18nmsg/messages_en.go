// messages_en.go is the English (en-US) translation table.
//
// Authoring rules: see messages_zh.go. Both maps must declare the same
// set of keys; the english-smoke test enforces parity and also asserts
// that every value in this file is free of CJK ideographs (so a copy-
// paste accident from the zh map is caught at CI time).
package i18nmsg

var messagesEN = map[Key]string{
	// Advisor
	KeyAdvisorPresetLocaleBlocked: "This advisor preset is only available in Simplified Chinese.",

	// Tactic display names
	KeyTacticName_TailSniper: "Closing-Bell Sniper",
	KeyTacticName_FirstLimit: "First Limit-Up Dip Buyer",
	KeyTacticName_DragonHead: "Sector Leader Breakout",
	KeyTacticName_Pullback:   "Low-Volume Pullback",

	// Plan verdicts
	KeyPlanVerdict_BuyIncrease: "Buy / Increase",
	KeyPlanVerdict_SellReduce:  "Sell / Reduce",
	KeyPlanVerdict_HoldWatch:   "Hold & Watch",
	KeyPlanVerdict_WatchOnly:   "Watch only",

	// Fund-assist
	KeyFundAssistSystem: "" +
		"You are the AI fund team's Fund Creation Assistant. The input is a free-form natural-language " +
		"description of the user's fund vision; the output is a JSON plan that the system can consume " +
		"directly. Strictly follow the schema and write the description / rationale fields in English.",
	KeyFundAssistUserIntro:       "Build the fund creation plan based on the following user input:",
	KeyFundAssistInvalidFundName: "Fund name is required (2-40 characters, ASCII or Unicode letters).",
	KeyFundAssistInvalidMarket:   "Market must be specified explicitly, e.g. us / a_share / hk.",

	// A/B shadow learning
	KeyABShadowSystemPrompt: "" +
		"You are the recap assistant for the A/B shadow experiment. Given the paired trade records, " +
		"produce a reasoning of at most 80 words in English and return the structured fields exactly " +
		"per the schema.",
	KeyABShadowRecapSystemPrompt: "" +
		"You are the summary assistant for the A/B shadow experiment. In at most 120 words of English, " +
		"summarise the key differences observed in this round and emit the lessons / adjustments / " +
		"summary fields.",
	KeyABShadowAutoBaseline:   "[auto-shadow] Variant A reuses the fund's current live decisions as the baseline shadow trade.",
	KeyABShadowAutoSkippedFmt: "[auto-shadow] Variant B did not participate in this trade: %[1]s",
	KeyABShadowAutoRejectFmt:  "[auto-shadow] Variant B's decision was rejected by the ledger (e.g. no inventory to sell): %[1]s",

	KeyABShadowVerdictNeedMoreData:     "Sample size is small; keep observing until the experiment accumulates enough trades before drawing a verdict.",
	KeyABShadowVerdictMissingSamples:   "Not enough valid paired samples to declare a winner.",
	KeyABShadowVerdictFlatPnl:          "A/B P&L difference is not material; do not promote the winning variant yet.",
	KeyABShadowVerdictKeepObserving:    "Keep observing until confidence is sufficient before replacing the baseline strategy.",
	KeyABShadowScoreReturnContribution: "Return contribution",
	KeyABShadowScoreRiskAdjusted:       "Risk-adjusted return",
	KeyABShadowScoreDrawdownControl:    "Drawdown control",
	KeyABShadowScoreVolatilityControl:  "Volatility control",
	KeyABShadowScoreTurnoverPenalty:    "Turnover penalty",
	KeyABShadowScoreCostPenalty:        "Cost penalty",
	KeyABShadowScoreSampleSufficiency:  "Sample sufficiency",

	KeyABShadowVariantBaseline:   "Baseline strategy",
	KeyABShadowVariantExperiment: "Experiment strategy",
	KeyABShadowSkillCandidateFmt: "A/B candidate skill · %[1]s",

	// Agent self-learning
	KeyAgentLearningSystem: "" +
		"You are the AI fund team's daily-recap coach. Based on today's decisions and execution replay, " +
		"produce a recap of at most 200 words in English for the named agent, with lessons / adjustments " +
		"fields.",
	KeyAgentLearningPlanMissing: "Plan: not generated today.",
	KeyAgentLearningRoleHintPM:  "Recap today's PM position-sizing and risk-budget decisions from a portfolio-level view.",
	KeyAgentLearningRoleHintRsr: "Recap today's Researcher coverage and view changes from a buy-side research perspective.",
	KeyAgentLearningRoleHintRsk: "Recap today's Risk alerts and order-rejection logic from a risk-management perspective.",
	KeyAgentLearningRoleHintTrd: "Recap today's Trader pacing and execution quality from an execution-desk perspective.",

	// Tactic agent
	KeyTacticAgentSystem: "" +
		"You are %[1]s (%[2]s), a strategy agent specialised in A-share short-term trading. " +
		"Write the thesis in English, keep it under 200 words, and return the structured fields per the schema.",
	KeyTacticAgentNoEntry:           "No entry trigger fired today.",
	KeyTacticAgentVetoedHardRisk:    "Vetoed by the global hard-risk pre-gate; skipping this tactic.",
	KeyTacticAgentVetoedHardRulesFx: "Hit a hard-veto rule: %[1]s",

	// Decision trace
	KeyDecisionTraceMacroBriefHeader:      "Macro brief",
	KeyDecisionTraceMacroBriefUnavailable: "Macro brief is unavailable: market-data source is not enabled.",
	KeyDecisionTraceMacroBriefNoBaseline:  "Macro brief is unavailable: benchmark research could not refresh.",
	KeyDecisionTraceTopicNews:             "Topic news",
	KeyDecisionTraceSectorRotation:        "Sector rotation",
	KeyDecisionTraceNewsSentiment:         "News sentiment",
	KeyDecisionTraceQuantUnavailable:      "Quant signals unavailable.",
	KeyDecisionTraceQuantNoSignal:         "No signal.",
	KeyDecisionTraceResearchMacro:         "Macro research",
	KeyDecisionTraceResearchSingleStock:   "Single-stock research",
	KeyDecisionTraceResearchFundamentals:  "Fundamental research",
	KeyDecisionTraceResearchGeneric:       "Research",
	KeyDecisionTraceQuoteFallbackFmt:      "Could not fetch a real-time quote for %[1]s from the market source; click the data-source toggle to try another channel.",
	KeyDecisionTraceTargetIncomplete:      "Target configuration is still incomplete; fill in the required fields before submitting the decision.",
	KeyDecisionTraceCnTPlusOne:            "A-share T+1 settlement rule: positions opened today cannot be sold until the next trading day.",
	KeyDecisionTraceObservationOnly:       "No order placed against this team's coverage this round; flagged as observation-only.",
	KeyDecisionTraceRiskReviewDone:        "Risk review completed.",
	KeyDecisionTraceMatchedSkillsFmt:      "Matched skills: %[1]s",
}
