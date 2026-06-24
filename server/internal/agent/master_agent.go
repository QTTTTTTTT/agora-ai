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
	"math"
	"path"
	"sort"
	"strconv"
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
	Key           string   `json:"agent_id"`
	NameZh        string   `json:"name_zh"`
	NameEn        string   `json:"name_en"`
	Style         string   `json:"style"`
	HoldingPeriod string   `json:"holding_period"`
	Philosophy    string   `json:"philosophy"`
	VerdictEnum   []string `json:"verdict_enum,omitempty"`

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
	MasterKey    string `json:"master_key"`
	MasterNameZh string `json:"master_name_zh"`
	MasterNameEn string `json:"master_name_en"`
	Symbol       string `json:"symbol"`
	// SymbolName is the issuer's short Chinese / English name (e.g.
	// "德科立"). Optional — empty when the upstream provider doesn't
	// resolve it. Plumbed through so the UI can show
	// "德科立 (688205)" without round-tripping a separate lookup.
	SymbolName     string         `json:"symbol_name,omitempty"`
	AsOf           time.Time      `json:"asof"`
	GeneratedAt    time.Time      `json:"generated_at"`
	Verdict        string         `json:"verdict"`
	Confidence     int            `json:"confidence"`
	Thesis         string         `json:"thesis"`
	KeyReasons     []string       `json:"key_reasons"`
	KeyRisks       []string       `json:"key_risks"`
	MasterSpecific map[string]any `json:"master_specific,omitempty"`
	RedLinesHit    []string       `json:"red_lines_hit,omitempty"`
	// RedLinesHitEn is the parallel English translation of any
	// red-line entries triggered. Populated only when the persona
	// JSON ships a `red_lines_en` array of equal length; the
	// front-end picks this list when language=en-US so SEC marketing
	// surfaces don't bleed Chinese into English copy. The original
	// RedLinesHit (Chinese, verbatim) stays as-is so the compliance
	// scanner's exact-string match keeps working.
	RedLinesHitEn    []string `json:"red_lines_hit_en,omitempty"`
	LLMModel         string   `json:"llm_model,omitempty"`
	PromptTokens     int      `json:"prompt_tokens,omitempty"`
	CompletionTokens int      `json:"completion_tokens,omitempty"`
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
	Symbol string
	// Name is the issuer's short Chinese / English name (e.g.
	// "德科立", "Apple Inc."). Optional — when present the
	// master prompt prefixes it so the LLM reasons about the
	// company by name rather than only by ticker, and the UI
	// shows e.g. "德科立 (688205)" in the verdict header.
	Name        string
	AssetClass  string
	Market      string
	AsOf        time.Time
	PriceLast   float64
	PriceChange float64
	Currency    string

	// Fundamentals is the single-period snapshot from
	// internal/fundamental (PE, PB, ROE, dividend yield, …).
	// Phase 2 extends this with History so masters can
	// validate "10年ROE平均≥15%" type criteria. nil-safe:
	// the agent prompts the LLM to honestly say "数据缺失"
	// rather than fabricate values.
	Fundamentals *FundamentalsBlock

	// Technical is the price-action / momentum / volatility
	// snapshot. Optional — when the wiring layer's OHLC fetcher
	// can't reach the symbol (no bars, upstream throttled, etc.)
	// this stays nil and the prompt just omits the section. The
	// LLM is instructed (rule 9) to never invent technical values.
	Technical *MasterTechnicalBlock

	// Notes is freeform context. Operators may prepend
	// "earnings in 3 days" or "$$$ insider buying detected"
	// to bias the prompt.
	Notes string
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
		Thesis:       fmt.Sprintf("%s (%s): insufficient data to form a strong view at this time.", a.persona.NameEn, a.persona.NameZh),
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

	raw, parsed, perr := a.completeAndParseWithRetry(ctx, sys, user, in.Symbol)
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
	// Strict whitelist: only keep entries that are an EXACT match
	// (after trim) to one of the persona's canonical red_lines.
	// This drops every LLM monologue leak the byte-pattern blacklist
	// missed — schema field names ("master_specific", "expected_IRR"),
	// reasoning suffixes ("传统行业的小修小补（修正）"), Hindi character
	// slop ("传统行业的小修小补 मासूम]"), thinking-mode tails ("最终数组：…"),
	// etc. The persona JSON is authoritative; if the model invents a
	// new red-line phrase, we drop it instead of trusting it.
	cleaned = whitelistAgainstPersona(cleaned, a.persona.Raw)
	// Mega-cap exemption: certain red-lines logically can't apply
	// to companies that themselves are the "scale incumbent". Drop
	// them so the LLM doesn't end up flagging GOOGL / MSFT / META
	// as "out-scaled by a competitor". Threshold is $500B market
	// cap (USD), measured against the snapshot fundamentals.
	cleaned = applyMegaCapExemptions(cleaned, in.Fundamentals)
	if len(cleaned) > 0 {
		rep.RedLinesHit = cleaned
		// Map each Chinese red-line hit to its English counterpart
		// using the persona's parallel red_lines / red_lines_en
		// arrays. Anything that doesn't have an exact match in the
		// canonical list falls back to its Chinese form so the UI
		// at least shows something rather than dropping the entry.
		rep.RedLinesHitEn = translateRedLinesHit(cleaned, a.persona.Raw)
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
			Thesis:       fmt.Sprintf("%s: model output failed validation; defaulting to a conservative HOLD.", a.persona.NameEn),
			KeyReasons:   []string{"model_output_invalid"},
			KeyRisks:     []string{err.Error()},
			LLMModel:     "fallback",
		}, nil
	}
	return rep, nil
}

func (a *MasterAgent) completeAndParseWithRetry(ctx context.Context, sys, user, symbol string) (string, llmMasterReport, error) {
	const maxAttempts = 2
	var lastRaw string
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		prompt := user
		if attempt > 1 {
			// Change the prompt text so the LLM cache does not replay the
			// previous empty/truncated response. This is intentionally short:
			// daily_picks can fan out hundreds of master calls, and retries
			// must preserve prefix-cache friendliness while forcing a complete
			// visible JSON answer.
			prompt += "\n\nRETRY_INSTRUCTION: Your previous reply was empty or malformed. Return exactly one complete compact JSON object now. No markdown, no prose, close every brace."
		}

		raw, err := a.complete(ctx, sys, prompt)
		lastRaw = raw
		if err != nil {
			lastErr = fmt.Errorf("llm_error:%w", err)
			if attempt < maxAttempts {
				a.logger.Warn("master agent: LLM failed, retrying",
					"master", a.persona.Key, "symbol", symbol, "attempt", attempt, "err", err)
				continue
			}
			return lastRaw, llmMasterReport{}, lastErr
		}

		parsed, perr := parseMasterLLM(raw)
		if perr == nil {
			return raw, parsed, nil
		}
		lastErr = perr
		if attempt < maxAttempts && isRetryableMasterParseError(perr) {
			a.logger.Warn("master agent: parse failed, retrying",
				"master", a.persona.Key, "symbol", symbol, "attempt", attempt, "err", perr, "raw_len", len(strings.TrimSpace(raw)))
			continue
		}
		return lastRaw, llmMasterReport{}, lastErr
	}
	return lastRaw, llmMasterReport{}, lastErr
}

func isRetryableMasterParseError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "empty llm reply") ||
		strings.Contains(msg, "no json object found") ||
		strings.Contains(msg, "unexpected end of JSON") ||
		strings.Contains(msg, "unexpected EOF")
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
//
// Output language contract: the prompt is written in English and
// instructs the model to respond in English. The persona JSON
// itself can contain Chinese descriptions (philosophy, must-have
// criteria, etc.) — modern LLMs read them as context but obey
// the prompt's "respond in English" instruction. This is the
// 6/11/2026 reset: previously the entire prompt was Chinese, so
// even when the UI ran with language=en-US the
// daily_picks.result_json was filled with Chinese thesis blobs
// the user couldn't read. Switching the prompt to English makes
// English the default; rare Chinese-UI readers will be served
// via a future translation layer.
func (a *MasterAgent) buildSystemPrompt() string {
	var b strings.Builder
	// Prefix-cache friendly layout: keep the universal contract at the
	// very beginning and byte-identical across every master. DeepSeek's
	// automatic context cache keys on stable prefixes; putting persona-
	// specific text first would prevent cross-master reuse of this large
	// rule block. Dynamic stock data lives only in the user prompt.
	b.WriteString("You are an investment master agent. Apply the PERSONA below to the stock in the user message and return one compact JSON object.\n")
	b.WriteString("GLOBAL RULES:\n")
	b.WriteString("1) Missing data: write 'data_unavailable:<field>'; never invent numbers. If listing_years < 10, use 'listing_lt_10y:N years' for criteria requiring 10y+ history.\n")
	b.WriteString("2) red_lines_hit entries MUST be quoted verbatim from the persona.red_lines list (<=80 chars); [] if none. Do not translate, explain, or add inner monologue.\n")
	b.WriteString("3) verdict MUST use persona.verdict_enum when present.\n")
	b.WriteString("4) Output exactly ONE compact JSON object, no prose/fences. thesis MAX 60 words; exactly 3 key_reasons (MAX 20 words each); exactly 2 key_risks (MAX 20 words each); master_specific = {} unless 1-5 short scalar fields add value. Do NOT repeat the persona or restate input data.\n")
	b.WriteString("5) Cite periods for any fund.* percentage/amount: use YYYY-Q[1-4] or annual_period/latest_period. Do NOT use vague phrases like 'recent/latest quarter'. For *_latest cite latest_announce_date (e.g. 2026-04-28) plus one absolute value (e.g. $254M).\n")
	b.WriteString("6) If revenue_growth_yoy and earnings_growth_yoy have opposite signs with a >=20 percentage points gap, add a dedicated revenue-up-profit-down/profit-up-revenue-down risk with numbers and period. If latest YoY/QoQ signs conflict (e.g. 2026-Q1), add a dedicated momentum-reversal reason/risk.\n")
	b.WriteString("7) Technical data may support fundamentals, but never give price forecasts, targets, stop-loss/take-profit, entry/exit levels, or action imperatives. Describe moving-average crosses only as past observations.\n")
	b.WriteString("8) RESPOND IN ENGLISH for thesis/key_reasons/key_risks/master_specific. Exception: red_lines_hit entries MUST be quoted verbatim.\n")
	b.WriteString("\n=== PERSONA SUMMARY ===\n")
	fmt.Fprintf(&b, "You are %s (%s), an investment master whose style is \"%s\".\n",
		a.persona.NameEn, a.persona.NameZh, a.persona.Style)
	if a.persona.HoldingPeriod != "" {
		fmt.Fprintf(&b, "Your typical holding period: %s.\n", a.persona.HoldingPeriod)
	}
	if a.persona.Philosophy != "" {
		fmt.Fprintf(&b, "Your core investment philosophy: %s\n", a.persona.Philosophy)
	}
	b.WriteString("Analyse strictly within the persona JSON below, including must_have_criteria, red_lines, qualitative_filters, valuation_method and output_format.\n")
	b.WriteString("\n=== PERSONA JSON ===\n")
	// Compact JSON is materially cheaper than pretty-printed JSON at
	// daily-picks scale (hundreds of master calls per run) and preserves
	// exactly the same semantic content for the model.
	if raw, err := json.Marshal(a.persona.Raw); err == nil {
		b.Write(raw)
	}
	b.WriteString("\n=== OUTPUT JSON SHAPE ===\n")
	b.WriteString("Return exactly one compact JSON object with keys: verdict, confidence, thesis (a single English paragraph, MAX 60 words), key_reasons (exactly the 3 most decisive judgement factors, each in English and MAX 20 words), key_risks (exactly the 2 most material risks, each in English and MAX 20 words), red_lines_hit, master_specific (compact, max 5 fields).\n")
	return b.String()
}

// buildUserPrompt renders the per-symbol data block. Marks every
// missing field explicitly so the LLM has no excuse to fabricate.
func (a *MasterAgent) buildUserPrompt(in MasterInput) string {
	var b strings.Builder
	b.WriteString("Apply the persona above to the stock below and return exactly ONE JSON object. Respond in English (see GLOBAL RULES in the system prompt).\n\n")
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
		// Daily-picks cache keys include the full prompt. A seconds-level
		// timestamp makes otherwise identical manual reruns miss the
		// 24h advisor_master chat cache and re-burn tokens. The filing
		// periods and technical.asof carry the real data freshness; the
		// top-level prompt only needs the run date.
		fmt.Fprintf(&b, "asof_date: %s\n", in.AsOf.Format(time.DateOnly))
	}
	if in.PriceLast > 0 {
		fmt.Fprintf(&b, "price_last: %.4f %s\n", in.PriceLast, in.Currency)
	}
	if in.PriceChange != 0 {
		fmt.Fprintf(&b, "price_change_pct: %.2f%%\n", in.PriceChange*100)
	}
	b.WriteString("\n--- fundamentals snapshot ---\n")
	if in.Fundamentals == nil {
		b.WriteString("(data missing: no fundamentals snapshot was provided)\n")
	} else {
		// Period labels FIRST so the persona reads them before the
		// numbers and understands which fiscal period each metric
		// corresponds to. Without this the LLM sees a single
		// "earnings_growth_yoy=-0.29" line and assumes it's the
		// latest available figure, missing the fact that a fresher
		// quarterly print may have already turned the trend.
		if ap := strings.TrimSpace(in.Fundamentals.AnnualPeriod); ap != "" {
			fmt.Fprintf(&b, "annual_period: %s (fiscal period for the fund.*_yoy fields)\n", ap)
		}
		if lp := strings.TrimSpace(in.Fundamentals.LatestPeriod); lp != "" {
			fmt.Fprintf(&b, "latest_period: %s (fiscal period for the fund.*_yoy_latest / *_qoq / *_latest fields, typically the most recent quarterly or interim filing)\n", lp)
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
			fmt.Fprintf(&b, "latest_announce_date: %s (date of the most recent original filing)\n", ad)
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
			b.WriteString("quality.* = (not computed)\n")
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
			b.WriteString("fund.* = (no financial metrics provided)\n")
		}
		if len(in.Fundamentals.IndustryPeers) > 0 {
			fmt.Fprintf(&b, "industry_peers=%s\n", strings.Join(in.Fundamentals.IndustryPeers, ", "))
		}
		if hist := in.Fundamentals.History; len(hist) > 0 {
			b.WriteString("\n--- history (most recent first) ---\n")
			limit := len(hist)
			maxHist := maxHistoryRowsForMaster(a.persona.Key)
			if limit > maxHist {
				limit = maxHist
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
			b.WriteString("history.10yr = (historical series unavailable; for any criterion that requires multi-year history, write 'data_unavailable:<field>' in key_reasons)\n")
		}
		if rp := in.Fundamentals.RulePrior; rp != nil && len(rp.Items) > 0 {
			b.WriteString("\n--- rule_based_prior (deterministic server-side pre-check; treat these as authoritative, do NOT re-compute multi-year averages) ---\n")
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
			b.WriteString("If any of the entries above has status=FAIL, you MUST surface it explicitly in key_reasons or key_risks and lower the verdict accordingly (multiple FAILs should produce AVOID). For status=UNKNOWN entries, be honest that data is insufficient rather than assuming.\n")
		}
	}
	if in.Technical != nil {
		renderTechnicalBlock(&b, in.Technical, technicalDetailForMaster(a.persona.Key))
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
type technicalDetailLevel int

const (
	technicalDetailCompact technicalDetailLevel = iota
	technicalDetailMedium
	technicalDetailFull
)

func maxHistoryRowsForMaster(masterKey string) int {
	switch strings.ToLower(strings.TrimSpace(masterKey)) {
	case "buffett", "munger", "graham", "greenblatt":
		return 10
	case "lynch", "oneil", "wood":
		return 5
	case "dalio", "marks", "druckenmiller":
		return 3
	default:
		return 5
	}
}

func technicalDetailForMaster(masterKey string) technicalDetailLevel {
	switch strings.ToLower(strings.TrimSpace(masterKey)) {
	case "oneil", "druckenmiller", "wood":
		return technicalDetailFull
	case "lynch", "dalio", "marks":
		return technicalDetailMedium
	default:
		return technicalDetailCompact
	}
}

func renderTechnicalBlock(b *strings.Builder, t *MasterTechnicalBlock, detail technicalDetailLevel) {
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
	if detail == technicalDetailCompact {
		if t.MAAlignment != "" {
			fmt.Fprintf(b, "ma_alignment=%s\n", t.MAAlignment)
		}
		writeTechnicalTags(b, t.Tags, 2)
		return
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
	if detail == technicalDetailMedium {
		if t.ATR14PctOfPx != 0 {
			fmt.Fprintf(b, "atr14_pct_of_price=%.2f%%\n", t.ATR14PctOfPx*100)
		}
		if t.RelativeVolume != 0 {
			fmt.Fprintf(b, "relative_volume=%.2fx (latest / 20-bar SMA)\n", t.RelativeVolume)
		}
		if t.BreakoutState != "" {
			fmt.Fprintf(b, "breakout_state=%s\n", t.BreakoutState)
		}
		writeTechnicalTags(b, t.Tags, 4)
		return
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
	writeTechnicalTags(b, t.Tags, 0)
}

func writeTechnicalTags(b *strings.Builder, tags []string, limit int) {
	if len(tags) == 0 {
		return
	}
	if limit <= 0 || limit > len(tags) {
		limit = len(tags)
	}
	b.WriteString("technical_tags:\n")
	for _, tag := range tags[:limit] {
		fmt.Fprintf(b, "  - %s\n", tag)
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
	var wire struct {
		Verdict        string          `json:"verdict"`
		Confidence     json.RawMessage `json:"confidence"`
		Thesis         string          `json:"thesis"`
		KeyReasons     []string        `json:"key_reasons"`
		KeyRisks       []string        `json:"key_risks"`
		RedLinesHit    []string        `json:"red_lines_hit"`
		MasterSpecific map[string]any  `json:"master_specific"`
	}
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return llmMasterReport{}, err
	}
	confidence, err := parseMasterConfidence(wire.Confidence)
	if err != nil {
		return llmMasterReport{}, err
	}
	out := llmMasterReport{
		Verdict:        wire.Verdict,
		Confidence:     confidence,
		Thesis:         wire.Thesis,
		KeyReasons:     wire.KeyReasons,
		KeyRisks:       wire.KeyRisks,
		RedLinesHit:    wire.RedLinesHit,
		MasterSpecific: wire.MasterSpecific,
	}
	return out, nil
}

func parseMasterConfidence(raw json.RawMessage) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, errors.New("confidence missing")
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int(math.Round(f)), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("confidence invalid: %w", err)
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "%"))
	if s == "" {
		return 0, errors.New("confidence empty string")
	}
	parsed, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("confidence invalid string: %w", err)
	}
	return int(math.Round(parsed)), nil
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
//  1. length > redLineMaxRunes (catches paragraph-long monologues)
//  2. contains a known monologue tell, case-insensitive:
//     EN: "wait,", "i must", "ignore", "monologue", "removing",
//     "json generation", "let me", "actually", "instead"
//     ZH: "此处", "修正为", "等待", "重做", "重置", "我必须", "实际上"
//  3. wrapped in parens AND contains an English imperative ("Ignore
//     previous", "Setting array", "Removing invalid") — the parens
//     are a strong signal the model is talking to itself
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

// megaCapThresholdUSD is the market-cap threshold above which the
// "out-scaled by a competitor" red-line is structurally implausible.
// $500B captures every FAANG-tier name plus current AI majors
// (NVDA, TSM, ...) without false-positive-ing $200-400B large caps
// where the relative-scale argument can still bite.
const megaCapThresholdUSD = 500_000_000_000.0

// megaCapExemptRedLines lists the red-line phrases that should be
// dropped when the subject is itself a mega-cap incumbent. Keep
// this list deliberately small and obvious — a red-line that
// might still apply (e.g. management-quality lines) is left in.
var megaCapExemptRedLines = map[string]struct{}{
	"竞争对手具备同等技术且规模更大": {},
}

// applyMegaCapExemptions drops red-line entries that don't make
// physical sense for mega-cap companies. The market-cap signal
// comes from FundamentalsBlock.Metrics["market_cap"] (USD per the
// upstream provider contract). When the metric is missing the
// list is returned unchanged — we don't want to silently drop
// legitimate hits because a Yahoo quote was throttled.
func applyMegaCapExemptions(hits []string, fund *FundamentalsBlock) []string {
	if len(hits) == 0 || fund == nil || len(fund.Metrics) == 0 {
		return hits
	}
	mcap, ok := fund.Metrics["market_cap"]
	if !ok || mcap < megaCapThresholdUSD {
		return hits
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		if _, drop := megaCapExemptRedLines[strings.TrimSpace(h)]; drop {
			continue
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// whitelistAgainstPersona enforces that each red-line entry is an
// EXACT match (after trim) to one of the persona's canonical
// red_lines. Items outside the whitelist are dropped. This is the
// authoritative gate against LLM hallucination / thinking-mode
// leakage on the red_lines_hit array — the byte-pattern
// sanitizeRedLines stays in front as a cheap pre-filter, but
// whitelisting is what guarantees only known red-line phrases
// reach UI / scanner.
func whitelistAgainstPersona(hits []string, raw map[string]any) []string {
	if len(hits) == 0 {
		return nil
	}
	zhList := stringSliceFromAny(raw["red_lines"])
	if len(zhList) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(zhList))
	for _, s := range zhList {
		allowed[strings.TrimSpace(s)] = struct{}{}
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		key := strings.TrimSpace(h)
		if key == "" || key == "无" {
			continue
		}
		if _, ok := allowed[key]; !ok {
			continue
		}
		out = append(out, key)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// translateRedLinesHit maps each (Chinese) red-line entry to its
// English counterpart using the persona's parallel `red_lines` and
// `red_lines_en` arrays from the JSON config. Returns nil when the
// English list isn't provided or is the wrong length; falls back
// to the original Chinese string for any single entry that doesn't
// hit an exact match in the canonical zh list.
func translateRedLinesHit(hits []string, raw map[string]any) []string {
	if len(hits) == 0 || len(raw) == 0 {
		return nil
	}
	zhList := stringSliceFromAny(raw["red_lines"])
	enList := stringSliceFromAny(raw["red_lines_en"])
	if len(enList) == 0 || len(enList) != len(zhList) {
		return nil
	}
	idxByZh := make(map[string]int, len(zhList))
	for i, s := range zhList {
		idxByZh[strings.TrimSpace(s)] = i
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		key := strings.TrimSpace(h)
		if i, ok := idxByZh[key]; ok && i < len(enList) {
			out = append(out, strings.TrimSpace(enList[i]))
			continue
		}
		// Fallback — keep the original entry so the UI doesn't
		// silently drop a triggered red-line.
		out = append(out, h)
	}
	return out
}

func stringSliceFromAny(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
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
// Limits picked from the compact prompt contract (60-word thesis,
// exactly 3 short reasons, exactly 2 short risks) plus modest
// headroom. Output tokens are the dominant daily-picks cost, so the
// schema intentionally discourages essay-length responses.
var MasterReportJSONSchema = []byte(`{
  "type": "object",
  "properties": {
    "verdict":        { "type": "string", "maxLength": 32 },
    "confidence":     { "type": "integer", "minimum": 0, "maximum": 100 },
    "thesis":         { "type": "string", "maxLength": 420 },
    "key_reasons":    { "type": "array", "minItems": 3, "maxItems": 3, "items": { "type": "string", "maxLength": 180 } },
    "key_risks":      { "type": "array", "minItems": 2, "maxItems": 2, "items": { "type": "string", "maxLength": 180 } },
    "red_lines_hit":  { "type": "array", "maxItems": 10, "items": { "type": "string", "maxLength": 80 } },
    "master_specific":{ "type": "object", "maxProperties": 5 }
  },
  "required": ["verdict", "confidence", "thesis", "key_reasons", "key_risks"],
  "additionalProperties": true
}`)

// ---------------------------------------------------------------------------
// Persona loader (reads internal/agent/masters/*.json once at boot)
// ---------------------------------------------------------------------------

var (
	personaMu    sync.RWMutex
	personaCache map[string]MasterPersona
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
