package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// FundAssistService is the LLM-backed glue that turns a free-form
// natural-language brief ("我想做美股芯片基金，团队需要 1 个 PM, 2 个研究员
// — 一个盯 NVDA 一个盯 AMD/AVGO，1 个交易员") into a concrete
// CreateFund + AddAgent + UpdateAgent batch. It exists because users
// who don't know our config schema would otherwise have to fill out a
// dozen dropdowns to get a viable fund + team going.
//
// The interface is intentionally tiny: anything richer should be
// implemented in the LLM prompt or in validateAssistPlan. This keeps
// the surface mockable in tests (see assistChatStub) and avoids
// leaking llm package types into api/.
type FundAssistService interface {
	// Chat issues a JSON-mode-compatible chat and returns the raw
	// assistant content. Implementations are responsible for
	// supplying their own routing / tier / budgeting; assist only
	// passes the system + user messages and a userID for billing.
	Chat(ctx context.Context, userID, systemPrompt, userPrompt string) (string, error)
}

// FundAssistRequest is what the HTTP handler decodes from the body.
// Prompt is required; everything else is optional and exists either
// for previewing (DryRun) or for forcing locale-appropriate output.
type FundAssistRequest struct {
	Prompt       string `json:"prompt"`
	DryRun       bool   `json:"dryRun,omitempty"`
	LanguageHint string `json:"languageHint,omitempty"`
}

// FundAssistPlan is the structured spec the LLM must return. Fields
// mirror CreateFundInput + AgentConfig closely so the down-stream
// orchestration is mostly a 1:1 copy after validation succeeds.
//
// Why the wire-shape isn't *exactly* CreateFundInput:
//   - We freeze TradingMode to "simulation" inside the orchestrator
//     no matter what the LLM says. Live trading creation must go
//     through the explicit (and KYC-gated) UI flow.
//   - CompanyID comes from the URL, not the LLM, so we don't even
//     ask for it.
//   - Agents are denormalised here as a flat list with role + focus
//     + systemPrompt because that's how the platform creates them
//     (POST /team then PUT /team/:id).
type FundAssistPlan struct {
	Fund    FundAssistPlanFund    `json:"fund"`
	Agents  []FundAssistPlanAgent `json:"agents"`
	// Rationale is a short LLM-written summary the UI surfaces back
	// to the user so they can sanity-check what it inferred from
	// their prompt. It's purely cosmetic — server doesn't act on
	// it.
	Rationale string `json:"rationale,omitempty"`
}

type FundAssistPlanFund struct {
	Name             string                            `json:"name"`
	Description      string                            `json:"description,omitempty"`
	Market           string                            `json:"market"`
	Exchange         string                            `json:"exchange,omitempty"`
	AssetClass       string                            `json:"assetClass,omitempty"`
	BaseCurrency     string                            `json:"baseCurrency,omitempty"`
	BenchmarkSymbol  string                            `json:"benchmarkSymbol,omitempty"`
	PrimaryDirection string                            `json:"primaryDirection,omitempty"`
	InitialCapital   float64                           `json:"initialCapital,omitempty"`
	Universe         *FundAssistPlanUniverse           `json:"universe,omitempty"`
	Specialization   *FundAssistPlanSpecialization     `json:"specialization,omitempty"`
}

type FundAssistPlanUniverse struct {
	Mode    string   `json:"mode,omitempty"`
	Symbols []string `json:"symbols,omitempty"`
	Themes  []string `json:"themes,omitempty"`
}

type FundAssistPlanSpecialization struct {
	Markets      []string `json:"markets,omitempty"`
	AssetClasses []string `json:"assetClasses,omitempty"`
	Themes       []string `json:"themes,omitempty"`
	Instruments  []string `json:"instruments,omitempty"`
	StyleHints   []string `json:"styleHints,omitempty"`
}

type FundAssistPlanAgent struct {
	Role         string `json:"role"`
	Name         string `json:"name,omitempty"`
	Focus        string `json:"focus,omitempty"`
	SystemPrompt string `json:"systemPrompt,omitempty"`
}

// FundAssistResponse is what the handler writes back. On dryRun=true
// FundID is empty and Fund / Agents are nil — the UI uses Plan to
// render a confirmation screen. On dryRun=false everything is
// populated and FundID points at a freshly created fund the user can
// navigate to.
type FundAssistResponse struct {
	FundID   string                 `json:"fundId,omitempty"`
	Fund     *Fund                  `json:"fund,omitempty"`
	Agents   []Agent                `json:"agents,omitempty"`
	Plan     FundAssistPlan         `json:"plan"`
	Warnings []string               `json:"warnings,omitempty"`
}

// FundAssistPlanIssue is one validation finding. Field is dotted
// path (e.g., "fund.market", "agents[2].focus"); Code is a stable
// machine-readable token (UI can branch on it); Message is
// user-facing and Mandarin-safe.
type FundAssistPlanIssue struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// FundAssistError is returned from the handler when the LLM produced
// an invalid plan. Issues is non-empty; Plan echoes whatever the LLM
// did produce so the UI can show what was wrong instead of just
// hiding the failure behind a generic 500.
type FundAssistError struct {
	Issues []FundAssistPlanIssue
	Plan   FundAssistPlan
}

func (e *FundAssistError) Error() string {
	if e == nil {
		return "<nil>"
	}
	parts := make([]string, 0, len(e.Issues))
	for _, iss := range e.Issues {
		parts = append(parts, fmt.Sprintf("%s: %s", iss.Field, iss.Message))
	}
	return strings.Join(parts, "; ")
}

// ErrFundAssistEmptyPlan is returned by extractAssistPlan when the
// LLM returned content that didn't contain any JSON object (or one
// that couldn't be decoded). Wrapping a sentinel makes the handler
// route it to a 502 ("LLM produced unusable output") rather than a
// 422 ("plan was invalid") — the latter is reserved for plans that
// parsed but failed validation.
var ErrFundAssistEmptyPlan = errors.New("llm returned no usable plan")

// SupportedAssistMarkets is the canonical list of market codes this
// flow accepts. Pinned here (rather than read from a registry) so
// tests don't drift and so the LLM prompt can echo it verbatim.
var SupportedAssistMarkets = []string{"a_share", "us_equity", "hk_equity", "crypto", "futures"}

// supportedAssistRoles mirrors what TeamService.AddAgent will accept
// downstream. Pinning here lets the assist layer reject unknown
// roles ("strategist", "analyst") *before* a partial team gets half-
// created.
var supportedAssistRoles = []string{"pm", "researcher", "trader", "risk"}

// reUSTicker / reAShareTicker / reHKTicker / reFuturesContract
// are best-effort symbol classifiers used by the validator to
// catch the common "wrong market for this instrument" mistake the
// LLM occasionally makes. They are intentionally permissive — false
// negatives (skipping validation) are preferred over false positives
// because the user can always inspect the dryRun preview before
// committing. The patterns are anchored so partial matches don't
// leak through.
var (
	// US equity tickers: 1–5 uppercase letters, optionally with a
	// dot-suffix class share (BRK.B). Matches NVDA, AMD, AVGO,
	// BRK.B; rejects 600519, 0700, ESM5.
	reUSTicker = regexp.MustCompile(`^[A-Z]{1,5}(\.[A-Z])?$`)
	// A-share tickers: exactly 6 digits, optionally with a known
	// suffix. Matches 600519, 688205, 000001.SZ, 600519.SH; rejects
	// NVDA, 0700.HK.
	reAShareTicker = regexp.MustCompile(`^\d{6}(\.(SH|SZ|SS|XSHG|XSHE))?$`)
	// HK equity tickers: 4 or 5 digits, optionally .HK suffix.
	// Matches 0700, 00700, 0700.HK, 9988.HK; rejects 600519, NVDA.
	reHKTicker = regexp.MustCompile(`^\d{4,5}(\.HK)?$`)
)

// validateAssistPlan applies server-side checks on the LLM output.
// Returns nil + warnings on success, or *FundAssistError on failure.
// Warnings are non-fatal nudges (e.g., "no benchmarkSymbol — falling
// back to default") that the UI can surface but don't block creation.
//
// The validator is intentionally strict on cross-market consistency
// (the user's #1 stated requirement) and lenient on cosmetic gaps
// (we'd rather silently fill in a sensible default than reject the
// plan and force the user to re-prompt).
func validateAssistPlan(plan FundAssistPlan) (warnings []string, _ *FundAssistError) {
	issues := make([]FundAssistPlanIssue, 0, 4)
	addIssue := func(field, code, msg string) {
		issues = append(issues, FundAssistPlanIssue{Field: field, Code: code, Message: msg})
	}

	if strings.TrimSpace(plan.Fund.Name) == "" {
		addIssue("fund.name", "required", "基金名称不能为空，请在描述里给出（例如 \"美股芯片精选\"）")
	}
	market := strings.TrimSpace(plan.Fund.Market)
	if market == "" {
		addIssue("fund.market", "required", "需要明确市场（a_share / us_equity / hk_equity / crypto / futures）")
	} else if !containsString(SupportedAssistMarkets, market) {
		addIssue("fund.market", "unsupported", fmt.Sprintf("市场 %q 不在支持列表里：%s", market, strings.Join(SupportedAssistMarkets, ", ")))
	}

	if len(plan.Agents) == 0 {
		addIssue("agents", "required", "团队至少需要 1 个 PM。可以在描述里加上 \"团队需要一个组合经理\"")
	}

	pmCount := 0
	for i, ag := range plan.Agents {
		field := fmt.Sprintf("agents[%d]", i)
		role := strings.ToLower(strings.TrimSpace(ag.Role))
		switch role {
		case "":
			addIssue(field+".role", "required", "请说明这个成员的角色（pm / researcher / trader / risk）")
		case "pm":
			pmCount++
		case "researcher", "trader", "risk":
		default:
			addIssue(field+".role", "unsupported", fmt.Sprintf("角色 %q 不被支持，可选：%s", ag.Role, strings.Join(supportedAssistRoles, ", ")))
		}

		// Cross-market consistency: if a researcher's focus looks
		// like an instrument from a different market than the
		// fund, reject. This is the canonical "美股基金不能塞 A 股
		// 研究员" guard the user explicitly asked for.
		if role == "researcher" && market != "" {
			focus := strings.TrimSpace(ag.Focus)
			if focus != "" {
				if mismatch := detectFocusMarketMismatch(focus, market); mismatch != "" {
					addIssue(field+".focus", "market_mismatch",
						fmt.Sprintf("研究员的标的 %q 看起来属于 %s 市场，与基金市场 %s 不匹配；请在描述里换一个该市场的标的，或者改基金市场",
							focus, mismatch, market))
				}
			}
		}
	}
	if pmCount == 0 && len(plan.Agents) > 0 {
		addIssue("agents", "missing_pm", "团队里至少要有 1 个 pm（组合经理）才能跑工作流。请在描述里说明 \"需要一个 PM\"")
	}

	// Specialization markets (if the LLM bothered to set them) must
	// be a subset of {fund.market}. We're strict here: a US fund
	// with team.markets = ["us_equity", "a_share"] is the exact
	// silent leakage the user complained about.
	if plan.Fund.Specialization != nil && plan.Fund.Specialization.Markets != nil && market != "" {
		for j, m := range plan.Fund.Specialization.Markets {
			mm := strings.TrimSpace(m)
			if mm == "" {
				continue
			}
			if mm != market {
				addIssue(fmt.Sprintf("fund.specialization.markets[%d]", j), "market_mismatch",
					fmt.Sprintf("团队研究市场 %q 与基金市场 %q 不一致；同一只基金的研究范围应当聚焦在该市场内", mm, market))
			}
		}
	}

	// Universe symbols — do a soft check for crossed markets. We
	// only WARN here (not reject): users sometimes intentionally
	// pick HK ADRs or cross-listed names, and we don't want to
	// block edge cases. The reject-level guard above already
	// catches the obviously-broken case where the team itself is
	// cross-market.
	if plan.Fund.Universe != nil && market != "" {
		for _, sym := range plan.Fund.Universe.Symbols {
			s := strings.TrimSpace(sym)
			if s == "" {
				continue
			}
			if mm := detectFocusMarketMismatch(s, market); mm != "" {
				warnings = append(warnings, fmt.Sprintf("Universe 里的 %q 看起来属于 %s 而非 %s — 请人工确认", s, mm, market))
			}
		}
	}

	if len(issues) > 0 {
		return warnings, &FundAssistError{Issues: issues, Plan: plan}
	}
	return warnings, nil
}

// detectFocusMarketMismatch returns the *inferred* market for sym if
// it looks like it belongs somewhere other than fundMarket, or "" if
// the symbol is either ambiguous (so don't risk a false reject) or
// matches.
//
// Rationale: we don't have a global symbol registry available at
// validation time (and shouldn't — it'd require a network round-trip
// per /assist call). Regex classification covers the common case
// (NVDA in a 美股 fund: pass; 600519 in a 美股 fund: reject) and
// gracefully degrades on unknown formats by returning "" (no
// rejection). The fallout is detected later at trade-time anyway by
// the existing market routing.
func detectFocusMarketMismatch(sym, fundMarket string) string {
	s := strings.ToUpper(strings.TrimSpace(sym))
	if s == "" {
		return ""
	}

	matched := ""
	switch {
	case reUSTicker.MatchString(s):
		matched = "us_equity"
	case reAShareTicker.MatchString(s):
		matched = "a_share"
	case reHKTicker.MatchString(s):
		// HK and A-share both use digits — the regex orders matter.
		// Anything that matched A-share above will be caught there
		// first; HK matches the leftover 4-digit shapes.
		matched = "hk_equity"
	default:
		return ""
	}
	if matched == fundMarket {
		return ""
	}
	return matched
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// extractAssistPlan parses the LLM's raw text. We tolerate two
// shapes: pure JSON object, and JSON wrapped in a fenced code block
// like ```json {...} ```. Returns ErrFundAssistEmptyPlan when no
// usable JSON object was found so the caller can render a 502
// distinct from a 422.
func extractAssistPlan(raw string) (FundAssistPlan, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return FundAssistPlan{}, ErrFundAssistEmptyPlan
	}
	// Strip a leading ```json (or just ```) fence if present.
	if strings.HasPrefix(s, "```") {
		// Drop the opening fence (and an optional language tag) up
		// to the first newline — we don't care about the language
		// tag, only the JSON content.
		if nl := strings.Index(s, "\n"); nl >= 0 {
			s = s[nl+1:]
		}
		// Drop a trailing ``` on its own line (or anywhere the
		// LLM tucked it).
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}
	// Find the outermost JSON object. The LLM sometimes prefixes a
	// chatty preamble even when we ask for JSON-only — we scan for
	// the first '{' and the matching closing '}' by depth.
	startIdx := strings.Index(s, "{")
	if startIdx < 0 {
		return FundAssistPlan{}, ErrFundAssistEmptyPlan
	}
	depth := 0
	endIdx := -1
	for i := startIdx; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				endIdx = i
				break
			}
		}
		if endIdx >= 0 {
			break
		}
	}
	if endIdx < 0 {
		return FundAssistPlan{}, ErrFundAssistEmptyPlan
	}
	jsonBlob := s[startIdx : endIdx+1]
	var plan FundAssistPlan
	if err := json.Unmarshal([]byte(jsonBlob), &plan); err != nil {
		return FundAssistPlan{}, fmt.Errorf("decode plan json: %w", err)
	}
	return plan, nil
}

// buildAssistSystemPrompt is the LLM contract. We embed the exact
// JSON shape we want, the supported enums, and an explicit
// "single-market only" rule so the model is biased toward the
// right answer before server-side validation runs.
//
// Why so verbose: weaker models (deepseek-chat, gpt-4o-mini) latch
// onto examples and enum lists much more reliably than terse
// instructions. The extra ~400 tokens of prompt is worth it for
// first-shot success.
//
// The two language branches are deliberately not produced by
// running the zh template through a translator at runtime — the
// schema, enum set, and formatting rules are load-bearing for
// downstream parsing, so we hand-author both versions and pin them
// with the english-smoke test in Step 12.
func buildAssistSystemPrompt(languageHint string) string {
	lang := strings.TrimSpace(languageHint)
	if lang == "" {
		lang = "zh-CN"
	}
	if strings.HasPrefix(strings.ToLower(lang), "en") {
		return `You are the "Fund Creation Assistant" on the fundai platform. The user describes the fund they want to launch, and what teammates they need, in free-form natural language. Your job is to translate that description into a structured JSON plan.

[Hard rules]
1. Output exactly one JSON object. No commentary, greeting, or code-fence markers.
2. When a field is missing, fall back to a reasonable default; when unknown, omit rather than fabricate.
3. Every market code MUST be one of the whitelist: a_share, us_equity, hk_equity, crypto, futures. A fund has exactly one market.
4. Each team member's focus must belong to the fund's market. For example, a us_equity fund must NOT contain A-share/HK tickers like 600519 / 0700; an a_share fund must NOT contain US tickers like NVDA / AAPL.
5. fund.specialization.markets, when present, must contain only the fund's own market. Multi-market funds are forbidden.
6. The team must contain at least one role=pm member; without a PM the workflow cannot run.
7. Role must be one of: pm, researcher, trader, risk.
8. Respond in English. Write description / rationale / systemPrompt fields in English.

[Output JSON schema]
{
  "fund": {
    "name": "string, fund name",
    "description": "string, 1-2 sentence fund overview",
    "market": "a_share | us_equity | hk_equity | crypto | futures",
    "exchange": "optional, e.g. NASDAQ, NYSE, SSE, SZSE, HKEX",
    "assetClass": "optional, e.g. equity, futures, crypto",
    "baseCurrency": "optional, e.g. USD, CNY, HKD",
    "primaryDirection": "long | long_short | short, default long",
    "initialCapital": number, initial capital (in baseCurrency), default 1000000,
    "universe": {
      "mode": "explicit | sector | theme",
      "symbols": ["optional, explicit ticker list"],
      "themes": ["optional, theme tags"]
    },
    "specialization": {
      "markets": ["must contain only fund.market"],
      "themes": ["optional"],
      "instruments": ["optional"]
    }
  },
  "agents": [
    {
      "role": "pm | researcher | trader | risk",
      "name": "string, role display name",
      "focus": "optional, the researcher's specific theme / ticker (e.g. \"NVDA\" or \"semiconductors\")",
      "systemPrompt": "1-3 English sentences describing this agent's responsibility, scope, and output requirements"
    }
  ],
  "rationale": "1-2 English sentences explaining how you interpreted the user's brief"
}

Return JSON only, no additional text.`
	}
	return fmt.Sprintf(`你是 fundai 平台的"基金创建助手"。用户会用自然语言告诉你他想做什么样的基金，以及团队需要哪些成员；你的任务是把这段描述转换为结构化的 JSON 计划。

【硬性规则】
1. 严格只输出一个 JSON 对象，不要附加任何解释、问候或代码块标记。
2. 字段缺失时使用合理的默认值；未知时省略而不是编造。
3. 所有市场代码必须从这个白名单中选一个：a_share, us_equity, hk_equity, crypto, futures。一只基金只能有一个市场。
4. 团队成员的研究方向（focus）必须属于基金所在的市场。例如 us_equity 基金里禁止出现 600519、0700 这种 A 股 / 港股代码；a_share 基金里禁止出现 NVDA、AAPL 这种美股代码。
5. fund.specialization.markets 如果给出，只能包含基金本身的市场代码，不允许多市场。
6. 团队里必须至少有 1 个 role=pm，没有 PM 的基金无法运行工作流。
7. role 字段只能是：pm, researcher, trader, risk。
8. 用户说的语言：%s。systemPrompt / description 等中文字段就用中文写，英文场景就用英文。

【输出 JSON Schema】
{
  "fund": {
    "name": "string，基金名称",
    "description": "string，1-2 句基金简介",
    "market": "a_share | us_equity | hk_equity | crypto | futures",
    "exchange": "可选，例如 NASDAQ, NYSE, SSE, SZSE, HKEX",
    "assetClass": "可选，例如 equity, futures, crypto",
    "baseCurrency": "可选，例如 USD, CNY, HKD",
    "primaryDirection": "long | long_short | short，默认 long",
    "initialCapital": 数字，初始资金（按 baseCurrency），默认 1000000,
    "universe": {
      "mode": "explicit | sector | theme",
      "symbols": ["可选，明确的标的列表"],
      "themes": ["可选，主题标签"]
    },
    "specialization": {
      "markets": ["必须只包含 fund.market"],
      "themes": ["可选"],
      "instruments": ["可选"]
    }
  },
  "agents": [
    {
      "role": "pm | researcher | trader | risk",
      "name": "string，角色显示名（中文优先）",
      "focus": "可选，研究员的具体方向 / 标的（例如 \"NVDA\" 或 \"半导体\"）",
      "systemPrompt": "1-3 句中文，告诉这个 agent 自己的职责 / 关注范围 / 输出要求"
    }
  ],
  "rationale": "1-2 句中文，告诉用户你是怎么理解他的需求的"
}

只返回 JSON，不要任何额外文本。`, lang)
}

// buildAssistUserPrompt prefixes the user's free-form brief with a
// "remember the schema" reminder. Without this nudge the LLM
// occasionally answers conversationally ("好的，我帮您设计一个基金...")
// and skips the JSON entirely. The reminder is short and stable so
// it doesn't blow up the prompt cache.
func buildAssistUserPrompt(brief, languageHint string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(languageHint)), "en") {
		return "User brief:\n\n" + strings.TrimSpace(brief) + "\n\nReturn JSON exactly per the schema in the system prompt."
	}
	return "用户的需求如下：\n\n" + strings.TrimSpace(brief) + "\n\n请按 system prompt 中的 schema 输出 JSON。"
}

// computeAssistPlan runs the full LLM round-trip + parsing path and
// returns either a validated plan + warnings, or a typed error. It
// does NOT touch the DB — orchestrating the actual fund/team
// creation is the handler's job. Splitting it makes the LLM step
// trivially mockable and lets unit tests pin parser + validator
// behaviour without spinning up the Fund/Team services.
func computeAssistPlan(ctx context.Context, svc FundAssistService, userID string, req FundAssistRequest) (FundAssistPlan, []string, error) {
	if svc == nil {
		return FundAssistPlan{}, nil, errors.New("assist service not configured")
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return FundAssistPlan{}, nil, errors.New("prompt is required")
	}

	system := buildAssistSystemPrompt(req.LanguageHint)
	user := buildAssistUserPrompt(prompt, req.LanguageHint)

	raw, err := svc.Chat(ctx, userID, system, user)
	if err != nil {
		return FundAssistPlan{}, nil, fmt.Errorf("llm chat failed: %w", err)
	}

	plan, err := extractAssistPlan(raw)
	if err != nil {
		return FundAssistPlan{}, nil, err
	}

	warnings, vErr := validateAssistPlan(plan)
	if vErr != nil {
		return plan, warnings, vErr
	}
	return plan, warnings, nil
}

// applyAssistDefaults fills in sensible defaults for a validated plan
// before it goes downstream. Centralising the logic here keeps the
// pre/post-validation states comparable: anything the user-facing
// /assist preview shows is post-default, so what they confirm is what
// gets created.
//
// Why not push these into the LLM prompt: defaults like "fall back
// to simulation tradingMode" or "default initial capital = 1M" are
// platform invariants — they shouldn't be at the LLM's discretion
// because a model that hallucinates "live" trading would otherwise
// trigger the KYC gate and confuse the UX. Pinning them server-side
// also means future default changes are a one-line patch instead of
// a prompt rewrite + re-eval cycle.
func applyAssistDefaults(plan FundAssistPlan) FundAssistPlan {
	if plan.Fund.InitialCapital <= 0 {
		// 1M of whatever base currency is — matches the seed
		// scripts and the manual "create fund" form's default.
		plan.Fund.InitialCapital = 1_000_000
	}
	if plan.Fund.PrimaryDirection == "" {
		plan.Fund.PrimaryDirection = "long"
	}
	if plan.Fund.AssetClass == "" {
		switch plan.Fund.Market {
		case "us_equity", "a_share", "hk_equity":
			plan.Fund.AssetClass = "equity"
		case "crypto":
			plan.Fund.AssetClass = "crypto"
		case "futures":
			plan.Fund.AssetClass = "futures"
		}
	}
	if plan.Fund.BaseCurrency == "" {
		switch plan.Fund.Market {
		case "us_equity", "crypto":
			plan.Fund.BaseCurrency = "USD"
		case "a_share":
			plan.Fund.BaseCurrency = "CNY"
		case "hk_equity":
			plan.Fund.BaseCurrency = "HKD"
		case "futures":
			// Futures basket can be cross-currency; default to
			// USD because that's the most common cross-listed
			// settlement currency.
			plan.Fund.BaseCurrency = "USD"
		}
	}
	// Ensure specialization.markets is populated (single-market
	// invariant) even if the LLM forgot. This is what feeds the
	// research-agent guardrails downstream.
	if plan.Fund.Specialization == nil {
		plan.Fund.Specialization = &FundAssistPlanSpecialization{Markets: []string{plan.Fund.Market}}
	} else if len(plan.Fund.Specialization.Markets) == 0 {
		plan.Fund.Specialization.Markets = []string{plan.Fund.Market}
	}
	return plan
}

// planToCreateInput converts the validated assist plan into the
// CreateFundInput the FundService expects. Note the explicit
// TradingMode = "simulation": we DO NOT let the LLM pick live mode.
// (Live trading must go through the explicit KYC-gated path; assist
// is for getting a config skeleton, not for spinning up production
// capital.)
func planToCreateInput(companyID string, plan FundAssistPlan) CreateFundInput {
	in := CreateFundInput{
		CompanyID:        companyID,
		Name:             plan.Fund.Name,
		Description:      plan.Fund.Description,
		TradingMode:      "simulation",
		InitialCapital:   plan.Fund.InitialCapital,
		Market:           plan.Fund.Market,
		Exchange:         plan.Fund.Exchange,
		AssetClass:       plan.Fund.AssetClass,
		BaseCurrency:     plan.Fund.BaseCurrency,
		BenchmarkSymbol:  plan.Fund.BenchmarkSymbol,
		PrimaryDirection: plan.Fund.PrimaryDirection,
	}
	if u := plan.Fund.Universe; u != nil {
		in.Universe = &FundUniverse{
			Mode:    u.Mode,
			Symbols: u.Symbols,
			Themes:  u.Themes,
		}
	}
	if s := plan.Fund.Specialization; s != nil {
		in.Specialization = &FundSpecialization{
			Team: &FundTeamSpecialization{
				Markets:      s.Markets,
				AssetClasses: s.AssetClasses,
				Themes:       s.Themes,
				Instruments:  s.Instruments,
				StyleHints:   s.StyleHints,
			},
		}
	}
	return in
}
