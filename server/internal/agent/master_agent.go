// master_agent.go — international investor "master" agents for the
// /advisor consultation surface.
//
// A MasterAgent is a thin wrapper around an LLM call that uses one
// of the 10 persona JSON templates in internal/agent/masters/ as
// the system prompt scaffold. The persona file defines:
//
//   * philosophy / holding period (Buffett → "10年+", Druckenmiller
//     → "数月到数年（趋势期限）")
//   * must-have quantitative criteria (Buffett → "ROE 10yr avg ≥ 15%")
//   * qualitative filters (Munger → "看不懂直接 pass")
//   * red lines (Graham → "市值 < $5亿 否决")
//   * verdict enum + output JSON schema
//
// The Go agent's job is to:
//   1. take a single symbol + the fundamental snapshot we have
//      available;
//   2. render the persona JSON + symbol context into a prompt;
//   3. ask the LLM to fill the persona's output_format JSON;
//   4. parse + validate + return a MasterReport.
//
// Phase 1 design notes:
//
//   * We do NOT yet have 10-year historical financials (that's
//     Phase 2). The prompt explicitly tells the LLM to mark fields
//     as "data_unavailable" in key_reasons rather than fabricate
//     numbers. Phase 2's HistoricalProvider will fill the gap and
//     the prompt will gain a "rule_based_prior" block computed in
//     Go, mirroring the analyst_*.go pattern.
//
//   * MasterReport intentionally differs from AnalystReport — masters
//     output a verdict (STRONG_BUY..AVOID) + master-specific extras
//     (Buffett intrinsic value, Lynch PEG, Graham number, …), not a
//     bullish/bearish/neutral direction. Forcing them into
//     AnalystReport would lose the per-master rich payload.
//
//   * The persona JSON files are loaded once at boot via
//     internal/agent/masters.FS; we cache parsed personas in this
//     file so a hot-reload story is one Reset() call away.

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/agent/masters"
)

// ---------------------------------------------------------------------------
// MasterPersona — parsed shape of a masters/*.json file
// ---------------------------------------------------------------------------

// MasterPersona is the persona description loaded from one JSON
// file in internal/agent/masters/. The struct intentionally
// captures only the fields the agent layer needs to reason about;
// any extra fields the JSON template carries (master-specific
// frameworks like Munger's 25 biases, CANSLIM, etc.) flow through
// the Raw map and get rendered into the LLM system prompt verbatim.
type MasterPersona struct {
	Key             string   `json:"agent_id"`
	NameZh          string   `json:"name_zh"`
	NameEn          string   `json:"name_en"`
	Style           string   `json:"style"`
	HoldingPeriod   string   `json:"holding_period"`
	Philosophy      string   `json:"philosophy"`
	VerdictEnum     []string `json:"verdict_enum,omitempty"`

	// Raw is the full parsed JSON. The renderer in
	// buildSystemPrompt walks Raw to embed every key — that
	// way a new persona JSON file gets its bespoke sections
	// (Buffett's moat scoring, Lynch's six categories, Marks'
	// 7 cycle questions) in the prompt without code changes.
	Raw map[string]any `json:"-"`
}

// Validate enforces the must-have fields a persona needs to be
// safely materialised into a MasterAgent.
func (p MasterPersona) Validate() error {
	if strings.TrimSpace(p.Key) == "" {
		return errors.New("master: persona.agent_id required")
	}
	if strings.TrimSpace(p.NameEn) == "" {
		return fmt.Errorf("master: persona %q missing name_en", p.Key)
	}
	if strings.TrimSpace(p.Philosophy) == "" {
		return fmt.Errorf("master: persona %q missing philosophy", p.Key)
	}
	return nil
}

// ---------------------------------------------------------------------------
// MasterReport — what one master returns for one symbol
// ---------------------------------------------------------------------------

// MasterReport is the structured output every MasterAgent produces.
// Distinct from AnalystReport because masters speak in verdicts
// (BUY/HOLD/AVOID) and carry per-master extras (intrinsic value,
// PEG ratio, moat score) the analyst quartet doesn't have.
type MasterReport struct {
	MasterKey       string         `json:"master_key"`
	MasterNameZh    string         `json:"master_name_zh"`
	MasterNameEn    string         `json:"master_name_en"`
	Symbol          string         `json:"symbol"`
	// SymbolName is the issuer's short Chinese / English name (e.g.
	// "德科立"). Optional — empty when the upstream provider doesn't
	// resolve it. Plumbed through so the UI can show
	// "德科立 (688205)" without round-tripping a separate lookup.
	SymbolName      string         `json:"symbol_name,omitempty"`
	AsOf            time.Time      `json:"asof"`
	GeneratedAt     time.Time      `json:"generated_at"`
	Verdict         string         `json:"verdict"`
	Confidence      int            `json:"confidence"`
	Thesis          string         `json:"thesis"`
	KeyReasons      []string       `json:"key_reasons"`
	KeyRisks        []string       `json:"key_risks"`
	MasterSpecific  map[string]any `json:"master_specific,omitempty"`
	RedLinesHit     []string       `json:"red_lines_hit,omitempty"`
	LLMModel        string         `json:"llm_model,omitempty"`
	PromptTokens    int            `json:"prompt_tokens,omitempty"`
	CompletionTokens int           `json:"completion_tokens,omitempty"`
}

// Validate enforces what must be set before persistence.
func (r MasterReport) Validate() error {
	if strings.TrimSpace(r.MasterKey) == "" {
		return errors.New("master: report.MasterKey required")
	}
	if strings.TrimSpace(r.Symbol) == "" {
		return errors.New("master: report.Symbol required")
	}
	v := strings.ToUpper(strings.TrimSpace(r.Verdict))
	switch v {
	case "STRONG_BUY", "BUY", "HOLD", "AVOID", "SHORT", "PASS", "SKIP":
	default:
		return fmt.Errorf("master: verdict %q invalid", r.Verdict)
	}
	if r.Confidence < 0 || r.Confidence > 100 {
		return fmt.Errorf("master: confidence %d out of [0,100]", r.Confidence)
	}
	if strings.TrimSpace(r.Thesis) == "" {
		return errors.New("master: report.Thesis required")
	}
	return nil
}

// ---------------------------------------------------------------------------
// MasterInput — per-consultation input
// ---------------------------------------------------------------------------

// MasterInput is what the master receives per symbol. We
// deliberately keep it leaner than AnalystInput: the master agents
// reason on a single business case and don't need the four
// per-category blocks. The Notes field is a freeform escape hatch
// for caller-supplied context (e.g. "EPS preview tomorrow").
type MasterInput struct {
	Symbol          string
	// Name is the issuer's short Chinese / English name (e.g.
	// "德科立", "Apple Inc."). Optional — when present the
	// master prompt prefixes it so the LLM reasons about the
	// company by name rather than only by ticker, and the UI
	// shows e.g. "德科立 (688205)" in the verdict header.
	Name            string
	AssetClass      string
	Market          string
	AsOf            time.Time
	PriceLast       float64
	PriceChange     float64
	Currency        string

	// Fundamentals is the single-period snapshot from
	// internal/fundamental (PE, PB, ROE, dividend yield, …).
	// Phase 2 extends this with History so masters can
	// validate "10年ROE平均≥15%" type criteria. nil-safe:
	// the agent prompts the LLM to honestly say "数据缺失"
	// rather than fabricate values.
	Fundamentals    *FundamentalsBlock

	// Technical is the price-action / momentum / volatility
	// snapshot. Optional — when the wiring layer's OHLC fetcher
	// can't reach the symbol (no bars, upstream throttled, etc.)
	// this stays nil and the prompt just omits the section. The
	// LLM is instructed (rule 9) to never invent technical values.
	Technical *MasterTechnicalBlock

	// Notes is freeform context. Operators may prepend
	// "earnings in 3 days" or "$$$ insider buying detected"
	// to bias the prompt.
	Notes           string
}

// ---------------------------------------------------------------------------
// MasterAgent — runs one master persona end-to-end
// ---------------------------------------------------------------------------

// MasterAgent is a single international-investor agent. Construct
// via NewMasterAgent; share across goroutines is safe.
type MasterAgent struct {
	persona MasterPersona
	llm     LLMClient
	logger  *slog.Logger
	now     func() time.Time
}

// MasterAgentOption configures construction.
type MasterAgentOption func(*MasterAgent)

// WithMasterLogger swaps the default slog.Default() logger.
func WithMasterLogger(l *slog.Logger) MasterAgentOption {
	return func(a *MasterAgent) {
		if l != nil {
			a.logger = l
		}
	}
}

// WithMasterClock injects a deterministic clock for tests.
func WithMasterClock(now func() time.Time) MasterAgentOption {
	return func(a *MasterAgent) {
		if now != nil {
			a.now = now
		}
	}
}

// NewMasterAgent constructs a MasterAgent from a parsed persona +
// an LLM client. Returns an error if the persona is malformed.
func NewMasterAgent(persona MasterPersona, llm LLMClient, opts ...MasterAgentOption) (*MasterAgent, error) {
	if err := persona.Validate(); err != nil {
		return nil, err
	}
	a := &MasterAgent{
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

// Key returns the persona's agent_id (e.g. "buffett", "lynch").
func (a *MasterAgent) Key() string { return a.persona.Key }

// NameZh returns the persona's Chinese display name.
func (a *MasterAgent) NameZh() string { return a.persona.NameZh }

// NameEn returns the persona's English display name.
func (a *MasterAgent) NameEn() string { return a.persona.NameEn }

// Persona returns a copy of the persona this agent was built with.
func (a *MasterAgent) Persona() MasterPersona { return a.persona }

// Analyze runs one master on one symbol. Errors that don't kill
// the call (LLM glitch + parse failure) degrade to a HOLD with a
// "data_unavailable / model_failed" thesis so the panel always
// returns N reports, never N-1.
func (a *MasterAgent) Analyze(ctx context.Context, in MasterInput) (MasterReport, error) {
	if strings.TrimSpace(in.Symbol) == "" {
		return MasterReport{}, errors.New("master: input.Symbol required")
	}
	rep := MasterReport{
		MasterKey:    a.persona.Key,
		MasterNameZh: a.persona.NameZh,
		MasterNameEn: a.persona.NameEn,
		Symbol:       strings.ToUpper(strings.TrimSpace(in.Symbol)),
		SymbolName:   strings.TrimSpace(in.Name),
		AsOf:         in.AsOf,
		GeneratedAt:  a.now(),
		Verdict:      "HOLD",
		Confidence:   30,
		Thesis:       fmt.Sprintf("%s (%s) 暂无足够数据形成强观点。", a.persona.NameZh, a.persona.NameEn),
		KeyReasons:   []string{"insufficient_data"},
		KeyRisks:     []string{"data_unavailable"},
		LLMModel:     "fallback",
	}
	if a.llm == nil {
		// Without an LLM we still return the fallback shell so
		// the panel can run a UI-only smoke test.
		if err := rep.Validate(); err != nil {
			return MasterReport{}, err
		}
		return rep, nil
	}

	sys := a.buildSystemPrompt()
	user := a.buildUserPrompt(in)

	raw, err := a.complete(ctx, sys, user)
	if err != nil {
		a.logger.Warn("master agent: LLM failed, returning fallback",
			"master", a.persona.Key, "symbol", in.Symbol, "err", err)
		rep.KeyRisks = append(rep.KeyRisks, fmt.Sprintf("llm_error:%v", err))
		return rep, nil
	}
	parsed, perr := parseMasterLLM(raw)
	if perr != nil {
		// Include a sample of the raw reply (capped to 400 chars to
		// avoid log explosion on a chatty model). The most common cause
		// of "no json object found" on a thinking-capable model is the
		// response being truncated to empty because the thinking pass
		// consumed the entire maxOutputTokens envelope. Surfacing the
		// raw length + a head/tail lets us tell that apart from a
		// model that returned malformed JSON.
		sample := strings.TrimSpace(raw)
		rawLen := len(sample)
		if rawLen > 400 {
			sample = sample[:200] + "...[truncated]..." + sample[rawLen-200:]
		}
		a.logger.Warn("master agent: parse failed, returning fallback",
			"master", a.persona.Key,
			"symbol", in.Symbol,
			"err", perr,
			"raw_len", rawLen,
			"raw_sample", sample,
		)
		rep.KeyRisks = append(rep.KeyRisks, fmt.Sprintf("parse_error:%v", perr))
		return rep, nil
	}
	rep.Verdict = normaliseMasterVerdict(parsed.Verdict)
	rep.Confidence = clampConfidence(parsed.Confidence)
	if t := strings.TrimSpace(parsed.Thesis); t != "" {
		rep.Thesis = t
	}
	if len(parsed.KeyReasons) > 0 {
		rep.KeyReasons = parsed.KeyReasons
	}
	if len(parsed.KeyRisks) > 0 {
		rep.KeyRisks = parsed.KeyRisks
	}
	if len(parsed.MasterSpecific) > 0 {
		rep.MasterSpecific = parsed.MasterSpecific
	}
	// red_lines_hit is the field most prone to model leakage —
	// Gemini's thinking-mode internal monologue ("Wait, I must
	// output ONLY valid JSON", "此处修正为空数组", "(Ignore previous
	// internal monologue)") ends up here because the persona
	// prompt asks the model to think about which red lines apply
	// AND the schema's maxLength constraint is not enforced
	// strictly by responseSchema. The sanitizer below drops items
	// that are either (a) too long to be a canonical phrase tag
	// or (b) match known monologue tells. Anything that survives
	// is kept as a genuine red-line hit.
	cleaned := sanitizeRedLines(parsed.RedLinesHit)
	if len(cleaned) > 0 {
		rep.RedLinesHit = cleaned
		// Honouring the persona contract: any red-line hit
		// forces the verdict to AVOID regardless of what the
		// LLM said. The Go side is authoritative on hard rules.
		rep.Verdict = "AVOID"
		if rep.Confidence < 60 {
			rep.Confidence = 60
		}
	}
	rep.LLMModel = "llm"
	if err := rep.Validate(); err != nil {
		// Validation failure → fall back to the HOLD shell so we
		// always satisfy the panel contract.
		a.logger.Warn("master agent: validation failed, returning fallback",
			"master", a.persona.Key, "err", err)
		return MasterReport{
			MasterKey:    a.persona.Key,
			MasterNameZh: a.persona.NameZh,
			MasterNameEn: a.persona.NameEn,
			Symbol:       rep.Symbol,
			AsOf:         in.AsOf,
			GeneratedAt:  a.now(),
			Verdict:      "HOLD",
			Confidence:   30,
			Thesis:       fmt.Sprintf("%s 模型输出未通过校验，保守给出 HOLD。", a.persona.NameZh),
			KeyReasons:   []string{"model_output_invalid"},
			KeyRisks:     []string{err.Error()},
			LLMModel:     "fallback",
		}, nil
	}
	return rep, nil
}

// complete dispatches to the schema-aware LLM call when the
// client supports it. Falls back to plain Complete otherwise.
func (a *MasterAgent) complete(ctx context.Context, sys, user string) (string, error) {
	if schemaClient, ok := a.llm.(SchemaLLMClient); ok {
		return schemaClient.CompleteWithSchema(ctx, sys, user, MasterReportJSONSchema)
	}
	return a.llm.Complete(ctx, sys, user)
}

// ---------------------------------------------------------------------------
// Prompt rendering
// ---------------------------------------------------------------------------

// buildSystemPrompt renders the persona into a multi-section
// system prompt. We dump the entire persona JSON (sans nullable
// fields) as a reference, so masters with bespoke frameworks
// (Munger's biases, Lynch's six categories, CANSLIM, etc.) get
// their sections in front of the LLM without per-master code.
func (a *MasterAgent) buildSystemPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "你是 %s（%s），一位风格定位为 \"%s\" 的投资大师。\n",
		a.persona.NameZh, a.persona.NameEn, a.persona.Style)
	if a.persona.HoldingPeriod != "" {
		fmt.Fprintf(&b, "你的典型持有期：%s。\n", a.persona.HoldingPeriod)
	}
	if a.persona.Philosophy != "" {
		fmt.Fprintf(&b, "你的核心投资哲学：%s\n", a.persona.Philosophy)
	}
	b.WriteString("\n你必须严格按照下面的人格 JSON 中描述的框架进行分析（包含 must_have_criteria / red_lines / qualitative_filters / 估值方法 / 输出格式 等所有字段）。\n")
	b.WriteString("当你判断时，请遵守：\n")
	b.WriteString("1) 数据缺失时，必须在 key_reasons 里诚实写 'data_unavailable: <字段>'，不要编造数字；\n")
	b.WriteString("2) red_lines_hit 数组里每一项必须是 persona 红线列表中的【原词短语】（≤ 30 个汉字 / 80 字符），不要写解释、不要写推理过程、不要写 \"此处修正/暂未触及/Wait/Ignore\" 等元语言；若未触发任何红线，直接输出 []；\n")
	b.WriteString("3) verdict 必须从 verdict_enum 中选一项（若该 persona 定义了的话）；\n")
	b.WriteString("4) 仅输出一个 JSON 对象，不要 markdown 围栏，不要解释性前言。\n")
	// Rule 5 — disambiguation. Without this the model writes
	// "最新季度增速回升至27.97%" without saying which quarter,
	// and a downstream reader (or reviewing AI) defaults the
	// referent to the wrong period and judges us factually wrong.
	// The prompt already exposes annual_period + latest_period
	// labels in the user message; this rule forces the LLM to
	// quote them back instead of using generic "近期/最新季度".
	b.WriteString("5) 在 thesis / key_reasons / key_risks 中引用任何来自 fund.* 的百分比或绝对值时，必须显式带上该数字所对应的财报期间，且优先采用 'YYYY年Q[1-4]' 或 'YYYY 年报' 的写法（例如 '2026Q1 营收同比 +27.97%'、'2025 年报归母净利同比 -28.77%'）。禁止使用 '最新季度' / '近期' / '当前' / '近一年' 等模糊措辞替代具体期间。如果 prompt 中给出的 annual_period / latest_period 缺失，则不要引用对应字段。\n")
	// Rule 6 — quality gap. earnings_growth_yoy 与
	// revenue_growth_yoy 一正一负、且绝对差大于 20 个百分点
	// 时，是经典的"增收不增利"信号，应当在 key_risks 里单独
	// 标出。Wood / Lynch 这类成长派 persona 历史上会被营收数据
	// 牵着走，对净利崩塌着墨不足；这条规则把判断 explicit 化。
	b.WriteString("6) 当 fund.revenue_growth_yoy 与 fund.earnings_growth_yoy 同时存在且符号相反（即营收增长 > 0 而盈利下滑 < 0，或反之），并且二者绝对值之差 >= 20 个百分点时，必须在 key_risks 中显式列出 '增收不增利' 或 '增利不增收' 类型的风险条目，引用具体数字与期间，且不得仅在 thesis 中一笔带过。这是 quality-gap 信号，许多成长派 persona 容易忽略。\n")
	// Rule 7 — listing tenure. 当 listing_years 提供且 < 10
	// 时，凡是"需要 10 年历史"的 must-have（如穿越周期、十年
	// 自由现金流、长期股息记录等）应在 key_reasons 中标为
	// "listing_lt_10y:N" 而不是直接 "data_unavailable"，
	// 避免把次新股的结构性限制当成公司基本面缺陷。
	b.WriteString("7) 当 prompt 中给出 listing_years 字段时（公司上市年限），若该值 < 10，针对'需要 10 年以上历史数据'的条件（如穿越周期能力、十年自由现金流、十年股息记录等），请用 'listing_lt_10y:N年' 的标签代替 'data_unavailable:<字段>'，不要把次新股的结构性限制当成公司基本面缺陷扣分。仍然可以指出'历史样本不足以验证长期穿越能力'作为风险，但请明确归因为上市年限而非财务披露问题。\n")
	b.WriteString("8) 当 prompt 中给出 latest_announce_date 字段时，引用任何以 _latest 结尾的 fund.* 字段（如 fund.revenue_growth_yoy_latest、fund.latest_revenue、fund.latest_net_income、fund.latest_revenue_qoq、fund.gross_margin_latest 等）必须做到三件事：①明确写出 latest_period 对应的财报期间（如 '2026Q1'），②写出 latest_announce_date 公告日（如 '公告日 2026-04-28'），③在百分比之外同时给出至少一个绝对值（如 '营收 2.54 亿元' 或 '净利润 1964 万元'，单位用万元/亿元，保留 2 位有效数字）。示例：'2026Q1（公告日 2026-04-28）营收 2.54 亿元，同比 +27.97%，但环比 -9.63% 显示动能见顶'。这是为了让任何外部读者都能凭借'公告日 + 绝对值'两个锚点回到原始公告核对数字，避免出现无法溯源的孤立百分比。同时，当 latest_*_qoq 与 latest_*_yoy 出现明显反向（一个 > 0 一个 < 0）时，必须在 key_reasons 或 key_risks 中把'YoY 增长但 QoQ 下滑'类的动能反转信号显式列为一条独立条目。\n")
	// Rule 9 — technical-analysis citation. Prompt may carry a
	// 'technical snapshot' block with closed prices, moving
	// averages, RSI, MACD, KDJ, support/resistance, etc. The
	// LLM MUST be able to QUOTE those values (e.g. "RSI14 78
	// overbought, price 5% above 20D high") but MUST NOT turn
	// them into a price target / stop loss / 买点 / 目标位.
	// The Publisher-mode phrase scanner (compliance/scanner.go)
	// will redact such phrases server-side, but redaction makes
	// the output read awkwardly — far better to never emit them.
	//
	// What's allowed:
	//   - quoting a level: "20-day high $128.50 acted as
	//     resistance until 2026-06-08"
	//   - past-tense observation: "MA5 crossed above MA20 on
	//     2026-06-05 (typical short-term bullish crossover)"
	//   - regime classification: "trend regime: uptrend per
	//     SMA20>SMA50>SMA200 alignment"
	//
	// What's forbidden:
	//   - forecast: "price target $150" / "目标价 $150"
	//   - directive: "entry at $128" / "买点在 $128"
	//   - signal claim: "golden cross signal triggered" /
	//     "金叉信号确认"
	//   - imperative: "go long" / "建议买入"
	//
	// The technical block, when present, is FACT. The persona's
	// reasoning ABOUT the fact stays a research observation.
	b.WriteString("9) 当 prompt 中给出 '--- technical snapshot ---' 段时，你可以在 thesis / key_reasons / key_risks 中引用其中的具体数值（如 'RSI14 78 处于超买区'、'2026-06-08 收盘 $128.50 站上 20 日高位 $125.30'、'MA20>MA50>MA200 显示多头排列'），但必须遵守以下硬约束：①禁止给出任何形式的价格预测、目标价、止损/止盈位、买点/卖点、入场/出场点（无论数值或区间）；②禁止把均线交叉描述为'信号确认/触发/即将形成'（如'金叉信号' / 'golden cross triggered'），只能描述为已观察到的事件（如'MA5 于 2026-06-05 上穿 MA20'）；③禁止使用'建议买入/卖出/加仓/减仓'、'go long/short'、'strong buy/sell rating'等含有动作指令的措辞；④技术面只能作为基本面判断的辅助信息，不得替代基本面构成核心论点。如果只有技术面信号而基本面缺失，请在 key_reasons 中明确标注 'technical_only_signal'，并保持保守 verdict。\n")
	b.WriteString("\n=== PERSONA JSON ===\n")
	if raw, err := json.MarshalIndent(a.persona.Raw, "", "  "); err == nil {
		b.Write(raw)
	}
	b.WriteString("\n=== OUTPUT JSON SCHEMA ===\n")
	b.WriteString("{\n")
	b.WriteString("  \"verdict\": \"STRONG_BUY|BUY|HOLD|AVOID|SHORT|PASS|SKIP\",\n")
	b.WriteString("  \"confidence\": 0-100,\n")
	b.WriteString("  \"thesis\": \"一段不超过 200 字的中文论述\",\n")
	b.WriteString("  \"key_reasons\": [\"3 条最关键的判断依据\"],\n")
	b.WriteString("  \"key_risks\": [\"2 条最关键的风险\"],\n")
	b.WriteString("  \"red_lines_hit\": [\"命中的红线条目，若无则空数组\"],\n")
	b.WriteString("  \"master_specific\": { \"...\": \"该 persona 特有字段，如 intrinsic_value / PEG / graham_number / CANSLIM_score 等\" }\n")
	b.WriteString("}\n")
	return b.String()
}

// buildUserPrompt renders the per-symbol data block. Marks every
// missing field explicitly so the LLM has no excuse to fabricate.
func (a *MasterAgent) buildUserPrompt(in MasterInput) string {
	var b strings.Builder
	b.WriteString("请按上述 persona 分析下面这只股票，并仅返回一个 JSON 对象。\n\n")
	fmt.Fprintf(&b, "symbol: %s\n", strings.ToUpper(in.Symbol))
	if name := strings.TrimSpace(in.Name); name != "" {
		// Surface the issuer's short name so masters reason about
		// e.g. "德科立" rather than just the ticker code 688205.
		// The persona is instructed to weave the name into 'thesis'
		// when available — improves UI legibility on the verdict
		// cards.
		fmt.Fprintf(&b, "name: %s\n", name)
	}
	if in.AssetClass != "" {
		fmt.Fprintf(&b, "asset_class: %s\n", in.AssetClass)
	}
	if in.Market != "" {
		fmt.Fprintf(&b, "market: %s\n", in.Market)
	}
	if !in.AsOf.IsZero() {
		fmt.Fprintf(&b, "asof: %s\n", in.AsOf.Format(time.RFC3339))
	}
	if in.PriceLast > 0 {
		fmt.Fprintf(&b, "price_last: %.4f %s\n", in.PriceLast, in.Currency)
	}
	if in.PriceChange != 0 {
		fmt.Fprintf(&b, "price_change_pct: %.2f%%\n", in.PriceChange*100)
	}
	b.WriteString("\n--- fundamentals snapshot ---\n")
	if in.Fundamentals == nil {
		b.WriteString("（数据缺失：未提供基本面快照）\n")
	} else {
		// Period labels FIRST so the persona reads them before the
		// numbers and understands which fiscal period each metric
		// corresponds to. Without this the LLM sees a single
		// "earnings_growth_yoy=-0.29" line and assumes it's the
		// latest available figure, missing the fact that a fresher
		// quarterly print may have already turned the trend.
		if ap := strings.TrimSpace(in.Fundamentals.AnnualPeriod); ap != "" {
			fmt.Fprintf(&b, "annual_period: %s（fund.*_yoy 字段所对应的年报口径）\n", ap)
		}
		if lp := strings.TrimSpace(in.Fundamentals.LatestPeriod); lp != "" {
			fmt.Fprintf(&b, "latest_period: %s（fund.*_yoy_latest 字段所对应的最新季报/中报口径）\n", lp)
		}
		// Listing tenure — feeds rule 7 in buildSystemPrompt. When
		// the company has been public for less than the persona's
		// long-horizon window (typically 10y), the LLM is told to
		// label the gap as 'listing_lt_10y:N年' rather than
		// 'data_unavailable:<字段>', so a 2022-IPO next-new-stock
		// doesn't get hammered for not having pre-2015 history.
		if ld := strings.TrimSpace(in.Fundamentals.ListingDate); ld != "" {
			fmt.Fprintf(&b, "listing_date: %s\n", ld)
		}
		if y := in.Fundamentals.ListingYears; y > 0 {
			fmt.Fprintf(&b, "listing_years: %.2f\n", y)
		}
		// Citation metadata — feeds rule 8 in buildSystemPrompt.
		// Surfacing both the announce date AND the source name
		// gives the LLM (and any reviewer) a verifiable anchor for
		// the *_latest figures: a critique that "27.97% Q1 2026
		// growth doesn't appear in public records" can be
		// resolved in one search if the prompt cites "公告日
		// 2026-04-28" alongside the number.
		if ad := strings.TrimSpace(in.Fundamentals.LatestAnnounceDate); ad != "" {
			fmt.Fprintf(&b, "latest_announce_date: %s（最近一次披露原始公告的日期）\n", ad)
		}
		if src := strings.TrimSpace(in.Fundamentals.LatestSource); src != "" {
			fmt.Fprintf(&b, "latest_source: %s\n", src)
		}
		if q := in.Fundamentals.QualityScore; q != nil {
			fmt.Fprintf(&b, "quality.composite_z=%.2f\n", q.CompositeZ)
			fmt.Fprintf(&b, "quality.profitability_z=%.2f\n", q.ProfitabilityZ)
			fmt.Fprintf(&b, "quality.growth_z=%.2f\n", q.GrowthZ)
			fmt.Fprintf(&b, "quality.safety_z=%.2f\n", q.SafetyZ)
			if q.Quartile > 0 {
				fmt.Fprintf(&b, "quality.quartile=%d/4\n", q.Quartile)
			}
		} else {
			b.WriteString("quality.* = （未计算）\n")
		}
		if len(in.Fundamentals.Metrics) > 0 {
			// Sort for stable prompt rendering — makes prompt
			// hashing meaningful for cache layers.
			keys := make([]string, 0, len(in.Fundamentals.Metrics))
			for k := range in.Fundamentals.Metrics {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v := in.Fundamentals.Metrics[k]
				// Default fixed-point with 4 decimals reads
				// cleanly for ratios (0.2797, -0.0963). For
				// absolute amounts (revenue / net income /
				// market cap) it produces ugly
				// "254444250.0000"-style rails that bloat
				// the prompt and force the LLM to do its
				// own magnitude formatting. Switch to %g
				// (which picks the shorter of %e/%f, giving
				// "2.5444425e+08") above 1e6 — the same
				// threshold past which decimal padding
				// stops adding signal.
				if v >= 1e6 || v <= -1e6 {
					fmt.Fprintf(&b, "fund.%s=%g\n", k, v)
				} else {
					fmt.Fprintf(&b, "fund.%s=%.4f\n", k, v)
				}
			}
		} else {
			b.WriteString("fund.* = （未提供财务指标）\n")
		}
		if len(in.Fundamentals.IndustryPeers) > 0 {
			fmt.Fprintf(&b, "industry_peers=%s\n", strings.Join(in.Fundamentals.IndustryPeers, ", "))
		}
		if hist := in.Fundamentals.History; len(hist) > 0 {
			b.WriteString("\n--- history (most recent first) ---\n")
			limit := len(hist)
			if limit > 10 {
				limit = 10
			}
			for i := 0; i < limit; i++ {
				y := hist[i]
				fmt.Fprintf(&b, "year=%d roe=%.2f%% roic=%.2f%% gross=%.2f%% op=%.2f%% net=%.2f%% fcf=%.0f eps=%.4f rev_g=%.2f%% eps_g=%.2f%%\n",
					y.Year,
					y.ReturnOnEquity*100,
					y.ReturnOnCapital*100,
					y.GrossMargin*100,
					y.OperatingMargin*100,
					y.ProfitMargin*100,
					y.FreeCashFlow,
					y.EPS,
					y.RevenueGrowthYoY*100,
					y.EarningsGrowthYoY*100,
				)
			}
		} else {
			b.WriteString("history.10yr = （历史数据不可用，对需要多年历史的条件，请在 key_reasons 写 data_unavailable:<字段>）\n")
		}
		if rp := in.Fundamentals.RulePrior; rp != nil && len(rp.Items) > 0 {
			b.WriteString("\n--- rule_based_prior (服务器侧确定性预检，必须以此为准，不要自行重算多年平均数) ---\n")
			for _, it := range rp.Items {
				fmt.Fprintf(&b, "rule.%s status=%s required=%s observed=%s",
					it.Key, it.Status, it.Required, it.Observed)
				if it.Detail != "" {
					fmt.Fprintf(&b, " detail=%s", it.Detail)
				}
				b.WriteString("\n")
			}
			if len(rp.Notes) > 0 {
				for _, n := range rp.Notes {
					fmt.Fprintf(&b, "rule.note=%s\n", n)
				}
			}
			b.WriteString("如果上述任意条目 status=FAIL，请在 key_reasons / key_risks 中明确指出，并相应调低 verdict（多条 FAIL 应触发 AVOID）。对 status=UNKNOWN 的条目，请诚实说明数据不足而不是假设。\n")
		}
	}
	if in.Technical != nil {
		renderTechnicalBlock(&b, in.Technical)
	}
	if in.Notes != "" {
		fmt.Fprintf(&b, "\n--- notes ---\n%s\n", in.Notes)
	}
	return b.String()
}

// renderTechnicalBlock writes the price-action / momentum /
// volatility section to the prompt. Deliberately compact —
// every line is a "FIELD=VALUE" pair so the LLM can quote them
// verbatim in thesis / key_reasons without rephrasing into
// trade-instruction shapes (which rule 9 would forbid anyway,
// but rendering them as raw fields short-circuits the temptation).
//
// Empty / zero fields are SKIPPED so the LLM doesn't see
// "rsi14=0" and assume the market is at the floor — better to
// honestly omit than to render misleading zeros.
//
// Tags are also rendered as bullet points because they're the
// "human-prose" view the prompt rule 9 examples reference
// directly. Without them the LLM tends to over-quantify
// ("price 0.0123 above 20D high") instead of using readable
// labels ("MA20>MA50>MA200, multi-timeframe uptrend").
func renderTechnicalBlock(b *strings.Builder, t *MasterTechnicalBlock) {
	if t == nil {
		return
	}
	b.WriteString("\n--- technical snapshot (price action / momentum / volatility) ---\n")
	if t.AsOf != "" {
		fmt.Fprintf(b, "asof=%s bars_used=%d\n", t.AsOf, t.BarsUsed)
	}
	if t.LastClose != 0 {
		fmt.Fprintf(b, "last_close=%.4f\n", t.LastClose)
	}
	// Returns: always render as percentages with 2 dp so the
	// LLM doesn't need to convert.
	if t.PctChange1D != 0 {
		fmt.Fprintf(b, "pct_change_1d=%+.2f%%\n", t.PctChange1D*100)
	}
	if t.PctChange5D != 0 {
		fmt.Fprintf(b, "pct_change_5d=%+.2f%%\n", t.PctChange5D*100)
	}
	if t.PctChange20D != 0 {
		fmt.Fprintf(b, "pct_change_20d=%+.2f%%\n", t.PctChange20D*100)
	}
	if t.PctChange52WHi != 0 {
		fmt.Fprintf(b, "pct_change_from_52w_high=%+.2f%%\n", t.PctChange52WHi*100)
	}
	if t.SMA20 != 0 {
		fmt.Fprintf(b, "sma20=%.4f\n", t.SMA20)
	}
	if t.SMA50 != 0 {
		fmt.Fprintf(b, "sma50=%.4f\n", t.SMA50)
	}
	if t.SMA200 != 0 {
		fmt.Fprintf(b, "sma200=%.4f\n", t.SMA200)
	}
	if t.MAAlignment != "" {
		fmt.Fprintf(b, "ma_alignment=%s\n", t.MAAlignment)
	}
	if t.RSI14 != 0 {
		fmt.Fprintf(b, "rsi14=%.2f", t.RSI14)
		if t.RSI14Zone != "" {
			fmt.Fprintf(b, " (%s)", t.RSI14Zone)
		}
		b.WriteString("\n")
	}
	if t.MACDLine != 0 || t.MACDSignal != 0 || t.MACDHist != 0 {
		fmt.Fprintf(b, "macd_line=%.4f macd_signal=%.4f macd_hist=%.4f", t.MACDLine, t.MACDSignal, t.MACDHist)
		if t.MACDCross != "" {
			fmt.Fprintf(b, " (fresh %s cross at latest bar)", t.MACDCross)
		}
		b.WriteString("\n")
	}
	if t.ATR14PctOfPx != 0 {
		fmt.Fprintf(b, "atr14_pct_of_price=%.2f%%\n", t.ATR14PctOfPx*100)
	}
	if t.KDJK != 0 || t.KDJD != 0 || t.KDJJ != 0 {
		fmt.Fprintf(b, "kdj_k=%.2f kdj_d=%.2f kdj_j=%.2f\n", t.KDJK, t.KDJD, t.KDJJ)
	}
	if t.Volume != 0 {
		fmt.Fprintf(b, "volume=%g\n", t.Volume)
	}
	if t.RelativeVolume != 0 {
		fmt.Fprintf(b, "relative_volume=%.2fx (latest / 20-bar SMA)\n", t.RelativeVolume)
	}
	if t.Support != 0 && t.Resistance != 0 {
		fmt.Fprintf(b, "support=%.4f resistance=%.4f sr_window=%d\n", t.Support, t.Resistance, t.SRWindow)
	}
	if t.BreakoutState != "" {
		fmt.Fprintf(b, "breakout_state=%s\n", t.BreakoutState)
	}
	if len(t.Tags) > 0 {
		b.WriteString("technical_tags:\n")
		for _, tag := range t.Tags {
			fmt.Fprintf(b, "  - %s\n", tag)
		}
	}
}

// ---------------------------------------------------------------------------
// LLM JSON parsing
// ---------------------------------------------------------------------------

// llmMasterReport is the JSON envelope the LLM returns.
type llmMasterReport struct {
	Verdict        string         `json:"verdict"`
	Confidence     int            `json:"confidence"`
	Thesis         string         `json:"thesis"`
	KeyReasons     []string       `json:"key_reasons"`
	KeyRisks       []string       `json:"key_risks"`
	RedLinesHit    []string       `json:"red_lines_hit"`
	MasterSpecific map[string]any `json:"master_specific"`
}

// parseMasterLLM mirrors parseLLMJSONReport: tolerant on the
// envelope (strips ``` fences + prose), strict on shape once the
// JSON object is isolated.
func parseMasterLLM(raw string) (llmMasterReport, error) {
	body := strings.TrimSpace(raw)
	if body == "" {
		return llmMasterReport{}, errors.New("empty llm reply")
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
		return llmMasterReport{}, errors.New("no json object found")
	}
	body = body[start : end+1]
	var out llmMasterReport
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return llmMasterReport{}, err
	}
	return out, nil
}

// normaliseMasterVerdict maps free-text verdicts onto our enum.
// Unknown values degrade to HOLD so we never poison the aggregate
// with a malformed vote.
func normaliseMasterVerdict(s string) string {
	v := strings.ToUpper(strings.TrimSpace(s))
	v = strings.ReplaceAll(v, " ", "_")
	switch v {
	case "STRONG_BUY", "STRONGLY_BUY", "VERY_BUY":
		return "STRONG_BUY"
	case "BUY", "BULLISH", "POSITIVE", "LONG":
		return "BUY"
	case "HOLD", "NEUTRAL", "WAIT", "WATCH":
		return "HOLD"
	case "AVOID", "PASS", "NO":
		return "AVOID"
	case "SHORT", "BEARISH", "NEGATIVE":
		return "SHORT"
	case "SKIP", "SKIP_NOT_APPLICABLE":
		return "SKIP"
	default:
		return "HOLD"
	}
}

// sanitizeRedLines is the defensive scrubber that drops Gemini's
// thinking-mode internal-monologue leakage from the red_lines_hit
// array. The schema constraint (maxLength 80) is supposed to
// prevent this server-side, but responseSchema enforcement is
// best-effort for length bounds, so we trust nothing and scrub
// at parse time too.
//
// Drop rules — an item is dropped if it matches ANY of:
//
//	1. length > redLineMaxRunes (catches paragraph-long monologues)
//	2. contains a known monologue tell, case-insensitive:
//	     EN: "wait,", "i must", "ignore", "monologue", "removing",
//	         "json generation", "let me", "actually", "instead"
//	     ZH: "此处", "修正为", "等待", "重做", "重置", "我必须", "实际上"
//	3. wrapped in parens AND contains an English imperative ("Ignore
//	   previous", "Setting array", "Removing invalid") — the parens
//	   are a strong signal the model is talking to itself
//
// Anything that survives is kept verbatim — the persona prompts
// emit canonical short tags like "无技术壁垒的'伪创新'" (14 chars)
// or "ROE主要靠杠杆撑起" (10 chars) which are well under any
// threshold and contain no monologue markers.
const redLineMaxRunes = 100

func sanitizeRedLines(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	monologueTells := []string{
		"wait,", "i must", "ignore previous", "internal monologue",
		"removing invalid", "json generation", "let me", "instead",
		"setting array",
		"此处", "修正为", "等待", "重做", "重置", "我必须",
		"实际上", "由于无法", "由于要求", "为了严格", "脑海中",
		"修改逻辑", "保持空数组", "强行填入",
	}
	out := make([]string, 0, len(items))
	for _, raw := range items {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		// Rune count, not byte count — most leaks are Chinese.
		if utf8RuneCount(s) > redLineMaxRunes {
			continue
		}
		lower := strings.ToLower(s)
		bad := false
		for _, tell := range monologueTells {
			if strings.Contains(lower, tell) {
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// utf8RuneCount returns the visible character count for both
// ASCII and CJK strings without pulling in unicode/utf8.RuneCountInString
// at the import site (the package already imports utf8).
func utf8RuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// MasterReportJSONSchema is the strict-JSON contract the
// SchemaLLMClient-aware path enforces. Mirrors llmMasterReport's
// field shape so the schema-strict provider (OpenAI / Gemini)
// produces parseable output natively.
//
// The explicit upper bounds on every string + array are NOT just
// cosmetic — they're a safety net against degenerate decode loops
// where a model gets stuck repeating the same phrase ("竞争对手
// 具备同等技术且规模更大。" × 200). Without the caps, the loop
// keeps emitting until maxOutputTokens runs out, the response gets
// chopped mid-multibyte UTF-8, and the downstream parser sees
// "no json object found" on a 25K char garbage blob. With the
// caps, Gemini enforces them server-side and the model is forced
// to commit to a finite, well-formed reply.
//
// Limits picked from the prompt contract ("不超过 200 字 thesis,
// 3 条 key_reasons, 2 条 key_risks") plus a 2-3× headroom so the
// model can over-shoot the soft contract without being clipped.
var MasterReportJSONSchema = []byte(`{
  "type": "object",
  "properties": {
    "verdict":        { "type": "string", "maxLength": 32 },
    "confidence":     { "type": "integer", "minimum": 0, "maximum": 100 },
    "thesis":         { "type": "string", "maxLength": 800 },
    "key_reasons":    { "type": "array", "minItems": 1, "maxItems": 6, "items": { "type": "string", "maxLength": 400 } },
    "key_risks":      { "type": "array", "minItems": 1, "maxItems": 6, "items": { "type": "string", "maxLength": 400 } },
    "red_lines_hit":  { "type": "array", "maxItems": 10, "items": { "type": "string", "maxLength": 80 } },
    "master_specific":{ "type": "object" }
  },
  "required": ["verdict", "confidence", "thesis", "key_reasons", "key_risks"],
  "additionalProperties": true
}`)

// ---------------------------------------------------------------------------
// Persona loader (reads internal/agent/masters/*.json once at boot)
// ---------------------------------------------------------------------------

var (
	personaMu     sync.RWMutex
	personaCache  map[string]MasterPersona
)

// LoadMasterPersonas reads every *.json under internal/agent/masters/
// and returns the parsed personas keyed by agent_id. Cached after the
// first successful call so subsequent panel constructions are O(1).
//
// Returns a fresh copy of the map so callers can safely mutate it.
func LoadMasterPersonas() (map[string]MasterPersona, error) {
	personaMu.RLock()
	if personaCache != nil {
		out := copyPersonaMap(personaCache)
		personaMu.RUnlock()
		return out, nil
	}
	personaMu.RUnlock()

	personaMu.Lock()
	defer personaMu.Unlock()
	// Double-check after acquiring the write lock.
	if personaCache != nil {
		return copyPersonaMap(personaCache), nil
	}

	entries, err := fs.ReadDir(masters.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("master: read masters dir: %w", err)
	}
	out := make(map[string]MasterPersona, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		raw, err := fs.ReadFile(masters.FS, name)
		if err != nil {
			return nil, fmt.Errorf("master: read %s: %w", name, err)
		}
		var persona MasterPersona
		if err := json.Unmarshal(raw, &persona); err != nil {
			return nil, fmt.Errorf("master: parse %s: %w", name, err)
		}
		// Re-parse the raw bytes into a generic map so the
		// LLM prompt can quote every persona-specific field
		// verbatim. We trust the JSON file authors to keep
		// the agent_id consistent with the filename; the
		// loader takes the filename as the canonical key when
		// they disagree.
		var rawMap map[string]any
		if err := json.Unmarshal(raw, &rawMap); err != nil {
			return nil, fmt.Errorf("master: parse-raw %s: %w", name, err)
		}
		persona.Raw = rawMap
		key := strings.TrimSuffix(strings.ToLower(path.Base(name)), ".json")
		if strings.TrimSpace(persona.Key) == "" {
			persona.Key = key
		} else if strings.ToLower(persona.Key) != key {
			// Mismatch is a bug in the JSON template — surface
			// it now rather than at request time.
			return nil, fmt.Errorf("master: filename %q does not match agent_id %q", name, persona.Key)
		}
		if err := persona.Validate(); err != nil {
			return nil, err
		}
		out[persona.Key] = persona
	}
	if len(out) == 0 {
		return nil, errors.New("master: no persona JSON files found")
	}
	personaCache = out
	return copyPersonaMap(personaCache), nil
}

func copyPersonaMap(in map[string]MasterPersona) map[string]MasterPersona {
	out := make(map[string]MasterPersona, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ResetMasterPersonaCache clears the in-memory cache. Used by
// tests so a tweaked persona file is picked up without restarting
// the process.
func ResetMasterPersonaCache() {
	personaMu.Lock()
	personaCache = nil
	personaMu.Unlock()
}
