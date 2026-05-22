package llm

// ---------------------------------------------------------------------------
// 平台模型目录
// ---------------------------------------------------------------------------

// PlatformModels 平台提供的模型目录，前端用于展示给用户选择。
// 售价单位：分/百万 token（CNY cents per 1M tokens）。
var PlatformModels = []ModelInfo{
	// ===== Critical Tier =====
	{
		Provider:         "openai",
		ModelName:        "gpt-4o",
		DisplayName:      "GPT-4o",
		Tier:             "critical",
		InputPricePer1M:  3500,
		OutputPricePer1M: 14000,
		Description:      "最强综合模型，推荐用于关键决策",
		IsDefault:        true,
	},
	{
		Provider:         "openai",
		ModelName:        "o4-mini",
		DisplayName:      "o4-mini (推理)",
		Tier:             "critical",
		InputPricePer1M:  1500,
		OutputPricePer1M: 6000,
		Description:      "推理增强模型，擅长复杂分析",
		IsDefault:        false,
	},
	{
		Provider:         "claude",
		ModelName:        "claude-sonnet-4-20250514",
		DisplayName:      "Claude Sonnet 4",
		Tier:             "critical",
		InputPricePer1M:  4200,
		OutputPricePer1M: 21000,
		Description:      "Claude最新模型，投资分析能力强",
		IsDefault:        false,
	},
	{
		Provider:         "gemini",
		ModelName:        "gemini-3.1-pro-preview",
		DisplayName:      "Gemini 3.1 Pro Preview",
		Tier:             "critical",
		InputPricePer1M:  0,
		OutputPricePer1M: 0,
		Description:      "Gemini 原生接口模型，适合 Gemini 中转站或官方接口",
		IsDefault:        false,
	},

	// ===== Standard Tier =====
	{
		Provider:         "deepseek",
		ModelName:        "deepseek-chat",
		DisplayName:      "DeepSeek V3",
		Tier:             "standard",
		InputPricePer1M:  150,
		OutputPricePer1M: 300,
		Description:      "性价比之王，日常研究首选",
		IsDefault:        true,
	},
	{
		Provider:         "qwen",
		ModelName:        "qwen-max",
		DisplayName:      "通义千问 Max",
		Tier:             "standard",
		InputPricePer1M:  280,
		OutputPricePer1M: 840,
		Description:      "国产大模型，中文理解优秀",
		IsDefault:        false,
	},
	{
		Provider:         "openai",
		ModelName:        "gpt-4o-mini",
		DisplayName:      "GPT-4o Mini",
		Tier:             "standard",
		InputPricePer1M:  21,
		OutputPricePer1M: 84,
		Description:      "轻量快速，适合大量调用",
		IsDefault:        false,
	},
	{
		Provider:         "gemini",
		ModelName:        "gemini-3.1-pro-preview",
		DisplayName:      "Gemini 3.1 Pro Preview",
		Tier:             "standard",
		InputPricePer1M:  0,
		OutputPricePer1M: 0,
		Description:      "Gemini 原生接口模型，适合研究与翻译任务",
		IsDefault:        false,
	},

	// ===== Simple Tier =====
	{
		Provider:         "openai",
		ModelName:        "gpt-4o-mini",
		DisplayName:      "GPT-4o Mini",
		Tier:             "simple",
		InputPricePer1M:  21,
		OutputPricePer1M: 84,
		Description:      "默认简单任务模型",
		IsDefault:        true,
	},
	{
		Provider:         "deepseek",
		ModelName:        "deepseek-chat",
		DisplayName:      "DeepSeek V3",
		Tier:             "simple",
		InputPricePer1M:  150,
		OutputPricePer1M: 300,
		Description:      "替代简单任务模型",
		IsDefault:        false,
	},
	{
		Provider:         "gemini",
		ModelName:        "gemini-3.1-pro-preview",
		DisplayName:      "Gemini 3.1 Pro Preview",
		Tier:             "simple",
		InputPricePer1M:  0,
		OutputPricePer1M: 0,
		Description:      "Gemini 原生接口模型，可作为轻量回退",
		IsDefault:        false,
	},
}

// ---------------------------------------------------------------------------
// 定价加价率
// ---------------------------------------------------------------------------

// PlatformMarkup 平台加价率：售价 = 成本 × (1 + PlatformMarkup)。
// 40% 加价率。
const PlatformMarkup = 0.4

// ---------------------------------------------------------------------------
// 计费函数
// ---------------------------------------------------------------------------

// CalcPrice 计算单次调用的平台售价（单位：分）。
//
// 公式: price = tokens / 1_000_000 × pricePer1M
func CalcPrice(config *ModelConfig, inputTokens, outputTokens int) (inputPrice, outputPrice, totalPrice float64) {
	if config == nil {
		return 0, 0, 0
	}
	inputPrice = float64(inputTokens) / 1_000_000.0 * config.InputPricePer1M
	outputPrice = float64(outputTokens) / 1_000_000.0 * config.OutputPricePer1M
	totalPrice = inputPrice + outputPrice
	return
}

// CalcCost 计算单次调用的平台实际成本（单位：分）。
//
// 如果 CostPer1M > 0，使用 CostPer1M 作为统一成本单价；
// 否则按 售价 / (1 + PlatformMarkup) 反推。
func CalcCost(config *ModelConfig, inputTokens, outputTokens int) (inputCost, outputCost, totalCost float64) {
	if config == nil {
		return 0, 0, 0
	}

	totalTokens := inputTokens + outputTokens

	if config.CostPer1M > 0 {
		// 使用统一成本单价
		totalCost = float64(totalTokens) / 1_000_000.0 * config.CostPer1M
		// 按 input/output 比例拆分
		if totalTokens > 0 {
			inputCost = totalCost * float64(inputTokens) / float64(totalTokens)
			outputCost = totalCost * float64(outputTokens) / float64(totalTokens)
		}
		return
	}

	// 按售价反推
	_, _, tp := CalcPrice(config, inputTokens, outputTokens)
	totalCost = tp / (1.0 + PlatformMarkup)
	if totalTokens > 0 {
		inputCost = totalCost * float64(inputTokens) / float64(totalTokens)
		outputCost = totalCost * float64(outputTokens) / float64(totalTokens)
	}
	return
}

// ---------------------------------------------------------------------------
// 月度费用预估
// ---------------------------------------------------------------------------

// MonthlyEstimate 月度费用预估。
type MonthlyEstimate struct {
	Tier             string  `json:"tier"`
	Model            string  `json:"model"`
	EstDailyTokens   int     `json:"est_daily_tokens"`   // 预估每日 token 用量
	EstMonthlyTokens int     `json:"est_monthly_tokens"` // 预估每月 token 用量
	EstMonthlyCost   float64 `json:"est_monthly_cost"`   // 预估月度成本（分）
	EstMonthlyPrice  float64 `json:"est_monthly_price"`  // 预估月度售价（分）
}

// 各 tier 预估每日 token 用量（单基金，input + output 合计）。
var estimatedDailyTokens = map[ModelTier]int{
	TierCritical: 50_000,  // 关键步骤调用较少但 prompt 较长
	TierStandard: 200_000, // 日常研究是主要消耗
	TierSimple:   100_000, // 简单提取量适中
}

// tradingDaysPerMonth 每月交易日数。
const tradingDaysPerMonth = 22

// EstimateMonthly 预估某 tier 单基金月度费用。
func EstimateMonthly(tier ModelTier) *MonthlyEstimate {
	config, ok := DefaultModels[tier]
	if !ok {
		return nil
	}

	dailyTokens, ok := estimatedDailyTokens[tier]
	if !ok {
		dailyTokens = 100_000
	}

	monthlyTokens := dailyTokens * tradingDaysPerMonth

	// 假设 input:output = 3:1
	inputTokens := monthlyTokens * 3 / 4
	outputTokens := monthlyTokens / 4

	_, _, monthlyCost := CalcCost(config, inputTokens, outputTokens)
	_, _, monthlyPrice := CalcPrice(config, inputTokens, outputTokens)

	return &MonthlyEstimate{
		Tier:             string(tier),
		Model:            config.ModelName,
		EstDailyTokens:   dailyTokens,
		EstMonthlyTokens: monthlyTokens,
		EstMonthlyCost:   monthlyCost,
		EstMonthlyPrice:  monthlyPrice,
	}
}

// EstimateMonthlyTotal 预估单基金所有 tier 合计月度费用。
func EstimateMonthlyTotal() (estimates []*MonthlyEstimate, totalCost, totalPrice float64) {
	for _, tier := range ValidTiers {
		est := EstimateMonthly(tier)
		if est == nil {
			continue
		}
		estimates = append(estimates, est)
		totalCost += est.EstMonthlyCost
		totalPrice += est.EstMonthlyPrice
	}
	return
}

// ---------------------------------------------------------------------------
// 辅助：按模型名查找目录定价
// ---------------------------------------------------------------------------

// FindModelInfo 在 PlatformModels 中按名称查找。
// 如果同名模型在多个 tier 中出现，返回第一个匹配项。
func FindModelInfo(modelName string) *ModelInfo {
	for i := range PlatformModels {
		if PlatformModels[i].ModelName == modelName {
			info := PlatformModels[i]
			return &info
		}
	}
	return nil
}

// FindModelInfoByTier 在 PlatformModels 中按名称和 tier 查找。
func FindModelInfoByTier(modelName string, tier string) *ModelInfo {
	for i := range PlatformModels {
		if PlatformModels[i].ModelName == modelName && PlatformModels[i].Tier == tier {
			info := PlatformModels[i]
			return &info
		}
	}
	return nil
}

// GetDefaultModelsForTier 返回某 tier 下的所有平台模型。
func GetDefaultModelsForTier(tier string) []ModelInfo {
	var result []ModelInfo
	for _, m := range PlatformModels {
		if m.Tier == tier {
			result = append(result, m)
		}
	}
	return result
}
