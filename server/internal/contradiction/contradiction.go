// Package contradiction 实现 Sprint 3 / L3 的跨 agent 矛盾检测。
//
// 动机：daily workflow 里 bull / bear / quant 三个 researcher 各自
// 输出一段 thesis；PM 在 prompt 里看到三段 consensus，但当三段实
// 际互相打架时（bull 看多 AAPL、bear 同时看空 AAPL、quant 给出
// 中性范围），PM 的回归选项很容易是"选 quant 案"或"取均值"，从而
// 把已经显式表达的分歧吞掉。我们引入一个独立 LLM checker：在 PM
// 决策之前，让它扫所有 researcher 文本 + 最终 plan，把"显式分歧
// 又被 plan 忽略"的情况标出来，写成一行 risk_note 注入到
// DecisionInput。PM prompt 里已经有 risk_notes 的解释，所以
// 接入零成本。
//
// 设计取舍：
//
//   * Checker 是 advisory，不阻塞。它失败 / 超时 → 直接返回 nil
//     notes，不让 PM 的主链路因为它中断。
//   * Checker 用 simple tier model — 它读的是结构化文本+短输出，
//     不需要 critical tier。
//   * 输出 schema 极简：[]Note，Note 只有 severity + summary +
//     evidence。evidence 是文字证据（不强行 symbol/百分比 fact-
//     check —— 那是 lessons 那边的事）。
//   * 当 researcher 数量 < 2 时不调 — 单个 researcher 没法和自
//     己矛盾。
package contradiction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Severity 用 string 是为了在 prompt / log / DB 里都易读。
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityBlock   Severity = "block"
)

// Note 是 checker 输出的一条矛盾。
type Note struct {
	Severity Severity `json:"severity"`
	Summary  string   `json:"summary"`
	Evidence string   `json:"evidence,omitempty"`
	// Symbol 可选；当矛盾仅围绕单个标的时填上，让下游 prompt
	// 可以做 per-symbol 关联。
	Symbol string `json:"symbol,omitempty"`
}

// String 便于把 Note 拼成一行 risk_note 文本。
func (n Note) String() string {
	parts := []string{}
	if n.Severity != "" {
		parts = append(parts, "["+strings.ToUpper(string(n.Severity))+"]")
	}
	if n.Symbol != "" {
		parts = append(parts, n.Symbol+":")
	}
	parts = append(parts, n.Summary)
	if n.Evidence != "" {
		parts = append(parts, "—", n.Evidence)
	}
	return strings.Join(parts, " ")
}

// Input 是检查器输入。每个 ResearcherView 就是一段 thesis + role 标签；
// 我们把它们一起喂给 LLM，让模型自己找矛盾点。Plan 是可选的 — 当
// 提供时，模型可以指出"已表达的矛盾被 plan 选择性忽略"。
type Input struct {
	FundID       string
	TradingDate  time.Time
	Universe     []string
	Researchers  []ResearcherView
	MacroSummary string
	PlanSummary  string // optional, "" → checker 只看 researcher 间矛盾
}

type ResearcherView struct {
	Role     string // bull / bear / quant / pm / risk
	Stance   string // optional one-word label (long/short/neutral)
	Body     string // free text thesis
}

// LLMClient 是最小依赖，只要能 ChatJSON 即可；project 里已有相同
// shape 的实现（llm.Client）— 我们走 interface 留接入弹性。
type LLMClient interface {
	ChatJSON(ctx context.Context, req ChatRequest) (string, error)
}

// ChatRequest 与 project 里 llm.ChatRequest 兼容形状（不直接引入
// 是为 keep contradiction pkg 不拉整个 llm 依赖图）。FundID /
// AgentID / UserID / StepName 用作路由与计费 tag，由 caller 填好；
// 本 pkg 不构造它们。
type ChatRequest struct {
	FundID     string
	AgentID    string
	UserID     string
	StepName   string
	System     string
	User       string
	ModelTier  string
	MaxTokens  int
}

// Checker 是 stateful 入口。Disabled=true 时直接返回 nil，方便环境
// 变量 / fund profile 控制 rollout。
type Checker struct {
	Client    LLMClient
	Disabled  bool
	MaxNotes  int    // 0 → 3
	ModelTier string // "" → "simple"
}

// New 建一个默认配置的 checker。
func New(client LLMClient) *Checker {
	return &Checker{
		Client:    client,
		MaxNotes:  3,
		ModelTier: "simple",
	}
}

// Check 跑一次 LLM check。返回 (notes, error)：error 仅在调用
// 本身完全坏掉时回；模型解析失败 / 超时一律静默 → nil。
func (c *Checker) Check(ctx context.Context, in Input) ([]Note, error) {
	if c == nil || c.Disabled || c.Client == nil {
		return nil, nil
	}
	if len(in.Researchers) < 2 {
		return nil, nil
	}
	tier := c.ModelTier
	if tier == "" {
		tier = "simple"
	}
	maxNotes := c.MaxNotes
	if maxNotes <= 0 {
		maxNotes = 3
	}
	req := ChatRequest{
		FundID:    in.FundID,
		StepName:  "contradiction-check",
		System:    systemPrompt(maxNotes),
		User:      userPrompt(in),
		ModelTier: tier,
		MaxTokens: 512,
	}
	body, err := c.Client.ChatJSON(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("contradiction: chat: %w", err)
	}
	notes, err := parseNotes(body)
	if err != nil {
		// 解析失败不报错给 caller — checker 是 advisory，PM 不
		// 应因为这个被阻塞。Caller 通过 metric/log 观察。
		return nil, nil
	}
	if len(notes) > maxNotes {
		notes = notes[:maxNotes]
	}
	return notes, nil
}

// FormatRiskNotes 把 []Note 转成可直接塞入 DecisionInput.RiskNotes
// 的字符串列表。我们只保留 warning + block — info 不进 prompt，
// 防止 noisy。
func FormatRiskNotes(notes []Note) []string {
	if len(notes) == 0 {
		return nil
	}
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		if n.Severity == SeverityInfo {
			continue
		}
		s := strings.TrimSpace(n.String())
		if s == "" {
			continue
		}
		out = append(out, "[contradiction] "+s)
	}
	return out
}

// systemPrompt — 短而硬。LLM 容易把"差异"误判为"矛盾"，要拉一条
// 警戒线：必须是实质性、可操作的分歧才算。
func systemPrompt(maxNotes int) string {
	return fmt.Sprintf(`You are a contradiction auditor for a multi-agent investment workflow. Inputs are short researcher theses (bull/bear/quant/etc.) plus optionally a plan summary. Identify pairs of theses or plan vs thesis that express an OPERATIONAL contradiction — same instrument, opposing direction or incompatible sizing, where executing the plan as written would visibly ignore one of the views.

Return STRICT JSON with the schema:
{
  "notes": [
    {"severity": "info|warning|block", "summary": "...", "evidence": "...", "symbol": "OPTIONAL"}
  ]
}

Rules:
- At most %d notes total.
- "info"   = different framing, same direction → drop silently (do not return).
- "warning"= researchers disagree on direction or sizing AND plan picks one side without rationale.
- "block"  = plan acts on a thesis that another researcher explicitly tagged as high-risk avoid, in conflicting direction.
- Do NOT invent symbols or numbers. Quote evidence text fragments verbatim.
- If no contradiction qualifies, return {"notes": []}.
- Output JSON only. No prose.`, maxNotes)
}

// userPrompt — encode 输入。we keep it dumb JSON for the model.
func userPrompt(in Input) string {
	type rv struct {
		Role   string `json:"role"`
		Stance string `json:"stance,omitempty"`
		Body   string `json:"body"`
	}
	payload := struct {
		FundID       string   `json:"fundId"`
		TradingDate  string   `json:"tradingDate"`
		Universe     []string `json:"universe,omitempty"`
		MacroSummary string   `json:"macroSummary,omitempty"`
		Researchers  []rv     `json:"researchers"`
		PlanSummary  string   `json:"planSummary,omitempty"`
	}{
		FundID:       in.FundID,
		TradingDate:  in.TradingDate.Format("2006-01-02"),
		Universe:     trimUniverse(in.Universe, 25),
		MacroSummary: truncate(in.MacroSummary, 600),
		PlanSummary:  truncate(in.PlanSummary, 800),
	}
	for _, r := range in.Researchers {
		body := truncate(r.Body, 600)
		if body == "" {
			continue
		}
		payload.Researchers = append(payload.Researchers, rv{
			Role:   r.Role,
			Stance: r.Stance,
			Body:   body,
		})
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	return string(b)
}

// parseNotes — LLM JSON 解析；接受裸 array 或 {notes: [...]} 两种。
func parseNotes(body string) ([]Note, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("empty body")
	}
	// 去掉 LLM 偶尔会包的 markdown code fence。
	if strings.HasPrefix(body, "```") {
		body = strings.TrimPrefix(body, "```json")
		body = strings.TrimPrefix(body, "```")
		body = strings.TrimSuffix(body, "```")
		body = strings.TrimSpace(body)
	}
	type envelope struct {
		Notes []Note `json:"notes"`
	}
	var env envelope
	if err := json.Unmarshal([]byte(body), &env); err == nil && len(env.Notes) > 0 {
		return sanitizeNotes(env.Notes), nil
	}
	var arr []Note
	if err := json.Unmarshal([]byte(body), &arr); err == nil {
		return sanitizeNotes(arr), nil
	}
	return nil, errors.New("could not parse notes")
}

func sanitizeNotes(in []Note) []Note {
	out := make([]Note, 0, len(in))
	for _, n := range in {
		n.Summary = strings.TrimSpace(n.Summary)
		n.Evidence = strings.TrimSpace(n.Evidence)
		n.Symbol = strings.TrimSpace(n.Symbol)
		if n.Summary == "" {
			continue
		}
		switch n.Severity {
		case SeverityInfo, SeverityWarning, SeverityBlock:
		default:
			n.Severity = SeverityWarning
		}
		out = append(out, n)
	}
	return out
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}

func trimUniverse(in []string, max int) []string {
	if len(in) <= max {
		return in
	}
	out := make([]string, max)
	copy(out, in[:max])
	return out
}
