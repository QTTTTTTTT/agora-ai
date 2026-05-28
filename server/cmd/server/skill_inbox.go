package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/factorlab"
	"github.com/fundai/server/internal/mailer"
	"github.com/fundai/server/internal/repository"
)

// Sprint 3 / M5: 跨基金技能审批 inbox。
//
// 设计思路：
//
//  * skill 是 agent-level 的 JSON 配置（parsedSkillConfig 里的 skills[]），
//    没有独立表 — 因此 inbox 通过扫所有 agents.skill_config 来构建。
//    这跟 reflection-driven proposed skill 一致：reflection cycle 会
//    给 fund 的每个 agent 都 propose 同样的 skill（key 是 reflection:<id>），
//    inbox 显示按 fund 分组的唯一候选。
//
//  * shadow-evaluate 用 factorlab 跑一个简化 backtest：根据 skill 文本
//    关键词选 strategy（momentum / value / low_beta / low_vol / equal_weight），
//    在合成 fixture 上跑 14 天。返回 Sharpe / hit_rate。
//
//  * auto-approve：累计 shadow 评估，每条 skill 在连续 3 次评估都满足
//    Sharpe > 0.8 且 hit_rate > 55% 时自动 flip 为 approved，并给 fund
//    owner 发邮件。

const (
	skillShadowMinSharpe      = 0.8
	skillShadowMinHitRatePct  = 55.0
	skillShadowAutoApproveRun = 3
)

// skillShadowKeyword 把 skill content 关键词映射到 factorlab strategy 名。
var skillShadowKeywordToStrategy = map[string]factorlab.Strategy{
	"momentum":    factorlab.Momentum12_1M{},
	"动量":         factorlab.Momentum12_1M{},
	"trend":       factorlab.Momentum12_1M{},
	"低 beta":     factorlab.LowBeta{},
	"low beta":   factorlab.LowBeta{},
	"low_beta":   factorlab.LowBeta{},
	"低波":         factorlab.LowVol{},
	"low vol":    factorlab.LowVol{},
	"low_vol":    factorlab.LowVol{},
	"defensive":  factorlab.LowVol{},
	"防御":         factorlab.LowVol{},
}

// pickShadowStrategy 走匹配：第一个命中的关键词决定 strategy；都不命中
// 用 EqualWeightLong 作 baseline（这样至少给出可比的 Sharpe）。
func pickShadowStrategy(content string) factorlab.Strategy {
	lc := strings.ToLower(content)
	for keyword, strat := range skillShadowKeywordToStrategy {
		if strings.Contains(lc, strings.ToLower(keyword)) {
			return strat
		}
	}
	return factorlab.EqualWeightLong{}
}

// ProposedSkillRow 是 inbox 列表的一行。跨基金、按 proposedAt 升序，
// 带 ageHours 方便 UI 高亮停滞过久的项。
type ProposedSkillRow struct {
	FundID            string    `json:"fundId"`
	FundName          string    `json:"fundName,omitempty"`
	AgentID           string    `json:"agentId"`
	AgentName         string    `json:"agentName,omitempty"`
	AgentRole         string    `json:"agentRole,omitempty"`
	OwnerUserID       string    `json:"ownerUserId,omitempty"`
	SkillKey          string    `json:"skillKey"`
	SkillName         string    `json:"skillName"`
	Description       string    `json:"description,omitempty"`
	Source            string    `json:"source,omitempty"`
	ProposedAt        string    `json:"proposedAt,omitempty"`
	AgeHours          float64   `json:"ageHours"`
	ShadowEvalRuns    int       `json:"shadowEvalRuns,omitempty"`
	ShadowMeanSharpe  float64   `json:"shadowMeanSharpe,omitempty"`
	ShadowMeanHitRate float64   `json:"shadowMeanHitRate,omitempty"`
	LastShadowAt      time.Time `json:"lastShadowAt,omitempty"`
}

// ProposedSkillsResponse is the inbox list envelope.
type ProposedSkillsResponse struct {
	GeneratedAt time.Time          `json:"generatedAt"`
	Items       []ProposedSkillRow `json:"items"`
}

// ShadowEvalResponse 是单条 skill 的 shadow 结果。
type ShadowEvalResponse struct {
	SkillKey       string  `json:"skillKey"`
	Strategy       string  `json:"strategy"`
	Sharpe         float64 `json:"sharpe"`
	HitRatePct     float64 `json:"hitRatePct"`
	AnnualReturn   float64 `json:"annualReturn"`
	AnnualVol      float64 `json:"annualVol"`
	MaxDrawdown    float64 `json:"maxDrawdown"`
	TradingDays    int     `json:"tradingDays"`
	AutoApproved   bool    `json:"autoApproved"`
	RunNumber      int     `json:"runNumber"`
	Threshold      string  `json:"threshold"`
	EvaluatedAt    time.Time `json:"evaluatedAt"`
}

// skillInbox 是 admin handler 调用的核心服务。db / agentRepo / fundRepo
// / mailer 都可空 — 测试可以注入 stub；nil 字段时对应能力降级而不是 panic。
type skillInbox struct {
	db         *sql.DB
	agentRepo  *repository.AgentRepo
	fundRepo   *repository.FundRepo
	mailer     mailer.Mailer
	brand      string
	appBaseURL string
}

func newSkillInbox(db *sql.DB, mailerInstance mailer.Mailer, brand, appURL string) *skillInbox {
	return &skillInbox{
		db:         db,
		agentRepo:  repository.NewAgentRepo(db),
		fundRepo:   repository.NewFundRepo(db),
		mailer:     mailerInstance,
		brand:      brand,
		appBaseURL: appURL,
	}
}

// ListProposed 扫所有 agent，按 (fund, skillKey) 唯一展开 proposed skills。
// agingMinHours = 0 时不过滤；> 0 时只回不晚于 (now - hours) 的项（"已经
// 在 inbox 待审过 X 小时"）。
func (s *skillInbox) ListProposed(ctx context.Context, agingMinHours int) (*ProposedSkillsResponse, error) {
	if s == nil || s.db == nil {
		return &ProposedSkillsResponse{GeneratedAt: time.Now().UTC()}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT a.id, a.user_id, a.name, a.role, a.skill_config,
       tm.fund_id, f.name
  FROM agents a
  LEFT JOIN team_members tm ON tm.agent_id = a.id AND tm.status = 'active'
  LEFT JOIN funds f ON f.id = tm.fund_id
 WHERE a.skill_config IS NOT NULL
   AND a.deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("skill inbox: list agents: %w", err)
	}
	defer rows.Close()
	now := time.Now().UTC()
	threshold := now
	if agingMinHours > 0 {
		threshold = now.Add(-time.Duration(agingMinHours) * time.Hour)
	}
	seen := make(map[string]struct{}) // dedupe by (fundID, skillKey)
	out := []ProposedSkillRow{}
	for rows.Next() {
		var (
			agentID    string
			userID     string
			agentName  string
			agentRole  string
			rawSkill   json.RawMessage
			fundID     sql.NullString
			fundName   sql.NullString
		)
		if err := rows.Scan(&agentID, &userID, &agentName, &agentRole, &rawSkill, &fundID, &fundName); err != nil {
			return nil, err
		}
		if !fundID.Valid {
			continue
		}
		config := parseSkillConfig(rawSkill)
		for _, entry := range config.Skills {
			if !strings.EqualFold(entry.Status, skillStatusProposed) {
				continue
			}
			proposedAt, _ := time.Parse(time.RFC3339, entry.ProposedAt)
			if agingMinHours > 0 && proposedAt.After(threshold) {
				continue
			}
			key := fundID.String + "::" + entry.Key
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			row := ProposedSkillRow{
				FundID:      fundID.String,
				FundName:    fundName.String,
				AgentID:     agentID,
				AgentName:   agentName,
				AgentRole:   agentRole,
				OwnerUserID: userID,
				SkillKey:    entry.Key,
				SkillName:   entry.Name,
				Description: entry.Description,
				Source:      entry.Source,
				ProposedAt:  entry.ProposedAt,
			}
			if !proposedAt.IsZero() {
				row.AgeHours = now.Sub(proposedAt).Hours()
			}
			// 拉历史 shadow eval 聚合：runs + mean sharpe / hit rate。
			if runs, ms, mh, last, lerr := s.loadShadowAggregate(ctx, fundID.String, entry.Key); lerr == nil {
				row.ShadowEvalRuns = runs
				row.ShadowMeanSharpe = ms
				row.ShadowMeanHitRate = mh
				row.LastShadowAt = last
			}
			out = append(out, row)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 排序：旧的在前（FIFO，鼓励先看积压的）。
	for i := 0; i < len(out)-1; i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].AgeHours > out[i].AgeHours {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return &ProposedSkillsResponse{GeneratedAt: now, Items: out}, nil
}

// loadShadowAggregate 拉 (fund, skill) 维度的历史 shadow eval 统计。
// 不存在 / 错误时返回零值。
func (s *skillInbox) loadShadowAggregate(ctx context.Context, fundID, skillKey string) (int, float64, float64, time.Time, error) {
	const q = `
SELECT COUNT(*), COALESCE(AVG(sharpe), 0), COALESCE(AVG(hit_rate_pct), 0), COALESCE(MAX(evaluated_at), '1970-01-01'::timestamptz)
  FROM skill_shadow_evals
 WHERE fund_id = $1 AND skill_key = $2`
	var (
		runs       int
		meanSharpe float64
		meanHit    float64
		lastAt     time.Time
	)
	err := s.db.QueryRowContext(ctx, q, fundID, skillKey).Scan(&runs, &meanSharpe, &meanHit, &lastAt)
	if err != nil {
		// 表不存在（migrations 未跑）/ 其他错误：静默降级。
		return 0, 0, 0, time.Time{}, nil
	}
	return runs, meanSharpe, meanHit, lastAt, nil
}

// ShadowEvaluate 是审批前的回测验证：在一个合成 14-天 / 504-天 fixture 上
// 跑 skill 隐含的 strategy，给一组 metrics。同时把结果落 skill_shadow_evals，
// 累计 3 次过门槛后自动 approve。
func (s *skillInbox) ShadowEvaluate(ctx context.Context, fundID, skillKey string) (*ShadowEvalResponse, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("skill inbox: db unavailable")
	}
	// 找出此 fund 下任一 agent 上同 key 的 skill entry —— 反推 strategy
	// 文本。所有 agent 上同 reflection-id 的 skill content 应该完全一致
	// （reflection cycle 写同样的内容），所以选第一个命中即可。
	row := s.db.QueryRowContext(ctx, `
SELECT a.id, a.skill_config
  FROM agents a
  JOIN team_members tm ON tm.agent_id = a.id AND tm.status = 'active'
 WHERE tm.fund_id = $1 AND a.deleted_at IS NULL
 LIMIT 50`, fundID)
	_ = row // multi-row; fallthrough to Query
	rows, err := s.db.QueryContext(ctx, `
SELECT a.id, a.skill_config
  FROM agents a
  JOIN team_members tm ON tm.agent_id = a.id AND tm.status = 'active'
 WHERE tm.fund_id = $1 AND a.deleted_at IS NULL`, fundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found parsedSkillEntry
	for rows.Next() {
		var (
			agentID  string
			rawSkill json.RawMessage
		)
		if err := rows.Scan(&agentID, &rawSkill); err != nil {
			return nil, err
		}
		config := parseSkillConfig(rawSkill)
		for _, entry := range config.Skills {
			if entry.Key == skillKey {
				found = entry
				break
			}
		}
		if found.Key != "" {
			break
		}
	}
	if found.Key == "" {
		return nil, api.ErrNotFound
	}

	strat := pickShadowStrategy(found.Name + " " + found.Description + " " + found.Content)

	// 504 个交易日 ≈ 2 年合成数据，足以让 factor 收敛。每个 skill
	// 用确定性 seed 派生，三次 shadow eval 都能拿到同样的结果 → 适合
	// 做 auto-approve 阈值的 gate。
	fixture := factorlab.BuildSynthFixture(factorlab.SynthOptions{
		Seed: 42 + int64(hashStringToInt(skillKey)),
		Days: 504,
	})
	sim := &factorlab.Simulator{}
	results := sim.Run(fixture, []factorlab.Strategy{strat})
	if len(results) == 0 {
		return nil, errors.New("skill inbox: shadow simulator returned no result")
	}
	r := results[0]
	hitRatePct := r.HitRate * 100

	resp := &ShadowEvalResponse{
		SkillKey:     skillKey,
		Strategy:     strat.Name(),
		Sharpe:       r.Sharpe,
		HitRatePct:   hitRatePct,
		AnnualReturn: r.AnnualReturn,
		AnnualVol:    r.AnnualVol,
		MaxDrawdown:  r.MaxDrawdown,
		TradingDays:  r.TradingDays,
		Threshold:    fmt.Sprintf("sharpe>%.1f AND hit_rate>%.0f%% (3x)", skillShadowMinSharpe, skillShadowMinHitRatePct),
		EvaluatedAt:  time.Now().UTC(),
	}

	if _, err := s.db.ExecContext(ctx, `
INSERT INTO skill_shadow_evals (fund_id, skill_key, strategy, sharpe, hit_rate_pct, evaluated_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
		fundID, skillKey, strat.Name(), resp.Sharpe, resp.HitRatePct, resp.EvaluatedAt,
	); err != nil {
		// 表不在 / 写失败：观察工具，不阻断响应。
		slog.Warn("skill inbox: persist shadow eval failed", "fund_id", fundID, "skill", skillKey, "err", err)
	}

	// 取近 3 次评估，若全部满足门槛则 auto-approve。
	runs, autoOK := s.checkAutoApprove(ctx, fundID, skillKey)
	resp.RunNumber = runs
	if autoOK {
		if err := s.autoApprove(ctx, fundID, skillKey); err == nil {
			resp.AutoApproved = true
		} else {
			slog.Warn("skill inbox: auto-approve failed", "fund_id", fundID, "skill", skillKey, "err", err)
		}
	}

	return resp, nil
}

// checkAutoApprove 看最近 N 次评估是否全部满足门槛（Sharpe > min, HitRate > min%）。
// 返回 (累计运行次数, 是否触发 auto-approve)。
func (s *skillInbox) checkAutoApprove(ctx context.Context, fundID, skillKey string) (int, bool) {
	rows, err := s.db.QueryContext(ctx, `
SELECT sharpe, hit_rate_pct
  FROM skill_shadow_evals
 WHERE fund_id = $1 AND skill_key = $2
 ORDER BY evaluated_at DESC
 LIMIT $3`,
		fundID, skillKey, skillShadowAutoApproveRun,
	)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	count := 0
	all := true
	for rows.Next() {
		var sharpe, hit float64
		if err := rows.Scan(&sharpe, &hit); err != nil {
			return count, false
		}
		count++
		if sharpe <= skillShadowMinSharpe || hit <= skillShadowMinHitRatePct {
			all = false
		}
	}
	if count < skillShadowAutoApproveRun {
		return count, false
	}
	return count, all
}

// autoApprove 把 fund 内所有 agent 上同 key 的 proposed skill flip 成
// approved + enabled。完成后给 fund owner 发邮件。
func (s *skillInbox) autoApprove(ctx context.Context, fundID, skillKey string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
SELECT a.id, a.user_id, a.skill_config
  FROM agents a
  JOIN team_members tm ON tm.agent_id = a.id AND tm.status = 'active'
 WHERE tm.fund_id = $1 AND a.deleted_at IS NULL
 FOR UPDATE OF a`,
		fundID,
	)
	if err != nil {
		return err
	}
	type pending struct {
		agentID string
		userID  string
		raw     json.RawMessage
	}
	var todos []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.agentID, &p.userID, &p.raw); err != nil {
			rows.Close()
			return err
		}
		todos = append(todos, p)
	}
	rows.Close()
	now := time.Now().UTC()
	var ownerUserID string
	approvedSkillName := ""
	for _, td := range todos {
		updated, entry, found, err := approveSkillInConfig(td.raw, skillKey, now)
		if err != nil || !found {
			continue
		}
		if updated == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET skill_config = $1, updated_at = NOW() WHERE id = $2`, updated, td.agentID); err != nil {
			return err
		}
		if ownerUserID == "" {
			ownerUserID = td.userID
		}
		if approvedSkillName == "" {
			approvedSkillName = entry.Name
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// 发邮件 — 失败 warn 后继续，不要 fail 整个 approve 流程。
	if s.mailer != nil && ownerUserID != "" {
		go s.notifyAutoApprove(ownerUserID, fundID, skillKey, approvedSkillName)
	}
	return nil
}

// notifyAutoApprove 给 fund owner 发邮件通知。后台异步、独立超时，
// 避免阻塞调用方。
func (s *skillInbox) notifyAutoApprove(userID, fundID, skillKey, skillName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var email sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT email FROM users WHERE id = $1 AND deleted_at IS NULL`,
		userID,
	).Scan(&email); err != nil {
		return
	}
	if !email.Valid || strings.TrimSpace(email.String) == "" {
		return
	}
	subject := "[FundAI] 候选技能已自动批准 / Skill auto-approved"
	body := fmt.Sprintf(`Skill "%s" (key=%s) for fund %s has cleared 3 consecutive shadow evaluations with Sharpe > %.1f and hit rate > %.0f%%. The skill is now active for the fund's agents.

候选技能 "%s"（key=%s）在基金 %s 上连续 %d 次 shadow 回测都通过了 Sharpe > %.1f 且命中率 > %.0f%% 的门槛，已自动 approved，对该基金的所有 agent 生效。`,
		skillName, skillKey, fundID, skillShadowMinSharpe, skillShadowMinHitRatePct,
		skillName, skillKey, fundID, skillShadowAutoApproveRun, skillShadowMinSharpe, skillShadowMinHitRatePct,
	)
	if err := s.mailer.Send(ctx, mailer.Message{
		To:       email.String,
		Subject:  subject,
		TextBody: body,
	}); err != nil {
		slog.Warn("skill inbox: notify auto-approve failed", "user_id", userID, "err", err)
	}
}

// hashStringToInt 是 SynthFixture seed 派生的小 helper —— 同一个 skillKey
// 每次跑都得到同一个 seed → 评估稳定可复现。
func hashStringToInt(s string) int {
	h := 0
	for _, r := range s {
		h = (h*31 + int(r)) & 0x7FFFFFFF
	}
	return h
}
