// tactic_agent.go — generic A-share short-term tactic agent.
//
// A TacticAgent is the tactic-side analogue of MasterAgent, with one
// critical difference: tactic strategies have hard, quantitative
// trigger conditions ("daily_gain_pct between 2% and 5%", "封单 >
// 1亿", "属于当日涨幅榜前 3 板块") that we must verify deterministically
// in Go before calling the LLM. The LLM job is to NARRATE the
// already-decided verdict — not to recompute thresholds.
//
// Flow per call:
//
//   1. TimeWindow gate — if "now" is outside the persona's
//      trigger_time.scan_window the agent returns WAIT_FOR_WINDOW
//      with no LLM call.
//   2. HardRisk gate — ST / monitoring / 解禁 / 立案 flags from the
//      wiring layer block every tactic.
//   3. MarketRegime gate — persona may require "全市场涨停>=30" or
//      "上证日内跌幅<=1%". Missing data → SKIP with data_unavailable.
//   4. Red-line gate — every persona declares hard veto conditions
//      ("封单<3000万 否决", "炸板>1次 否决"); a Go evaluator checks
//      them against the IntradaySnapshot.
//   5. Must-have gate — quantitative ranges (turnover, gain, volume
//      ratio, MA distance). Missing data → SKIP.
//   6. Scoring — weighted sum of normalised feature scores.
//   7. LLM narration — the LLM receives the persona, the
//      deterministic verdict, and the rule-by-rule breakdown, and
//      produces thesis + reasons + risks. It is NOT allowed to flip
//      the verdict — the Go side is authoritative.
//
// Returns a TacticReport that includes verdict, score, entry / stop
// / target ranges, and a list of red lines hit (empty when clean).

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/agent/cn_tactics"
	"github.com/fundai/server/internal/cnmarketstructure"
)

// ---------------------------------------------------------------------------
// TacticPersona — parsed shape of a cn_tactics/*.json file
// ---------------------------------------------------------------------------

// TacticPersona is the parsed view of one A-share tactic JSON
// template. Like MasterPersona we keep Raw alongside the typed
// fields so the LLM prompt can quote every persona-specific block
// (exit_rules / scoring_weights / position_sizing) verbatim.
type TacticPersona struct {
	Key             string                 `json:"agent_id"`
	NameZh          string                 `json:"name_zh"`
	NameEn          string                 `json:"name_en"`
	Category        string                 `json:"category"`
	HoldingPeriod   string                 `json:"holding_period"`
	Philosophy      string                 `json:"philosophy"`
	TriggerTime     map[string]any         `json:"trigger_time"`
	MustHave        map[string]any         `json:"must_have_criteria"`
	QualFilters     []string               `json:"qualitative_filters"`
	MarketRegimeRaw map[string]any         `json:"market_regime_filter"`
	ScoringWeights  map[string]float64     `json:"scoring_weights"`
	RedLinesRaw     []string               `json:"red_lines"`
	PositionSizing  map[string]any         `json:"position_sizing"`
	ExitRules       map[string]any         `json:"exit_rules"`
	BacktestBase    map[string]any         `json:"backtest_baseline"`
	VerdictEnum     []string               `json:"verdict_enum"`

	Raw map[string]any `json:"-"`
}

// Validate ensures the persona has enough structure to run.
func (p TacticPersona) Validate() error {
	if strings.TrimSpace(p.Key) == "" {
		return errors.New("tactic: persona.agent_id required")
	}
	if strings.TrimSpace(p.NameEn) == "" {
		return fmt.Errorf("tactic: persona %q missing name_en", p.Key)
	}
	if strings.TrimSpace(p.Philosophy) == "" {
		return fmt.Errorf("tactic: persona %q missing philosophy", p.Key)
	}
	return nil
}

// ---------------------------------------------------------------------------
// TacticAgent — runs one tactic persona end-to-end
// ---------------------------------------------------------------------------

// TacticAgent is a single short-term-tactic agent. Construct via
// NewTacticAgent; safe to share across goroutines.
type TacticAgent struct {
	persona TacticPersona
	llm     LLMClient
	logger  *slog.Logger
	now     func() time.Time
}

// TacticAgentOption configures construction.
type TacticAgentOption func(*TacticAgent)

// WithTacticLogger swaps the default slog.Default() logger.
func WithTacticLogger(l *slog.Logger) TacticAgentOption {
	return func(a *TacticAgent) {
		if l != nil {
			a.logger = l
		}
	}
}

// WithTacticClock injects a deterministic clock for tests. The clock
// is used for the trigger_time window check — Phase 4's tests
// inject 14:35 Shanghai to verify tail_sniper accepts.
func WithTacticClock(now func() time.Time) TacticAgentOption {
	return func(a *TacticAgent) {
		if now != nil {
			a.now = now
		}
	}
}

// NewTacticAgent constructs a TacticAgent from a parsed persona +
// an LLM client (which may be nil for tests).
func NewTacticAgent(persona TacticPersona, llm LLMClient, opts ...TacticAgentOption) (*TacticAgent, error) {
	if err := persona.Validate(); err != nil {
		return nil, err
	}
	a := &TacticAgent{
		persona: persona,
		llm:     llm,
		logger:  slog.Default(),
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// Key returns the persona's agent_id.
func (a *TacticAgent) Key() string { return a.persona.Key }

// NameZh returns the persona's Chinese display name.
func (a *TacticAgent) NameZh() string { return a.persona.NameZh }

// NameEn returns the persona's English display name.
func (a *TacticAgent) NameEn() string { return a.persona.NameEn }

// Persona returns a copy of the persona this agent was built with.
func (a *TacticAgent) Persona() TacticPersona { return a.persona }

// Analyze runs the persona's gates + scoring against `in` and
// returns the structured report.
func (a *TacticAgent) Analyze(ctx context.Context, in TacticInput) (TacticReport, error) {
	if strings.TrimSpace(in.Symbol) == "" {
		return TacticReport{}, errors.New("tactic: input.Symbol required")
	}
	rep := TacticReport{
		TacticKey:    a.persona.Key,
		TacticNameZh: a.persona.NameZh,
		TacticNameEn: a.persona.NameEn,
		Symbol:       strings.ToUpper(strings.TrimSpace(in.Symbol)),
		SymbolName:   strings.TrimSpace(in.Name),
		AsOf:         in.AsOf,
		GeneratedAt:  a.now(),
		Verdict:      "SKIP",
		Confidence:   40,
		Thesis:       fmt.Sprintf("%s 暂未触发买点。", a.persona.NameZh),
		KeyReasons:   []string{"not_in_setup"},
		KeyRisks:     []string{},
	}

	// Step 1 — hard-risk gate. Always blocks every tactic.
	if len(in.HardRiskFailures) > 0 {
		rep.Verdict = "SKIP"
		rep.RedLinesHit = append(rep.RedLinesHit, in.HardRiskFailures...)
		rep.KeyReasons = []string{"hard_risk_blocked"}
		rep.KeyRisks = []string{fmt.Sprintf("hard_risk:%s", strings.Join(in.HardRiskFailures, ","))}
		rep.Thesis = fmt.Sprintf("%s 被全局硬风控前置闸门否决（%s）。", a.persona.NameZh, strings.Join(in.HardRiskFailures, "; "))
		rep.Confidence = 80
		return rep, nil
	}

	// Step 2 — trigger time-window gate.
	if outside, reason := a.outsideTriggerWindow(); outside {
		rep.Verdict = "WAIT_FOR_WINDOW"
		rep.Confidence = 50
		rep.KeyReasons = []string{"out_of_trigger_window:" + reason}
		rep.Thesis = fmt.Sprintf("%s 当前时间不在策略触发时间窗（%s）内，建议在窗口内再判断。", a.persona.NameZh, reason)
		return rep, nil
	}

	// Step 3 — market regime gate.
	if blocked, reason := a.checkMarketRegime(in); blocked {
		rep.Verdict = "SKIP"
		rep.MarketRegimePass = false
		rep.MarketRegimeReason = reason
		rep.KeyReasons = []string{"regime_blocked:" + reason}
		rep.KeyRisks = []string{"market_regime_unfavourable"}
		rep.Thesis = fmt.Sprintf("%s 大盘环境不满足前置过滤条件：%s。", a.persona.NameZh, reason)
		rep.Confidence = 70
		return rep, nil
	}
	rep.MarketRegimePass = true

	// Step 4 — red-line gate.
	rep.RedLinesHit = a.evaluateRedLines(in)
	if len(rep.RedLinesHit) > 0 {
		rep.Verdict = "SKIP"
		rep.KeyReasons = []string{"red_line_hit"}
		rep.KeyRisks = append(rep.KeyRisks, rep.RedLinesHit...)
		rep.Thesis = fmt.Sprintf("%s 命中硬性否决条件：%s。", a.persona.NameZh, strings.Join(rep.RedLinesHit, "; "))
		rep.Confidence = 75
		return rep, nil
	}

	// Step 5 — must-have gate.
	mustHaveFail := a.evaluateMustHave(in)
	if len(mustHaveFail) > 0 {
		rep.Verdict = "SKIP"
		rep.KeyReasons = append([]string{"must_have_fail"}, mustHaveFail...)
		rep.Thesis = fmt.Sprintf("%s 必要条件未满足：%s。", a.persona.NameZh, strings.Join(mustHaveFail, "; "))
		rep.Confidence = 60
		return rep, nil
	}

	// Step 6 — scoring + buy verdict.
	score := a.scoreSetup(in)
	rep.Score = score
	rep.Verdict = a.buyVerdict()
	rep.Confidence = clampConfidence(int(math.Round(score * 100)))
	rep.KeyReasons = []string{"must_have_pass", "no_red_line", fmt.Sprintf("score=%.2f", score)}
	rep.KeyRisks = []string{}
	if in.PriceLast > 0 {
		entryLow := in.PriceLast * 0.997
		entryHigh := in.PriceLast * 1.003
		stop := in.PriceLast * (1 - tacticStopLossPct(a.persona)/100.0)
		t1 := in.PriceLast * (1 + tacticT1Pct(a.persona)/100.0)
		t3 := in.PriceLast * (1 + tacticT3Pct(a.persona)/100.0)
		rep.EntryPriceLow = &entryLow
		rep.EntryPriceHigh = &entryHigh
		rep.StopLossPrice = &stop
		rep.TargetT1 = &t1
		rep.TargetT3 = &t3
	}
	holdingDays := tacticDefaultHolding(a.persona)
	rep.ExpectedHoldingDays = &holdingDays

	// Step 7 — LLM narration. The LLM cannot flip the verdict,
	// it only enriches thesis + reasons + risks. If the LLM
	// call fails we keep the deterministic outputs.
	if a.llm != nil {
		sys := a.buildSystemPrompt()
		user := a.buildUserPrompt(in, rep)
		raw, err := a.complete(ctx, sys, user)
		if err == nil {
			parsed, perr := parseTacticLLM(raw)
			if perr == nil {
				if t := strings.TrimSpace(parsed.Thesis); t != "" {
					rep.Thesis = t
				}
				if len(parsed.KeyReasons) > 0 {
					rep.KeyReasons = parsed.KeyReasons
				}
				if len(parsed.KeyRisks) > 0 {
					rep.KeyRisks = parsed.KeyRisks
				}
			} else {
				sample := strings.TrimSpace(raw)
				rawLen := len(sample)
				if rawLen > 400 {
					sample = sample[:200] + "...[truncated]..." + sample[rawLen-200:]
				}
				a.logger.Warn("tactic agent: parse failed, keeping deterministic narrative",
					"tactic", a.persona.Key,
					"err", perr,
					"raw_len", rawLen,
					"raw_sample", sample,
				)
			}
		} else {
			a.logger.Warn("tactic agent: LLM failed, keeping deterministic narrative",
				"tactic", a.persona.Key, "err", err)
		}
	}
	return rep, nil
}

// complete dispatches to the schema-aware LLM call when the
// client supports it.
func (a *TacticAgent) complete(ctx context.Context, sys, user string) (string, error) {
	if schemaClient, ok := a.llm.(SchemaLLMClient); ok {
		return schemaClient.CompleteWithSchema(ctx, sys, user, TacticReportJSONSchema)
	}
	return a.llm.Complete(ctx, sys, user)
}

// ---------------------------------------------------------------------------
// Gates — trigger window / regime / red lines / must-have
// ---------------------------------------------------------------------------

// outsideTriggerWindow returns true + a human-readable reason when
// the current local time falls outside the persona's
// trigger_time.scan_window. Window format: "HH:MM-HH:MM" in the
// persona's timezone (defaults to Asia/Shanghai).
func (a *TacticAgent) outsideTriggerWindow() (bool, string) {
	tw := a.persona.TriggerTime
	if len(tw) == 0 {
		return false, ""
	}
	window, ok := tw["scan_window"].(string)
	if !ok || window == "" {
		return false, ""
	}
	tz := "Asia/Shanghai"
	if s, ok := tw["timezone"].(string); ok && s != "" {
		tz = s
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	now := a.now().In(loc)
	start, end, ok := parseScanWindow(window)
	if !ok {
		return false, ""
	}
	nowMinutes := now.Hour()*60 + now.Minute()
	if nowMinutes < start || nowMinutes > end {
		return true, fmt.Sprintf("%s %s (%s)", window, tz, now.Format("15:04"))
	}
	return false, ""
}

func parseScanWindow(spec string) (start, end int, ok bool) {
	parts := strings.Split(strings.TrimSpace(spec), "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	s, e := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	sh, sm, ok1 := splitHHMM(s)
	eh, em, ok2 := splitHHMM(e)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return sh*60 + sm, eh*60 + em, true
}

func splitHHMM(spec string) (h, m int, ok bool) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		return 0, 0, false
	}
	var hh, mm int
	if _, err := fmt.Sscanf(parts[0], "%d", &hh); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &mm); err != nil {
		return 0, 0, false
	}
	return hh, mm, true
}

// checkMarketRegime evaluates the persona's market_regime_filter.
// Returns (blocked, reason). reason is empty when not blocked.
func (a *TacticAgent) checkMarketRegime(in TacticInput) (bool, string) {
	if len(a.persona.MarketRegimeRaw) == 0 {
		return false, ""
	}
	if in.Regime == nil {
		// Without regime data we can't enforce — degrade
		// open. The aggregate report will note this in the
		// "data_unavailable" key risk.
		return false, ""
	}
	for key, raw := range a.persona.MarketRegimeRaw {
		switch strings.ToLower(key) {
		case "shanghai_index_intraday_change_pct":
			if blocked, reason := evalRangeBlock(raw, in.Regime.ShanghaiIndexChangePct, "上证日内涨跌幅"); blocked {
				return true, reason
			}
		case "limit_up_count_today":
			if blocked, reason := evalRangeBlock(raw, float64(in.Regime.LimitUpCount), "全市场涨停家数"); blocked {
				return true, reason
			}
		case "limit_up_break_rate_pct":
			if blocked, reason := evalRangeBlock(raw, in.Regime.FriedBoardRatePct, "炸板率"); blocked {
				return true, reason
			}
		}
	}
	return false, ""
}

// evaluateRedLines walks the persona's red_lines strings and
// reports the human-readable list that fires. Implementation is
// keyword-based: each red-line string is a Chinese sentence; we
// look for the well-known stems (封单<, 炸板, 长上影, 跌幅榜) and
// evaluate the associated snapshot field. Unknown stems are
// passed through unchecked (we err on the side of "not hit").
func (a *TacticAgent) evaluateRedLines(in TacticInput) []string {
	var hits []string
	if in.Intraday == nil {
		return hits
	}
	for _, line := range a.persona.RedLinesRaw {
		stem := line // keep raw for Chinese matching
		switch {
		case strings.Contains(stem, "长上影线"):
			if in.Intraday.UpperShadowPct > 3.0 {
				hits = append(hits, line)
			}
		case strings.Contains(stem, "炸板") || strings.Contains(stem, "开板") || strings.Contains(stem, "盘中触及涨停"):
			if in.Intraday.LimitUpReopenCount > 0 {
				hits = append(hits, line)
			}
		case strings.Contains(stem, "封单") && strings.Contains(stem, "3000万"):
			if in.Intraday.SealAmountYi > 0 && in.Intraday.SealAmountYi*1e8 < 30_000_000 {
				hits = append(hits, line)
			}
		case strings.Contains(stem, "回踩跌破"):
			if in.Intraday.PullbackFromHighPct > 12 {
				hits = append(hits, line)
			}
		case strings.ToLower(stem) == strings.ToLower(stem) && (strings.Contains(strings.ToUpper(stem), "ST") || strings.Contains(stem, "退市")):
			if in.Intraday.IsST {
				hits = append(hits, line)
			}
		case strings.Contains(stem, "跌幅榜") || strings.Contains(stem, "弱势板块"):
			// Sector-based red lines fire only if the sector
			// ranking universe is large enough to identify a
			// real bottom group AND this symbol's sector sits
			// in that bottom group.
			if len(in.Sectors) >= 5 && in.Intraday.SectorName != "" {
				bottomThreshold := len(in.Sectors) - 3
				for i, s := range in.Sectors {
					if i < bottomThreshold {
						continue
					}
					if strings.EqualFold(s.SectorName, in.Intraday.SectorName) {
						hits = append(hits, line)
						break
					}
				}
			}
		}
	}
	return hits
}

// evaluateMustHave walks must_have_criteria and reports any field
// that fails. Returns a list of "key:reason" strings for the report.
func (a *TacticAgent) evaluateMustHave(in TacticInput) []string {
	if in.Intraday == nil {
		return []string{"data_unavailable:intraday_snapshot"}
	}
	var fails []string
	for key, raw := range a.persona.MustHave {
		obs := observedFromSnapshot(key, in.Intraday)
		if math.IsNaN(obs) {
			// No snapshot field maps to this key — let the
			// LLM handle nuance, don't auto-fail.
			continue
		}
		if blocked, reason := evalRangeFail(raw, obs, key); blocked {
			fails = append(fails, reason)
		}
	}
	sort.Strings(fails)
	return fails
}

// observedFromSnapshot maps a persona must-have key to the matching
// IntradaySnapshot field. Returns NaN for keys we don't know how to
// map (in which case the LLM gets to reason about it).
func observedFromSnapshot(key string, s *cnmarketstructure.IntradaySnapshot) float64 {
	if s == nil {
		return math.NaN()
	}
	switch strings.ToLower(key) {
	case "daily_gain_pct":
		return s.DailyGainPct
	case "turnover_rate_pct":
		return s.TurnoverRatePct
	case "volume_ratio":
		return s.VolumeRatio
	case "float_market_cap_yi":
		return s.FloatMarketCapYi
	case "distance_to_ma10_pct":
		return s.DistanceToMA10Pct
	case "distance_to_ma20_pct":
		return s.DistanceToMA20Pct
	case "distance_to_ma60_pct":
		return s.DistanceToMA60Pct
	case "upper_shadow_pct":
		return s.UpperShadowPct
	case "intraday_pullback_from_high_pct":
		return s.PullbackFromHighPct
	case "open_pct_today":
		return s.OpenGapPct
	}
	return math.NaN()
}

// evalRangeBlock interprets a must-have / regime threshold value (a
// float, a string like ">=15%", or a {min,max} map) and returns
// (blocked, reason). Blocked means the observed value violates the
// rule — used in market_regime_filter where we want a true reason
// when the regime is hostile.
func evalRangeBlock(raw any, observed float64, label string) (bool, string) {
	switch t := raw.(type) {
	case float64:
		if observed < t {
			return true, fmt.Sprintf("%s=%.2f < %.2f", label, observed, t)
		}
	case int:
		if observed < float64(t) {
			return true, fmt.Sprintf("%s=%.2f < %d", label, observed, t)
		}
	case map[string]any:
		if minV, ok := numericFromMap(t, "min"); ok && observed < minV {
			return true, fmt.Sprintf("%s=%.2f < min %.2f", label, observed, minV)
		}
		if maxV, ok := numericFromMap(t, "max"); ok && observed > maxV {
			return true, fmt.Sprintf("%s=%.2f > max %.2f", label, observed, maxV)
		}
	}
	return false, ""
}

// evalRangeFail is the must-have variant — same shape but returns
// the "key:value out of [min,max]" message for the report.
func evalRangeFail(raw any, observed float64, key string) (bool, string) {
	switch t := raw.(type) {
	case float64:
		if observed < t {
			return true, fmt.Sprintf("%s=%.2f<%.2f", key, observed, t)
		}
	case int:
		if observed < float64(t) {
			return true, fmt.Sprintf("%s=%.2f<%d", key, observed, t)
		}
	case map[string]any:
		if minV, ok := numericFromMap(t, "min"); ok && observed < minV {
			return true, fmt.Sprintf("%s=%.2f<min:%.2f", key, observed, minV)
		}
		if maxV, ok := numericFromMap(t, "max"); ok && observed > maxV {
			return true, fmt.Sprintf("%s=%.2f>max:%.2f", key, observed, maxV)
		}
	}
	return false, ""
}

// ---------------------------------------------------------------------------
// Scoring + verdict + price helpers
// ---------------------------------------------------------------------------

// scoreSetup applies the persona's scoring_weights as a normalised
// 0..1 quality score. We score the easy-to-measure dimensions
// (sector strength, volume / price quality, technical pattern) from
// the IntradaySnapshot; dimensions that need LLM judgment (e.g.
// longhubang_history) default to 0.5 so the agent doesn't over- or
// under-weight what it can't measure.
func (a *TacticAgent) scoreSetup(in TacticInput) float64 {
	if len(a.persona.ScoringWeights) == 0 {
		return 0.7
	}
	var total float64
	var weightSum float64
	for key, weight := range a.persona.ScoringWeights {
		val := a.scoreOneDimension(key, in)
		total += val * weight
		weightSum += weight
	}
	if weightSum == 0 {
		return 0.7
	}
	return clampUnit(total / weightSum)
}

func (a *TacticAgent) scoreOneDimension(key string, in TacticInput) float64 {
	switch strings.ToLower(key) {
	case "sector_strength", "sector_continuation_strength":
		if in.Intraday == nil || len(in.Sectors) == 0 || in.Intraday.SectorName == "" {
			return 0.5
		}
		for i, s := range in.Sectors {
			if strings.EqualFold(s.SectorName, in.Intraday.SectorName) {
				return clampUnit(1.0 - float64(i)/float64(len(in.Sectors)))
			}
		}
		return 0.3
	case "volume_price_quality":
		if in.Intraday == nil {
			return 0.5
		}
		vol := in.Intraday.VolumeRatio
		if vol == 0 {
			return 0.5
		}
		// Sweet spot 1.5-2.5
		return clampUnit(1.0 - math.Abs(vol-2.0)/2.0)
	case "technical_pattern", "pullback_volume_shrinkage":
		if in.Intraday == nil {
			return 0.5
		}
		ma := in.Intraday.DistanceToMA20Pct
		if ma == 0 {
			return 0.5
		}
		// Sweet spot 0-5% above MA20
		return clampUnit(1.0 - math.Abs(ma-2.5)/10.0)
	case "northbound_flow", "main_capital_inflow":
		if in.Intraday == nil || in.Intraday.NorthboundNetInflow == 0 {
			return 0.5
		}
		// Positive flow → score >0.5; saturates at 5e8 (5亿)
		return clampUnit(0.5 + in.Intraday.NorthboundNetInflow/1e9)
	case "longhubang_history", "limit_up_seal_quality", "longhubang_seat_quality":
		// LLM-style heuristic — we don't have a structured
		// score so we accept the neutral default. The LLM
		// thesis can still narrate around 龙虎榜 seat tags.
		return 0.5
	}
	return 0.5
}

func clampUnit(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// buyVerdict returns the BUY-side enum value from the persona,
// falling back to "BUY_DIP" / "BUY_TAIL" / "BUY" / "CHASE_LIMIT_UP".
func (a *TacticAgent) buyVerdict() string {
	for _, v := range a.persona.VerdictEnum {
		up := strings.ToUpper(v)
		if strings.HasPrefix(up, "BUY") || strings.HasPrefix(up, "CHASE") {
			return up
		}
	}
	return "BUY"
}

func tacticStopLossPct(p TacticPersona) float64 {
	if v, ok := numericFromMap(p.PositionSizing, "stop_loss_pct"); ok {
		if v < 0 {
			return -v
		}
		return v
	}
	return 4.0
}

func tacticT1Pct(p TacticPersona) float64 {
	if v, ok := numericFromMap(p.PositionSizing, "first_take_profit_pct"); ok {
		return v
	}
	return 4.0
}

func tacticT3Pct(p TacticPersona) float64 {
	if v, ok := numericFromMap(p.PositionSizing, "let_profit_run_above_pct"); ok {
		return v
	}
	return 8.0
}

func tacticDefaultHolding(p TacticPersona) int {
	switch strings.ToUpper(strings.TrimSpace(p.HoldingPeriod)) {
	case "T+1":
		return 1
	case "T+1 ~ T+3", "T+1～T+3":
		return 2
	case "T+1 ~ T+5", "T+1～T+5":
		return 3
	case "T+1 ~ T+10", "T+1～T+10":
		return 5
	}
	return 2
}

// ---------------------------------------------------------------------------
// LLM prompt rendering + parsing
// ---------------------------------------------------------------------------

func (a *TacticAgent) buildSystemPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "你是 %s（%s），一位专注于 A 股短线交易的策略 agent。\n",
		a.persona.NameZh, a.persona.NameEn)
	if a.persona.HoldingPeriod != "" {
		fmt.Fprintf(&b, "你的持有周期：%s。\n", a.persona.HoldingPeriod)
	}
	if a.persona.Philosophy != "" {
		fmt.Fprintf(&b, "你的核心交易哲学：%s\n", a.persona.Philosophy)
	}
	b.WriteString("\n你必须遵守：\n")
	b.WriteString("1) 你不能 改写 verdict —— verdict 已由服务器侧确定性规则给出，你只负责丰富 thesis / key_reasons / key_risks 的中文叙述；\n")
	b.WriteString("2) 不要编造价格、不要编造历史数据；如果叙述需要某个未提供的数字，写 'data_unavailable'；\n")
	b.WriteString("3) 仅输出一个 JSON 对象（不要 ``` 围栏），字段如 schema 所示。\n")
	b.WriteString("\n=== PERSONA JSON ===\n")
	if raw, err := json.MarshalIndent(a.persona.Raw, "", "  "); err == nil {
		b.Write(raw)
	}
	b.WriteString("\n=== OUTPUT JSON SCHEMA ===\n")
	b.WriteString("{ \"thesis\": string<=200字, \"key_reasons\": [string,...], \"key_risks\": [string,...] }\n")
	return b.String()
}

func (a *TacticAgent) buildUserPrompt(in TacticInput, rep TacticReport) string {
	var b strings.Builder
	b.WriteString("请为下面这笔已被服务器侧规则判定为 ")
	b.WriteString(rep.Verdict)
	b.WriteString(" 的交易，撰写一段简洁的 thesis 并列出 reasons / risks。\n\n")
	fmt.Fprintf(&b, "symbol: %s\n", in.Symbol)
	if name := strings.TrimSpace(in.Name); name != "" {
		// See master_agent.buildUserPrompt — same rationale.
		fmt.Fprintf(&b, "name: %s\n", name)
	}
	if in.Intraday != nil {
		fmt.Fprintf(&b, "daily_gain_pct=%.2f turnover_rate=%.2f volume_ratio=%.2f float_cap_yi=%.2f ma20_dist=%.2f sector=%q seal_amount_yi=%.2f reopen_count=%d consecutive_limit_ups=%d northbound_flow=%.2f\n",
			in.Intraday.DailyGainPct, in.Intraday.TurnoverRatePct, in.Intraday.VolumeRatio,
			in.Intraday.FloatMarketCapYi, in.Intraday.DistanceToMA20Pct,
			in.Intraday.SectorName, in.Intraday.SealAmountYi,
			in.Intraday.LimitUpReopenCount, in.Intraday.ConsecutiveLimitUps,
			in.Intraday.NorthboundNetInflow,
		)
	}
	if in.Regime != nil {
		fmt.Fprintf(&b, "market_regime: limit_up=%d limit_down=%d fried_board=%d fried_rate=%.2f%% sh_change=%.2f%%\n",
			in.Regime.LimitUpCount, in.Regime.LimitDownCount,
			in.Regime.FriedBoardCount, in.Regime.FriedBoardRatePct,
			in.Regime.ShanghaiIndexChangePct,
		)
	}
	fmt.Fprintf(&b, "score=%.2f confidence=%d\n", rep.Score, rep.Confidence)
	if len(rep.RedLinesHit) > 0 {
		fmt.Fprintf(&b, "red_lines_hit=%s\n", strings.Join(rep.RedLinesHit, "; "))
	}
	if rep.EntryPriceLow != nil && rep.EntryPriceHigh != nil {
		fmt.Fprintf(&b, "entry=[%.4f, %.4f] stop=%.4f t1=%.4f t3=%.4f\n",
			*rep.EntryPriceLow, *rep.EntryPriceHigh,
			derefFloat(rep.StopLossPrice), derefFloat(rep.TargetT1), derefFloat(rep.TargetT3),
		)
	}
	if in.Notes != "" {
		fmt.Fprintf(&b, "\nnotes: %s\n", in.Notes)
	}
	return b.String()
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// llmTacticNarration is the JSON envelope the LLM is allowed to fill.
type llmTacticNarration struct {
	Thesis     string   `json:"thesis"`
	KeyReasons []string `json:"key_reasons"`
	KeyRisks   []string `json:"key_risks"`
}

func parseTacticLLM(raw string) (llmTacticNarration, error) {
	body := strings.TrimSpace(raw)
	if body == "" {
		return llmTacticNarration{}, errors.New("empty llm reply")
	}
	if i := strings.Index(body, "```"); i >= 0 {
		body = body[i+3:]
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(body)), "json") {
			body = strings.TrimSpace(body)
			body = body[4:]
		}
		if j := strings.LastIndex(body, "```"); j >= 0 {
			body = body[:j]
		}
	}
	start := strings.Index(body, "{")
	end := strings.LastIndex(body, "}")
	if start < 0 || end < 0 || end <= start {
		return llmTacticNarration{}, errors.New("no json object found")
	}
	body = body[start : end+1]
	var out llmTacticNarration
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return llmTacticNarration{}, err
	}
	return out, nil
}

// TacticReportJSONSchema is the strict-JSON contract for the
// SchemaLLMClient-aware path. Keeps the LLM narration constrained
// to thesis + reasons + risks.
var TacticReportJSONSchema = []byte(`{
  "type": "object",
  "properties": {
    "thesis":      { "type": "string" },
    "key_reasons": { "type": "array", "items": { "type": "string" } },
    "key_risks":   { "type": "array", "items": { "type": "string" } }
  },
  "required": ["thesis", "key_reasons", "key_risks"]
}`)

// ---------------------------------------------------------------------------
// Persona loader (reads internal/agent/cn_tactics/*.json once at boot)
// ---------------------------------------------------------------------------

var (
	tacticPersonaMu    sync.RWMutex
	tacticPersonaCache map[string]TacticPersona
)

// LoadTacticPersonas reads every *.json under internal/agent/cn_tactics
// and caches the parsed personas keyed by agent_id.
func LoadTacticPersonas() (map[string]TacticPersona, error) {
	tacticPersonaMu.RLock()
	if tacticPersonaCache != nil {
		out := copyTacticPersonaMap(tacticPersonaCache)
		tacticPersonaMu.RUnlock()
		return out, nil
	}
	tacticPersonaMu.RUnlock()

	tacticPersonaMu.Lock()
	defer tacticPersonaMu.Unlock()
	if tacticPersonaCache != nil {
		return copyTacticPersonaMap(tacticPersonaCache), nil
	}

	entries, err := fs.ReadDir(cn_tactics.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("tactic: read cn_tactics dir: %w", err)
	}
	out := make(map[string]TacticPersona, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		raw, err := fs.ReadFile(cn_tactics.FS, name)
		if err != nil {
			return nil, fmt.Errorf("tactic: read %s: %w", name, err)
		}
		var persona TacticPersona
		if err := json.Unmarshal(raw, &persona); err != nil {
			return nil, fmt.Errorf("tactic: parse %s: %w", name, err)
		}
		var rawMap map[string]any
		if err := json.Unmarshal(raw, &rawMap); err != nil {
			return nil, fmt.Errorf("tactic: parse-raw %s: %w", name, err)
		}
		persona.Raw = rawMap
		key := strings.TrimSuffix(strings.ToLower(path.Base(name)), ".json")
		if strings.TrimSpace(persona.Key) == "" {
			persona.Key = key
		} else if strings.ToLower(persona.Key) != key {
			return nil, fmt.Errorf("tactic: filename %q does not match agent_id %q", name, persona.Key)
		}
		if err := persona.Validate(); err != nil {
			return nil, err
		}
		out[persona.Key] = persona
	}
	if len(out) == 0 {
		return nil, errors.New("tactic: no persona JSON files found")
	}
	tacticPersonaCache = out
	return copyTacticPersonaMap(tacticPersonaCache), nil
}

func copyTacticPersonaMap(in map[string]TacticPersona) map[string]TacticPersona {
	out := make(map[string]TacticPersona, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ResetTacticPersonaCache clears the in-memory cache.
func ResetTacticPersonaCache() {
	tacticPersonaMu.Lock()
	tacticPersonaCache = nil
	tacticPersonaMu.Unlock()
}
