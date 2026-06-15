// messages_zh.go is the Simplified Chinese (zh-CN) translation table.
//
// Authoring rules:
//
//   - Every key declared in keys.go MUST exist here AND in messages_en.go.
//     The bundle_test enforces parity.
//   - Templates use indexed placeholders (%[1]s, %[2]d, ...) so the en
//     variant can re-order arguments freely.
//   - Keep one entry per line (or use a `string` literal block for
//     multi-line prompt bodies); never inline runtime concatenation —
//     that's what Tf() is for.
package i18nmsg

var messagesZH = map[Key]string{
	// Advisor
	KeyAdvisorPresetLocaleBlocked: "该投顾预设仅在简体中文下提供。",

	// Tactic display names
	KeyTacticName_TailSniper: "尾盘狙击手",
	KeyTacticName_FirstLimit: "首板低吸",
	KeyTacticName_DragonHead: "龙头打板",
	KeyTacticName_Pullback:   "缩量回踩",

	// Plan verdicts
	KeyPlanVerdict_BuyIncrease: "买入/增配",
	KeyPlanVerdict_SellReduce:  "卖出/降仓",
	KeyPlanVerdict_HoldWatch:   "持有观察",
	KeyPlanVerdict_WatchOnly:   "仅观察",

	// Fund-assist (zh template; the en version mirrors the structure)
	KeyFundAssistSystem: "" +
		"你是 AI 基金团队的「基金创建助手」，输入是用户对基金愿景的自然语言描述，" +
		"输出是一份可被系统直接消费的 JSON 计划。请严格按 schema 返回，并在 description / rationale 字段使用中文。",
	KeyFundAssistUserIntro:       "请基于以下用户输入完成基金创建计划：",
	KeyFundAssistInvalidFundName: "基金名称不能为空，请填写一个 2-40 字的中文或英文名称。",
	KeyFundAssistInvalidMarket:   "需要明确指定市场，例如 us / a_share / hk。",

	// A/B shadow learning
	KeyABShadowSystemPrompt: "" +
		"你是 A/B 影子实验的复盘助手。请根据提供的对照交易记录，" +
		"在 ≤80 字内用中文给出 reasoning，并按 schema 返回结构化字段。",
	KeyABShadowRecapSystemPrompt: "" +
		"你是 A/B 影子实验的总结助手。请在 ≤120 字内用中文总结本期对照实验的关键差异，" +
		"并给出 lessons / adjustments / summary 三个字段。",
	KeyABShadowAutoBaseline:   "[auto-shadow] A 组沿用当前基金真实决策作为基线影子交易。",
	KeyABShadowAutoSkippedFmt: "[auto-shadow] B 组本次未参与该笔交易：%[1]s",
	KeyABShadowAutoRejectFmt:  "[auto-shadow] B 组决策被账本拒绝（如无库存可卖）：%[1]s",

	KeyABShadowVerdictNeedMoreData:     "样本天数偏少，建议继续观察至样本充分后再结论。",
	KeyABShadowVerdictMissingSamples:   "缺少有效对照样本，无法给出胜负判断。",
	KeyABShadowVerdictFlatPnl:          "A/B 收益差异不明显，暂不建议直接应用胜出策略。",
	KeyABShadowVerdictKeepObserving:    "继续观察，待置信度充足后再决定是否替换基线策略。",
	KeyABShadowScoreReturnContribution: "收益贡献",
	KeyABShadowScoreRiskAdjusted:       "风险调整收益",
	KeyABShadowScoreDrawdownControl:    "回撤控制",
	KeyABShadowScoreVolatilityControl:  "波动控制",
	KeyABShadowScoreTurnoverPenalty:    "换手惩罚",
	KeyABShadowScoreCostPenalty:        "成本惩罚",
	KeyABShadowScoreSampleSufficiency:  "样本充分性",

	KeyABShadowVariantBaseline:   "当前策略",
	KeyABShadowVariantExperiment: "实验策略",
	KeyABShadowSkillCandidateFmt: "A/B 候选技能 · %[1]s",

	// Agent self-learning
	KeyAgentLearningSystem: "" +
		"你是 AI 基金团队的复盘教练。请基于今日决策与执行回放，" +
		"为指定 agent 生成 ≤200 字的中文复盘，包含 lessons / adjustments 字段。",
	KeyAgentLearningPlanMissing: "Plan: 今日未生成。",
	KeyAgentLearningRoleHintPM:  "请以组合层视角复盘本日 PM 的仓位与风险预算决策。",
	KeyAgentLearningRoleHintRsr: "请以买方研究视角复盘本日 Researcher 的研究覆盖与判断变化。",
	KeyAgentLearningRoleHintRsk: "请以风险控制视角复盘本日 Risk 的预警与拒单逻辑。",
	KeyAgentLearningRoleHintTrd: "请以执行视角复盘本日 Trader 的下单节奏与成交质量。",

	// Tactic agent
	KeyTacticAgentSystem: "" +
		"你是 %[1]s（%[2]s），一位专注于 A 股短线交易的策略 agent。" +
		"请用中文叙述 thesis，控制在 ≤200 字，并按 schema 返回结构化字段。",
	KeyTacticAgentNoEntry:           "暂未触发买点。",
	KeyTacticAgentVetoedHardRisk:    "被全局硬风控前置闸门否决，跳过本次策略推演。",
	KeyTacticAgentVetoedHardRulesFx: "命中硬性否决条件：%[1]s",

	// Decision trace
	KeyDecisionTraceMacroBriefHeader:      "宏观简报",
	KeyDecisionTraceMacroBriefUnavailable: "宏观简报暂不可用：行情数据源未启用。",
	KeyDecisionTraceMacroBriefNoBaseline:  "宏观简报暂不可用：基准研究无法刷新。",
	KeyDecisionTraceTopicNews:             "主题资讯",
	KeyDecisionTraceSectorRotation:        "板块轮动",
	KeyDecisionTraceNewsSentiment:         "新闻情绪",
	KeyDecisionTraceQuantUnavailable:      "量化信号暂不可用。",
	KeyDecisionTraceQuantNoSignal:         "暂无信号。",
	KeyDecisionTraceResearchMacro:         "宏观研究",
	KeyDecisionTraceResearchSingleStock:   "个股研究",
	KeyDecisionTraceResearchFundamentals:  "基本面研究",
	KeyDecisionTraceResearchGeneric:       "研究",
	KeyDecisionTraceQuoteFallbackFmt:      "未能从行情源取到 %[1]s 的实时报价；可点击数据源切换尝试其他渠道。",
	KeyDecisionTraceTargetIncomplete:      "标的配置仍不完整，请补充必要字段后再发起决策。",
	KeyDecisionTraceCnTPlusOne:            "A 股市场 T+1 结算规则：今日买入的仓位需至下一交易日才能卖出。",
	KeyDecisionTraceObservationOnly:       "本次未对该团队覆盖标的下单，仅纳入观察。",
	KeyDecisionTraceRiskReviewDone:        "风控审查已完成。",
	KeyDecisionTraceMatchedSkillsFmt:      "匹配技能：%[1]s",
}
