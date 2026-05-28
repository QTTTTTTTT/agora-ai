package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Sprint 3 / M1: lesson PnL lineage.
//
// "学习闭环可信" 的三个挑战：
//
//  1. 我们让 LLM 写 lessons，但从来没有验证它们事后是否 actually 兑现 —
//     既无统计 hit rate，也无办法在 prompt 里给某条 lesson 标 "历史 0/5 命中"。
//  2. lessons 的文本是自由格式，事后想 score 需要重新解析。
//  3. 写入和 score 不能在同一个 LLM 调用里完成，因为预测窗口（默认 7 天）
//     还没结束。
//
// 本文件解决这三件事：
//
//  * `extractLessonHypothesis` 把一条裸 lesson 文本走一遍轻量级 regex/heuristic，
//    抽出 {symbol, direction, window_days}。失败返回 nil — 不是所有 lesson
//    都对得到一条可证伪的假设（例如 "组合整体应继续观望"），那种就跳过。
//
//  * `recordLessonLineage` 在 daily review 写完 memories 之后被调用，
//    把每条命中的 hypothesis 落到 lesson_pnl_lineage 表里，挂上
//    window_close_at = trading_date + window_days。
//
//  * `lessonScoringLoop` 是一个 leader-gated 后台 goroutine，每小时
//    扫一次 "window_close_at < now AND observed_at IS NULL" 的行，
//    去 nav_snapshots 拉 trading_date → window_close_at 之间的累计
//    收益率，按方向算 score (+1 = 完全 vindicated, -1 = 反向)，写入。
//
// 设计取舍：
//  * 我们不走 LLM 二次评分（贵且不必要）；用确定性公式即可。
//  * 仅在 NavSnapshot 存在时打分；股票级 lineage 暂用 fund 级 NAV 作为代理
//    （未来 L 层 intraday 接上后再补 per-symbol）。
//  * 失败一律 slog.Warn 然后继续 — 评分是观察工具，不能因之挂住后台。

// LessonLineageLeaseName gates 多副本下只有一个 leader 跑评分。
const LessonLineageLeaseName = "lesson-lineage-scorer"

// lessonLineageInsert 是写一行 lineage 需要的最小信息。
type lessonLineageInsert struct {
	LessonMemoryID       string
	FundID               string
	AgentID              sql.NullString
	Symbol               sql.NullString
	Hypothesis           string
	PredictedDirection   int
	HypothesisWindowDays int
	WindowCloseAt        time.Time
}

// extractedHypothesis 是把一条 lesson 文本解析后得到的可证伪假设。
type extractedHypothesis struct {
	Symbol             string // 可能为空（组合级假设）
	PredictedDirection int    // +1 / -1 / 0
	WindowDays         int    // 默认 7
}

var (
	// "应继续加仓 / 应进一步减仓 / 看涨 / 看跌 / 增配 / 减配 / buy / sell / long / short ..."
	lessonBullishRE = regexp.MustCompile(`(?i)(加仓|增配|看涨|抄底|做多|长仓|build\s+up|increase\s+(allocation|exposure|position)|add\s+to|buy\b|long\b|bullish|outperform)`)
	lessonBearishRE = regexp.MustCompile(`(?i)(减仓|减配|减少|看跌|做空|空仓|止损|清仓|cut|trim|reduce|sell\b|short\b|bearish|underperform)`)

	// 窗口暗示：明日 / 下周 / 一周 / 1 周 / 7 天 / next session / next week ...
	lessonWindowDailyRE   = regexp.MustCompile(`(?i)(明日|明天|次日|下一(交易)?日|next\s+(session|day))`)
	lessonWindowWeeklyRE  = regexp.MustCompile(`(?i)(一?周内|下周|未来\s*[1-7一二三四五六七]\s*[天日]|next\s+week|over\s+the\s+next\s+week|7\s*days?)`)
	lessonWindowMonthlyRE = regexp.MustCompile(`(?i)(月内|一个月|下月|本月|next\s+month|30\s*days?)`)
)

// extractLessonHypothesis 解析一条 lesson 文本，返回可证伪 hypothesis。
// 没有 symbol 也没有 direction signal 时返回 nil（不值得 lineage）。
func extractLessonHypothesis(lesson string) *extractedHypothesis {
	trimmed := strings.TrimSpace(lesson)
	if trimmed == "" {
		return nil
	}

	out := extractedHypothesis{WindowDays: 7}

	// 方向 — 同一句里同时出现两类词时取 first match 在文本中位置较早的那个；
	// 否则 +0 表示中性（中性单独看 hypothesis 也没意义，下面会兜底丢弃）。
	bullIdx := -1
	bearIdx := -1
	if loc := lessonBullishRE.FindStringIndex(trimmed); loc != nil {
		bullIdx = loc[0]
	}
	if loc := lessonBearishRE.FindStringIndex(trimmed); loc != nil {
		bearIdx = loc[0]
	}
	switch {
	case bullIdx < 0 && bearIdx < 0:
		return nil
	case bullIdx < 0:
		out.PredictedDirection = -1
	case bearIdx < 0:
		out.PredictedDirection = 1
	case bullIdx < bearIdx:
		out.PredictedDirection = 1
	default:
		out.PredictedDirection = -1
	}

	// 窗口 — daily/weekly/monthly 三档；找不到默认 7 天。
	switch {
	case lessonWindowDailyRE.MatchString(trimmed):
		out.WindowDays = 1
	case lessonWindowWeeklyRE.MatchString(trimmed):
		out.WindowDays = 7
	case lessonWindowMonthlyRE.MatchString(trimmed):
		out.WindowDays = 30
	}

	// 提 symbol — 复用 fact-check 那两条 regex（A 股 6 位 / US 1-5 大写）。
	// 取第一个命中即可；symbol 缺失时 lineage 退化为 fund-level proxy。
	if m := lessonSymbolARE.FindStringSubmatch(trimmed); len(m) >= 2 {
		out.Symbol = m[1]
	} else if m := lessonSymbolUSRE.FindStringSubmatch(trimmed); len(m) >= 2 {
		candidate := strings.ToUpper(m[1])
		if _, deny := commonAllCapsWords[candidate]; !deny {
			out.Symbol = candidate
		}
	}

	return &out
}

// recordLessonLineage 把一组 lesson 字符串解析并落到 lesson_pnl_lineage。
// 失败一律 slog.Warn — 这是观察工具，不能阻塞 daily review。
// memoryID 可能为空（test 路径 / memoryRepo 不可用时），此时直接 no-op。
func recordLessonLineage(ctx context.Context, db *sql.DB, memoryID, fundID string, agentID sql.NullString, lessons []string, tradingDate time.Time) {
	if db == nil || strings.TrimSpace(memoryID) == "" || strings.TrimSpace(fundID) == "" {
		return
	}
	if tradingDate.IsZero() {
		tradingDate = time.Now().UTC()
	}
	inserts := make([]lessonLineageInsert, 0, len(lessons))
	for _, lesson := range lessons {
		h := extractLessonHypothesis(lesson)
		if h == nil {
			continue
		}
		windowClose := tradingDate.AddDate(0, 0, h.WindowDays)
		row := lessonLineageInsert{
			LessonMemoryID:       memoryID,
			FundID:               fundID,
			AgentID:              agentID,
			Hypothesis:           lesson,
			PredictedDirection:   h.PredictedDirection,
			HypothesisWindowDays: h.WindowDays,
			WindowCloseAt:        windowClose,
		}
		if h.Symbol != "" {
			row.Symbol = sql.NullString{String: h.Symbol, Valid: true}
		}
		inserts = append(inserts, row)
	}
	if len(inserts) == 0 {
		return
	}
	for _, row := range inserts {
		if _, err := db.ExecContext(ctx, `
INSERT INTO lesson_pnl_lineage (
    lesson_memory_id, fund_id, agent_id, symbol,
    hypothesis, predicted_direction, hypothesis_window_days,
    window_close_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			row.LessonMemoryID, row.FundID, row.AgentID, row.Symbol,
			row.Hypothesis, row.PredictedDirection, row.HypothesisWindowDays,
			row.WindowCloseAt,
		); err != nil {
			// 已经写过同一条（cron 重跑 / 重启）就静默跳过；
			// 其他错误 warn 后继续下一条 — 不要因为单条 lesson 卡住
			// 整个 daily review。
			slog.Warn("lesson lineage: insert failed",
				"fund_id", row.FundID,
				"memory_id", row.LessonMemoryID,
				"window_days", row.HypothesisWindowDays,
				"err", err,
			)
			continue
		}
	}
}

// dueLessonLineage 是 worker 待处理的一行。
type dueLessonLineage struct {
	ID                   int64
	FundID               string
	Symbol               sql.NullString
	PredictedDirection   int
	HypothesisWindowDays int
	WindowOpen           time.Time
	WindowClose          time.Time
}

// lessonScoringLoop 评分 worker。每小时扫一次到期 lineage，
// 按方向 × NAV 累计收益率算 score，写回观察值。
type lessonScoringLoop struct {
	db       *sql.DB
	leader   leaderChecker
	interval time.Duration

	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
}

func newLessonScoringLoop(db *sql.DB) *lessonScoringLoop {
	return &lessonScoringLoop{
		db:       db,
		interval: time.Hour,
		stopCh:   make(chan struct{}),
	}
}

func (l *lessonScoringLoop) SetLeaderChecker(checker leaderChecker) {
	if l == nil {
		return
	}
	l.leader = checker
}

func (l *lessonScoringLoop) isLeader() bool {
	if l == nil || l.leader == nil {
		return true
	}
	return l.leader.IsLeader(LessonLineageLeaseName)
}

func (l *lessonScoringLoop) Start() {
	if l == nil || l.db == nil {
		return
	}
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	if l.stopCh == nil {
		l.stopCh = make(chan struct{})
	}
	stopCh := l.stopCh
	l.started = true
	l.wg.Add(1)
	l.mu.Unlock()

	go func() {
		defer l.wg.Done()
		// 60 秒 warmup（让 leader-election 先 settle）。
		warmup := time.NewTimer(60 * time.Second)
		select {
		case <-stopCh:
			warmup.Stop()
			return
		case <-warmup.C:
		}
		l.runOnce()

		ticker := time.NewTicker(l.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				l.runOnce()
			}
		}
	}()
}

func (l *lessonScoringLoop) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return
	}
	stopCh := l.stopCh
	l.stopCh = nil
	l.started = false
	l.mu.Unlock()
	close(stopCh)
	l.wg.Wait()
}

// runOnce 是一轮扫表 + 评分。可单独被测试驱动。
func (l *lessonScoringLoop) runOnce() {
	if l == nil || l.db == nil || !l.isLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	rows, err := l.loadDue(ctx, 200)
	if err != nil {
		slog.Warn("lesson scoring: load due failed", "err", err)
		return
	}
	scored := 0
	for _, due := range rows {
		if err := l.scoreOne(ctx, due); err != nil {
			slog.Warn("lesson scoring: score failed",
				"id", due.ID,
				"fund_id", due.FundID,
				"err", err,
			)
			continue
		}
		scored++
	}
	if scored > 0 {
		slog.Info("lesson scoring pass",
			"due", len(rows),
			"scored", scored,
		)
	}
}

// loadDue 拉所有 window_close_at <= now AND observed_at IS NULL，
// 限量返回。WindowOpen 用 created_at 兜底（创建时刻 ≈ 决策时刻）。
func (l *lessonScoringLoop) loadDue(ctx context.Context, limit int) ([]dueLessonLineage, error) {
	const q = `
SELECT id, fund_id, symbol, predicted_direction, hypothesis_window_days,
       created_at, window_close_at
  FROM lesson_pnl_lineage
 WHERE observed_at IS NULL
   AND window_close_at <= NOW()
 ORDER BY window_close_at ASC
 LIMIT $1`
	rows, err := l.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dueLessonLineage
	for rows.Next() {
		var d dueLessonLineage
		if err := rows.Scan(&d.ID, &d.FundID, &d.Symbol, &d.PredictedDirection,
			&d.HypothesisWindowDays, &d.WindowOpen, &d.WindowClose); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// scoreOne 用 fund-level NAV 累计回报作代理算 score。
// score 公式：sign(direction) × clamp(cumulative_return / |1pp|, -3, 3) / 3
// 这样:
//   - 方向预测对、涨跌幅 >= 3pp → +1
//   - 方向预测对、涨跌幅 ≈ 0 → ~0
//   - 方向预测反 → -1
// 没找到 NAV 数据时不评分（保持 observed_at IS NULL，下一次再试）。
func (l *lessonScoringLoop) scoreOne(ctx context.Context, due dueLessonLineage) error {
	if due.PredictedDirection == 0 {
		return l.markObserved(ctx, due.ID, 0, 0, 0, "neutral", nil)
	}

	openNAV, openErr := l.navOnOrBefore(ctx, due.FundID, due.WindowOpen)
	closeNAV, closeErr := l.navOnOrBefore(ctx, due.FundID, due.WindowClose)
	if openErr != nil || closeErr != nil {
		return fmt.Errorf("nav lookup: open=%v close=%v", openErr, closeErr)
	}
	if openNAV <= 0 || closeNAV <= 0 {
		// NAV 还没生成 — 让下一轮再 retry。
		return nil
	}
	retPct := ((closeNAV / openNAV) - 1) * 100
	scaled := math.Max(-3, math.Min(3, retPct))
	signed := scaled
	if due.PredictedDirection < 0 {
		signed = -scaled
	}
	score := signed / 3.0
	verdict := classifyVerdict(score)
	pnl := closeNAV - openNAV

	return l.markObserved(ctx, due.ID, score, pnl, retPct, verdict, nil)
}

func (l *lessonScoringLoop) navOnOrBefore(ctx context.Context, fundID string, when time.Time) (float64, error) {
	const q = `
SELECT total_assets
  FROM nav_snapshots
 WHERE fund_id = $1
   AND trading_date <= $2
 ORDER BY trading_date DESC
 LIMIT 1`
	var nav float64
	err := l.db.QueryRowContext(ctx, q, fundID, when).Scan(&nav)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return nav, err
}

func (l *lessonScoringLoop) markObserved(ctx context.Context, id int64, score, pnl, retPct float64, verdict string, observedAt *time.Time) error {
	now := time.Now().UTC()
	if observedAt != nil {
		now = *observedAt
	}
	_, err := l.db.ExecContext(ctx, `
UPDATE lesson_pnl_lineage
   SET observed_at = $1,
       observed_pnl = $2,
       observed_return_pct = $3,
       score = $4,
       verdict = $5
 WHERE id = $6`,
		now, pnl, retPct, score, verdict, id,
	)
	return err
}

// classifyVerdict 把 score 落到一个人类可读的 bucket。
func classifyVerdict(score float64) string {
	switch {
	case score >= 0.5:
		return "validated"
	case score >= 0.1:
		return "weak_validated"
	case score > -0.1:
		return "neutral"
	case score > -0.5:
		return "weak_refuted"
	default:
		return "refuted"
	}
}

// lessonHitRate 是 PM prompt 引用某 lesson 时附带的 "历史命中率"。
// 用 (lesson_memory_id, fund_id) 算 validated / total。返回 (hit, total)。
// 数据库不可用 / 行数为 0 时返回 (0, 0)，调用方负责省略 surface。
func lessonHitRate(ctx context.Context, db *sql.DB, memoryID string) (float64, int, error) {
	if db == nil || strings.TrimSpace(memoryID) == "" {
		return 0, 0, nil
	}
	const q = `
SELECT COUNT(*) FILTER (WHERE verdict IN ('validated','weak_validated')),
       COUNT(*) FILTER (WHERE observed_at IS NOT NULL)
  FROM lesson_pnl_lineage
 WHERE lesson_memory_id = $1`
	var hits, total int
	if err := db.QueryRowContext(ctx, q, memoryID).Scan(&hits, &total); err != nil {
		return 0, 0, err
	}
	if total == 0 {
		return 0, 0, nil
	}
	return float64(hits) / float64(total), total, nil
}
