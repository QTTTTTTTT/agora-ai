// master_rule_prior.go — deterministic rule evaluator for master
// personas. See FundamentalsBlock.RulePrior + RulePriorBlock above.
//
// Why this exists: Buffett's persona declares "ROE_10yr_avg >= 15%"
// and we cannot trust an LLM to compute that average over a 10-year
// series without slipping. The /advisor wiring layer pre-computes
// the verdict in Go, so the LLM only narrates around a pre-decided
// PASS / FAIL / UNKNOWN signal.
//
// The evaluator is intentionally conservative: a missing field, a
// malformed threshold, or an inconclusive range yields UNKNOWN —
// never FAIL. Hard-rule FAILs only fire when the data is unambiguous.

package agent

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// BuildMasterRulePrior walks the persona.must_have_criteria block
// and evaluates each criterion against the snapshot + history.
//
// Returns nil when there is nothing evaluable — callers should
// fall back to the "history.10yr = unavailable" prompt nudge so
// the LLM is told to honestly admit data gaps.
func BuildMasterRulePrior(persona MasterPersona, fundamentals *FundamentalsBlock) *RulePriorBlock {
	if persona.Raw == nil || fundamentals == nil {
		return nil
	}
	criteriaRaw, ok := persona.Raw["must_have_criteria"].(map[string]any)
	if !ok || len(criteriaRaw) == 0 {
		return nil
	}

	// Sort criteria keys so the prior block renders deterministically.
	keys := make([]string, 0, len(criteriaRaw))
	for k := range criteriaRaw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	block := &RulePriorBlock{
		Persona: persona.Key,
	}
	for _, key := range keys {
		item := evaluateRule(key, criteriaRaw[key], fundamentals)
		block.Items = append(block.Items, item)
	}
	if len(fundamentals.History) == 0 {
		block.Notes = append(block.Notes,
			"history.unavailable: snapshot only — multi-year criteria may degrade to UNKNOWN")
	} else if span := historicalSpanYears(fundamentals.History); span > 0 {
		block.Notes = append(block.Notes,
			fmt.Sprintf("history.span_years=%d (multi-year averages computed over actual span)", span))
	}
	if len(block.Items) == 0 {
		return nil
	}
	return block
}

// evaluateRule routes one criterion to the right comparator based
// on its key + the shape of its threshold value.
func evaluateRule(key string, threshold any, f *FundamentalsBlock) RulePriorItem {
	item := RulePriorItem{
		Key:      key,
		Required: stringifyThreshold(threshold),
		Status:   "UNKNOWN",
	}

	lower := strings.ToLower(key)

	switch {
	case strings.Contains(lower, "10yr") && strings.Contains(lower, "roe"):
		applyHistoricalAvg(&item, f.History, func(y YearlyMetricsLite) float64 { return y.ReturnOnEquity }, 10, threshold)
	case strings.Contains(lower, "10yr") && strings.Contains(lower, "roic"):
		applyHistoricalAvg(&item, f.History, func(y YearlyMetricsLite) float64 { return y.ReturnOnCapital }, 10, threshold)
	case strings.Contains(lower, "earnings_predictability"):
		applyEarningsPredictability(&item, f.History, threshold)
	case strings.Contains(lower, "free_cash_flow") || strings.Contains(lower, "fcf"):
		applyFCFRule(&item, f.History, threshold)
	case strings.Contains(lower, "debt_to_equity"):
		applySnapshotRule(&item, f.Metrics, "debt_to_equity", threshold)
	case strings.Contains(lower, "gross_margin"):
		applyHistoricalStability(&item, f.History, func(y YearlyMetricsLite) float64 { return y.GrossMargin }, threshold)
	case strings.Contains(lower, "peg"):
		applySnapshotRule(&item, f.Metrics, "peg", threshold)
	case strings.Contains(lower, "pe") && !strings.Contains(lower, "peg"):
		applySnapshotRule(&item, f.Metrics, "pe", threshold)
	case strings.Contains(lower, "pb"):
		applySnapshotRule(&item, f.Metrics, "pb", threshold)
	case strings.Contains(lower, "dividend"):
		applySnapshotRule(&item, f.Metrics, "dividend_yield", threshold)
	case strings.Contains(lower, "market_cap"):
		applySnapshotRule(&item, f.Metrics, "market_cap", threshold)
	case strings.Contains(lower, "revenue_growth") || strings.Contains(lower, "sales_growth"):
		applySnapshotOrHistoryRule(&item, f, "revenue_growth_yoy", func(y YearlyMetricsLite) float64 { return y.RevenueGrowthYoY }, threshold)
	case strings.Contains(lower, "earnings_growth") || strings.Contains(lower, "eps_growth"):
		applySnapshotOrHistoryRule(&item, f, "earnings_growth_yoy", func(y YearlyMetricsLite) float64 { return y.EarningsGrowthYoY }, threshold)
	case strings.Contains(lower, "current_ratio"):
		applySnapshotRule(&item, f.Metrics, "current_ratio", threshold)
	default:
		// Unknown criterion key — leave UNKNOWN so the LLM
		// knows to apply its own judgment. We still record the
		// requirement so the prompt surfaces what the persona
		// wants.
	}
	return item
}

func applyHistoricalAvg(item *RulePriorItem, history []YearlyMetricsLite, sel func(YearlyMetricsLite) float64, cap int, threshold any) {
	if len(history) == 0 {
		item.Detail = "history empty"
		return
	}
	limit := cap
	if limit <= 0 || limit > len(history) {
		limit = len(history)
	}
	var sum float64
	var n int
	for i := 0; i < limit; i++ {
		v := sel(history[i])
		if v == 0 {
			continue
		}
		sum += v
		n++
	}
	if n == 0 {
		item.Detail = "no usable history values"
		return
	}
	avg := sum / float64(n)
	item.ValueLow, item.ValueHigh = avg, avg
	item.Observed = fmt.Sprintf("%.2f%% (%dyr avg)", avg*100, n)
	item.Status = compareThreshold(threshold, avg)
}

func applyHistoricalStability(item *RulePriorItem, history []YearlyMetricsLite, sel func(YearlyMetricsLite) float64, threshold any) {
	if len(history) < 3 {
		item.Detail = "need at least 3 years of history for stability check"
		return
	}
	var values []float64
	for _, y := range history {
		v := sel(y)
		if v == 0 {
			continue
		}
		values = append(values, v)
	}
	if len(values) < 3 {
		item.Detail = "fewer than 3 non-zero values"
		return
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))
	var varSum float64
	for _, v := range values {
		varSum += (v - mean) * (v - mean)
	}
	stddev := math.Sqrt(varSum / float64(len(values)))
	item.ValueLow = mean
	item.ValueHigh = mean
	item.Observed = fmt.Sprintf("%dyr mean=%.2f%% stddev=%.2fpp", len(values), mean*100, stddev*100)
	// Persona uses "10年稳定，波动<5pp" — heuristic: PASS when
	// stddev < 5pp (0.05). If threshold is numeric we honour it
	// directly, otherwise fall back to the 5pp heuristic.
	if pct, ok := numericThreshold(threshold); ok {
		if stddev*100 <= pct {
			item.Status = "PASS"
		} else {
			item.Status = "FAIL"
		}
		return
	}
	if stddev <= 0.05 {
		item.Status = "PASS"
	} else {
		item.Status = "FAIL"
	}
}

// applyEarningsPredictability heuristically scores whether earnings
// have grown monotonically or close to it across the available
// history. Used for Buffett's "earnings_predictability=高" check.
func applyEarningsPredictability(item *RulePriorItem, history []YearlyMetricsLite, _ any) {
	if len(history) < 3 {
		item.Detail = "fewer than 3 years of EPS"
		return
	}
	var rises, falls int
	// History is most-recent first; reverse-iterate to get YoY
	// changes in chronological order.
	for i := len(history) - 1; i > 0; i-- {
		prev := history[i].EPS
		cur := history[i-1].EPS
		if prev == 0 || cur == 0 {
			continue
		}
		if cur > prev {
			rises++
		} else if cur < prev {
			falls++
		}
	}
	total := rises + falls
	if total == 0 {
		item.Detail = "no comparable EPS pairs"
		return
	}
	hitRate := float64(rises) / float64(total)
	item.Observed = fmt.Sprintf("%.0f%% of years grew EPS (%d/%d)", hitRate*100, rises, total)
	item.ValueLow, item.ValueHigh = hitRate, hitRate
	switch {
	case hitRate >= 0.8:
		item.Status = "PASS"
	case hitRate >= 0.6:
		item.Status = "UNKNOWN"
		item.Detail = "mixed — borderline predictability"
	default:
		item.Status = "FAIL"
	}
}

// applyFCFRule checks the "连续 N 年 FCF 为正" pattern Buffett /
// Graham use.
func applyFCFRule(item *RulePriorItem, history []YearlyMetricsLite, threshold any) {
	if len(history) == 0 {
		item.Detail = "no history"
		return
	}
	// Inspect at most 10 years.
	limit := 10
	if limit > len(history) {
		limit = len(history)
	}
	var positives, negatives, zeros int
	for i := 0; i < limit; i++ {
		switch {
		case history[i].FreeCashFlow > 0:
			positives++
		case history[i].FreeCashFlow < 0:
			negatives++
		default:
			zeros++
		}
	}
	usable := positives + negatives + zeros
	if usable == 0 {
		item.Detail = "no FCF values in history"
		return
	}
	item.Observed = fmt.Sprintf("%dyr: pos=%d neg=%d zero=%d", usable, positives, negatives, zeros)
	item.ValueLow = float64(positives)
	item.ValueHigh = float64(usable)
	if negatives == 0 && positives >= 3 {
		item.Status = "PASS"
	} else if negatives >= 3 {
		item.Status = "FAIL"
	} else {
		item.Status = "UNKNOWN"
		item.Detail = "intermittent FCF gaps"
	}
	_ = threshold
}

// applySnapshotRule runs a numeric comparison against a single
// metric from the snapshot Metrics map.
func applySnapshotRule(item *RulePriorItem, metrics map[string]float64, metricKey string, threshold any) {
	if metrics == nil {
		item.Detail = "snapshot metrics nil"
		return
	}
	v, ok := metrics[metricKey]
	if !ok {
		item.Detail = fmt.Sprintf("snapshot missing %s", metricKey)
		return
	}
	item.ValueLow, item.ValueHigh = v, v
	item.Observed = fmt.Sprintf("%.4f", v)
	item.Status = compareThreshold(threshold, v)
}

// applySnapshotOrHistoryRule prefers the snapshot value but falls
// back to a multi-year average if the snapshot is missing.
func applySnapshotOrHistoryRule(item *RulePriorItem, f *FundamentalsBlock, metricKey string, sel func(YearlyMetricsLite) float64, threshold any) {
	if f == nil {
		return
	}
	if v, ok := f.Metrics[metricKey]; ok && v != 0 {
		item.ValueLow, item.ValueHigh = v, v
		item.Observed = fmt.Sprintf("%.4f (snapshot)", v)
		item.Status = compareThreshold(threshold, v)
		return
	}
	applyHistoricalAvg(item, f.History, sel, 5, threshold)
}

// compareThreshold parses a threshold (number, string like ">=15%",
// or map like {min:5,max:20}) and returns PASS / FAIL / UNKNOWN.
func compareThreshold(threshold any, observed float64) string {
	switch t := threshold.(type) {
	case float64:
		if observed >= t {
			return "PASS"
		}
		return "FAIL"
	case int:
		if observed >= float64(t) {
			return "PASS"
		}
		return "FAIL"
	case string:
		return compareStringThreshold(t, observed)
	case map[string]any:
		minV, hasMin := numericFromMap(t, "min")
		maxV, hasMax := numericFromMap(t, "max")
		switch {
		case hasMin && hasMax:
			if observed >= minV && observed <= maxV {
				return "PASS"
			}
			return "FAIL"
		case hasMin:
			if observed >= minV {
				return "PASS"
			}
			return "FAIL"
		case hasMax:
			if observed <= maxV {
				return "PASS"
			}
			return "FAIL"
		}
	}
	return "UNKNOWN"
}

// compareStringThreshold parses strings like ">=15%", "<=0.5",
// "<= 30", ">= 5亿".
func compareStringThreshold(spec string, observed float64) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "UNKNOWN"
	}
	var op string
	switch {
	case strings.HasPrefix(spec, ">="):
		op = ">="
		spec = strings.TrimSpace(spec[2:])
	case strings.HasPrefix(spec, "<="):
		op = "<="
		spec = strings.TrimSpace(spec[2:])
	case strings.HasPrefix(spec, ">"):
		op = ">"
		spec = strings.TrimSpace(spec[1:])
	case strings.HasPrefix(spec, "<"):
		op = "<"
		spec = strings.TrimSpace(spec[1:])
	case strings.HasPrefix(spec, "="):
		op = "="
		spec = strings.TrimSpace(spec[1:])
	default:
		// Bare numeric → treat as >=
		op = ">="
	}
	// Strip % and unit hints like "亿".
	v, err := parseNumericToken(spec)
	if err != nil {
		return "UNKNOWN"
	}
	switch op {
	case ">=":
		if observed >= v {
			return "PASS"
		}
	case "<=":
		if observed <= v {
			return "PASS"
		}
	case ">":
		if observed > v {
			return "PASS"
		}
	case "<":
		if observed < v {
			return "PASS"
		}
	case "=":
		if math.Abs(observed-v) < 1e-9 {
			return "PASS"
		}
	default:
		return "UNKNOWN"
	}
	return "FAIL"
}

func parseNumericToken(spec string) (float64, error) {
	spec = strings.TrimSpace(spec)
	// Strip trailing % and other unit hints; for "%" we convert
	// to a fraction so the comparison aligns with our convention
	// of storing ratios as 0.15 = 15%.
	hadPercent := false
	if strings.HasSuffix(spec, "%") {
		hadPercent = true
		spec = strings.TrimSuffix(spec, "%")
	}
	for _, suffix := range []string{"亿", "万", "x", "X"} {
		spec = strings.TrimSuffix(spec, suffix)
	}
	spec = strings.TrimSpace(spec)
	v, err := strconv.ParseFloat(spec, 64)
	if err != nil {
		return 0, err
	}
	if hadPercent {
		return v / 100, nil
	}
	return v, nil
}

func numericFromMap(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case string:
		if f, err := parseNumericToken(t); err == nil {
			return f, true
		}
	}
	return 0, false
}

func stringifyThreshold(threshold any) string {
	switch t := threshold.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case map[string]any:
		minV, hasMin := numericFromMap(t, "min")
		maxV, hasMax := numericFromMap(t, "max")
		switch {
		case hasMin && hasMax:
			return fmt.Sprintf("min=%v max=%v", minV, maxV)
		case hasMin:
			return fmt.Sprintf(">=%v", minV)
		case hasMax:
			return fmt.Sprintf("<=%v", maxV)
		}
	}
	return fmt.Sprintf("%v", threshold)
}

func numericThreshold(threshold any) (float64, bool) {
	switch t := threshold.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case string:
		if f, err := parseNumericToken(t); err == nil {
			return f, true
		}
	}
	return 0, false
}

func historicalSpanYears(history []YearlyMetricsLite) int {
	if len(history) == 0 {
		return 0
	}
	years := make(map[int]struct{}, len(history))
	for _, y := range history {
		if y.Year != 0 {
			years[y.Year] = struct{}{}
		}
	}
	return len(years)
}
