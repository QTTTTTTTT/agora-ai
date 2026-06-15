// keys.go declares every translation key in a single place so a typo at
// the call-site is a compile error rather than a silent runtime miss.
//
// Conventions:
//
//   - Keys are grouped by feature area (advisor / decision_trace /
//     ab_shadow / agent_learning / fund_assist / api_errors / ...).
//   - Within a group, the constant names mirror the dotted key string.
//   - Format-string templates use indexed placeholders (%[1]s, %[2]d ...)
//     so zh and en variants can re-order arguments freely.
//
// New keys are added here, then in messages_zh.go AND messages_en.go.
// The english-smoke test enforces parity.
package i18nmsg

// ---------------------------------------------------------------------------
// Advisor / Master Team
// ---------------------------------------------------------------------------

const (
	// KeyAdvisorPresetLocaleBlocked is the user-facing reason returned
	// when an en-US user requests a preset whose TacticKeys reference
	// the cn_tactics persona library (e.g. cn_short / 战术大师).
	KeyAdvisorPresetLocaleBlocked Key = "advisor.preset_locale_blocked"
)

// ---------------------------------------------------------------------------
// Tactic display names (advisor_coldstart.go:471-477 + tactic_agent labels).
// Each cn_tactics persona has a localised label; the key is built from the
// persona slug.
// ---------------------------------------------------------------------------

const (
	KeyTacticName_TailSniper Key = "tactic.name.tail_sniper"
	KeyTacticName_FirstLimit Key = "tactic.name.first_limit"
	KeyTacticName_DragonHead Key = "tactic.name.dragon_head"
	KeyTacticName_Pullback   Key = "tactic.name.pullback"
)

// ---------------------------------------------------------------------------
// Decision trace plan-action verdict labels
// (wiring_adapters.go:14406-14412)
// ---------------------------------------------------------------------------

const (
	KeyPlanVerdict_BuyIncrease Key = "plan_verdict.buy_increase"
	KeyPlanVerdict_SellReduce  Key = "plan_verdict.sell_reduce"
	KeyPlanVerdict_HoldWatch   Key = "plan_verdict.hold_watch"
	KeyPlanVerdict_WatchOnly   Key = "plan_verdict.watch_only"
)

// ---------------------------------------------------------------------------
// Fund-assist (internal/api/fund_assist.go)
// ---------------------------------------------------------------------------

const (
	KeyFundAssistSystem    Key = "fund_assist.system_prompt"
	KeyFundAssistUserIntro Key = "fund_assist.user_intro"
	// Validation hint keys (fund_assist.go:198-274)
	KeyFundAssistInvalidFundName Key = "fund_assist.invalid.fund_name"
	KeyFundAssistInvalidMarket   Key = "fund_assist.invalid.market"
)

// ---------------------------------------------------------------------------
// A/B shadow learning (cmd/server/ab_shadow_bside.go)
// ---------------------------------------------------------------------------

const (
	KeyABShadowSystemPrompt      Key = "ab_shadow.system_prompt"
	KeyABShadowRecapSystemPrompt Key = "ab_shadow.recap_system_prompt"

	// Auto-shadow runtime annotations
	KeyABShadowAutoBaseline   Key = "ab_shadow.auto_baseline"
	KeyABShadowAutoSkippedFmt Key = "ab_shadow.auto_skipped_fmt"
	KeyABShadowAutoRejectFmt  Key = "ab_shadow.auto_reject_fmt"

	// Verdict / scoreboard fallback strings (10132-10247)
	KeyABShadowVerdictNeedMoreData     Key = "ab_shadow.verdict.need_more_data"
	KeyABShadowVerdictMissingSamples   Key = "ab_shadow.verdict.missing_samples"
	KeyABShadowVerdictFlatPnl          Key = "ab_shadow.verdict.flat_pnl"
	KeyABShadowVerdictKeepObserving    Key = "ab_shadow.verdict.keep_observing"
	KeyABShadowScoreReturnContribution Key = "ab_shadow.score.return_contribution"
	KeyABShadowScoreRiskAdjusted       Key = "ab_shadow.score.risk_adjusted"
	KeyABShadowScoreDrawdownControl    Key = "ab_shadow.score.drawdown_control"
	KeyABShadowScoreVolatilityControl  Key = "ab_shadow.score.volatility_control"
	KeyABShadowScoreTurnoverPenalty    Key = "ab_shadow.score.turnover_penalty"
	KeyABShadowScoreCostPenalty        Key = "ab_shadow.score.cost_penalty"
	KeyABShadowScoreSampleSufficiency  Key = "ab_shadow.score.sample_sufficiency"

	// Variant default labels (8520, 8521)
	KeyABShadowVariantBaseline   Key = "ab_shadow.variant.baseline"
	KeyABShadowVariantExperiment Key = "ab_shadow.variant.experiment"

	// Cross-fund skill candidate (9301)
	KeyABShadowSkillCandidateFmt Key = "ab_shadow.skill_candidate_fmt"
)

// ---------------------------------------------------------------------------
// Agent self-learning (cmd/server/wiring_adapters.go:22012,
// agent_self_learning_prompts.go)
// ---------------------------------------------------------------------------

const (
	KeyAgentLearningSystem      Key = "agent_learning.system_prompt"
	KeyAgentLearningPlanMissing Key = "agent_learning.plan_missing"
	KeyAgentLearningRoleHintPM  Key = "agent_learning.role_hint.pm"
	KeyAgentLearningRoleHintRsr Key = "agent_learning.role_hint.researcher"
	KeyAgentLearningRoleHintRsk Key = "agent_learning.role_hint.risk"
	KeyAgentLearningRoleHintTrd Key = "agent_learning.role_hint.trader"
)

// ---------------------------------------------------------------------------
// Tactic agent system prompt (internal/agent/tactic_agent.go:702-722)
// ---------------------------------------------------------------------------

const (
	KeyTacticAgentSystem            Key = "tactic_agent.system_prompt"
	KeyTacticAgentNoEntry           Key = "tactic_agent.no_entry"
	KeyTacticAgentVetoedHardRisk    Key = "tactic_agent.vetoed_hard_risk"
	KeyTacticAgentVetoedHardRulesFx Key = "tactic_agent.vetoed_hard_rules_fmt"
)

// ---------------------------------------------------------------------------
// Decision-trace ancillary fallbacks (wiring_adapters.go misc)
// ---------------------------------------------------------------------------

const (
	KeyDecisionTraceMacroBriefHeader      Key = "decision_trace.macro_brief.header"
	KeyDecisionTraceMacroBriefUnavailable Key = "decision_trace.macro_brief.unavailable"
	KeyDecisionTraceMacroBriefNoBaseline  Key = "decision_trace.macro_brief.no_baseline"
	KeyDecisionTraceTopicNews             Key = "decision_trace.topic_news"
	KeyDecisionTraceSectorRotation        Key = "decision_trace.sector_rotation"
	KeyDecisionTraceNewsSentiment         Key = "decision_trace.news_sentiment"
	KeyDecisionTraceQuantUnavailable      Key = "decision_trace.quant.unavailable"
	KeyDecisionTraceQuantNoSignal         Key = "decision_trace.quant.no_signal"
	KeyDecisionTraceResearchMacro         Key = "decision_trace.research.macro"
	KeyDecisionTraceResearchSingleStock   Key = "decision_trace.research.single_stock"
	KeyDecisionTraceResearchFundamentals  Key = "decision_trace.research.fundamentals"
	KeyDecisionTraceResearchGeneric       Key = "decision_trace.research.generic"
	KeyDecisionTraceQuoteFallbackFmt      Key = "decision_trace.quote_fallback_fmt"
	KeyDecisionTraceTargetIncomplete      Key = "decision_trace.target_incomplete"
	KeyDecisionTraceCnTPlusOne            Key = "decision_trace.cn_t_plus_one"
	KeyDecisionTraceObservationOnly       Key = "decision_trace.observation_only"
	KeyDecisionTraceRiskReviewDone        Key = "decision_trace.risk_review_done"
	KeyDecisionTraceMatchedSkillsFmt      Key = "decision_trace.matched_skills_fmt"
)

// ---------------------------------------------------------------------------
// Backend error details (admin_handler.go:751,786,813 explicitly EXEMPT —
// only non-admin handlers are localised here. New keys go above this
// banner as they are discovered.)
// ---------------------------------------------------------------------------
