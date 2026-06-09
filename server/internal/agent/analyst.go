// analyst.go — S8.1 specialised analyst quartet.
//
// Before S8 the system had one ResearcherAgent that toggled its
// behaviour via a Focus enum (stock / fundamental / macro / quant).
// That worked but produced "jack-of-all-trades" reports because one
// LLM prompt had to juggle financials, news, sentiment, and
// technicals at the same time.
//
// S8.1 introduces four first-class analyst roles, each with its
// own tool set, its own prompt, and a uniform structured output
// (AnalystReport). The orchestration layer (workflow.Roundtable,
// the new Bull/Bear researchers in S8.2) consumes the report
// objects directly rather than free-text strings.
//
// Design decisions:
//
//   - One AnalystAgent interface, one AnalystReport struct,
//     four concrete implementations under analyst_*.go.
//   - Specialised inputs come through AnalystInput.<Block> sub-
//     structs. An analyst that doesn't need a block simply ignores
//     it. This keeps the interface signature stable; the wiring
//     layer fills the blocks once and fans out the same input.
//   - Backward compatibility: AnalystReport.ToBrief() converts to
//     ResearchBrief so the dormant RoundtableEngine and any code
//     still wired to ResearcherAgent keep working.
//   - Structured output: S8.1 reuses the existing freeform
//     LLMClient.Complete and parses a JSON-shaped reply. S8.3 will
//     swap the parsing for native function-calling / json_schema.
//   - Persona: every analyst supports a persona snippet, the same
//     way ResearcherAgent does, so operators can give "the value-
//     investing veteran" or "the chartist" personas without code
//     changes.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Categories
// ---------------------------------------------------------------------------

// AnalystCategory enumerates the four specialised roles. Mirrors
// the role taxonomy in TradingAgents but keeps strings consistent
// with the rest of our codebase (snake_case, lowercase).
type AnalystCategory string

const (
	CategoryFundamentals AnalystCategory = "fundamentals"
	CategorySentiment    AnalystCategory = "sentiment"
	CategoryNews         AnalystCategory = "news"
	CategoryTechnical    AnalystCategory = "technical"
)

// AllAnalystCategories is the iteration order used by the UI and
// the default "team template" wiring.
var AllAnalystCategories = []AnalystCategory{
	CategoryFundamentals,
	CategorySentiment,
	CategoryNews,
	CategoryTechnical,
}

// IsValid reports whether c is a recognised category. Used by the
// repo/handler layer to reject unknown strings on the wire.
func (c AnalystCategory) IsValid() bool {
	switch c {
	case CategoryFundamentals, CategorySentiment, CategoryNews, CategoryTechnical:
		return true
	}
	return false
}

// ParseAnalystCategory is the wire-friendly constructor.
func ParseAnalystCategory(s string) (AnalystCategory, bool) {
	c := AnalystCategory(strings.TrimSpace(strings.ToLower(s)))
	if !c.IsValid() {
		return "", false
	}
	return c, true
}

// ---------------------------------------------------------------------------
// Input blocks
// ---------------------------------------------------------------------------

// AnalystInput is the uniform input to every AnalystAgent.Analyze
// call. Each block is optional — analysts ignore blocks they don't
// need. The wiring layer fills the entire input once per symbol and
// fans it out to all four analysts.
type AnalystInput struct {
	Symbol     string
	AssetClass string
	Market     string
	AsOf       time.Time

	// Price / volume block (shared across all four).
	PriceLast   float64
	PriceChange float64
	Volume      float64
	AvgVolume   float64

	// Fundamentals analyst block: derived from internal/quality
	// + optional reported metrics (PE, PB, ROE, …) the wiring
	// layer plumbs in. ZeroValue → fundamentals analyst falls
	// back to a "no data" thesis rather than crashing.
	Fundamentals *FundamentalsBlock

	// Sentiment analyst block: pre-scored news/social items
	// keyed by source ("xueqiu", "stocktwits", "reuters", …)
	// plus the rolling AggregateScore for the symbol. Wiring
	// runs internal/sentiment.Composite first, then plumbs the
	// aggregate here.
	Sentiment *SentimentBlock

	// News analyst block: raw + dedup'd headlines with publish
	// time + source. News differs from Sentiment in that the
	// news analyst looks for narrative catalysts (M&A rumours,
	// earnings preannouncements) rather than crowd mood.
	News *NewsBlock

	// Technical analyst block: quantsnapshot + regime + the
	// list of technical signals the wiring layer has already
	// computed (MACD/RSI/MA cascade/...).
	Technical *TechnicalBlock

	// Freeform extra context for the LLM. Operators use this
	// to inject one-off context ("FOMC tomorrow, expect vol")
	// without changing the schema.
	Notes string
}

// FundamentalsBlock is the typed view of company financials the
// FundamentalsAnalyst consumes. Strings are pre-formatted so the
// prompt doesn't need to know how to render NUMERIC columns.
type FundamentalsBlock struct {
	// Name is the issuer's short / human-readable name (e.g.
	// "德科立", "Apple Inc."). Populated by loaders that can
	// resolve it cheaply (akshare sidecar, Yahoo quoteSummary).
	// Empty when the provider doesn't know — callers must not
	// assume non-empty.
	Name string
	// QualityScore is the cross-sectional composite z-score
	// from internal/quality. nil when the symbol isn't in the
	// scored universe.
	QualityScore *QualityScoreLite
	// Reported metrics keyed by canonical name (pe, pb, roe,
	// debt_to_equity, revenue_growth_yoy, …). Wiring layer
	// owns the canonical name list. Values are raw floats; the
	// prompt formats them.
	Metrics map[string]float64
	// AnnualPeriod / LatestPeriod label the fiscal periods the
	// numbers above were drawn from (e.g. "2025-12-31" annual,
	// "2026-03-31" Q1). Optional — providers that don't expose
	// period metadata leave them empty.
	//
	// Surfacing both lets the persona reason about acceleration
	// vs decay between the last annual and the freshest interim:
	// e.g. annual rev_growth=+11% but rev_growth_latest=+28% at
	// "2026-03-31" tells Wood the S-curve just inflected, not
	// the simple "growth too slow" reading of the annual alone.
	AnnualPeriod string
	LatestPeriod string
	// ListingDate is the company's exchange listing date in
	// "YYYY-MM-DD" form (e.g. "2022-08-09"). Together with
	// ListingYears, lets master personas distinguish a
	// "次新股 N年" from a missing-data condition — without
	// this, a 2022-listed STAR-market stock gets mechanically
	// flagged "history.10yr data_unavailable" by 10-year-
	// horizon personas (Buffett, Graham). Optional; empty
	// when the provider didn't resolve it.
	ListingDate  string
	ListingYears float64
	// LatestAnnounceDate is the YYYY-MM-DD of the filing the
	// *_latest metrics were drawn from (e.g. "2026-04-28" for
	// the Q1 2026 业绩快报). Surfaced to the prompt as an
	// anchor so the LLM (rule 8 in master_agent.go) can cite a
	// verifiable announcement date when quoting any *_latest
	// figure — and so any reviewer can pull the original
	// announcement off the exchange. Empty when the provider
	// didn't supply it.
	LatestAnnounceDate string
	// LatestSource is the provenance tag (e.g. "eastmoney_yjbb")
	// for the *_latest fields. Surfaced verbatim so the prompt
	// can name the data source in citations.
	LatestSource string
	// IndustryPeers carries up to ~5 peer symbols so the
	// analyst can frame relative valuation. Wiring optional.
	IndustryPeers []string
	// FilingsURL is the latest 10-K / annual report URL if
	// available. Empty → no citation in the report.
	FilingsURL string
	// History is the multi-year financial series. The slice
	// is ordered most-recent first; element [0] is the latest
	// complete fiscal year. nil when the provider doesn't
	// expose history for the requested market. Consumed by
	// the /advisor master agents (Buffett / Lynch / Graham /
	// O'Neil) which need 5-10y ROE, FCF, EPS series to
	// validate their must_have_criteria. Empty slice = no
	// history available.
	History []YearlyMetricsLite
	// RulePrior is the deterministic, Go-computed evaluation of
	// each must_have_criteria that the persona declares. The
	// wiring layer computes these on the cleaned history /
	// snapshot before the LLM ever sees the prompt — that way
	// the LLM can't misread a 10-year ROE average. nil when
	// no rules were evaluable (no history, no snapshot, etc.).
	RulePrior *RulePriorBlock
}

// MasterTechnicalBlock is the typed view of price-action /
// momentum / volatility data the MASTER agents consume in their
// prompt (and that the daily-picks publisher persists in
// result_json for the detail UI). Distinct from TechnicalBlock
// (which carries the AnalystInput shape for the technical
// AnalystAgent) — the two serve different surfaces:
//
//   - TechnicalBlock: AnalystInput → TechnicalAnalyst, structured
//     for the analyst's per-signal hard-vote map. Internal.
//   - MasterTechnicalBlock: MasterInput → MasterAgent prompt and
//     ConsultResponse / daily_picks.result_json. Serialisation +
//     prompt shape.
//
// Populated by the wiring layer from indicator.Snapshot — the
// agent package intentionally does NOT import internal/indicator
// to keep the layering one-way (the wiring layer composes
// indicator → agent, never the reverse).
//
// JSON tags: snake_case because this struct is serialised
// verbatim into daily_picks.result_json under a "technical" key
// and rendered directly by the React frontend (which expects
// snake_case throughout). The fields here are deliberately a
// SUBSET of indicator.Snapshot — only what's relevant for
// prompt context and UI rendering. Internal computation flags
// like BBPctPosition stay in indicator.Snapshot.
//
// Compliance:
//
//	This struct carries raw market data only (close, volumes,
//	indicator values, S/R levels). It contains zero recommendations.
//	The master_agent prompt rule 9 forbids the LLM from turning
//	any of these numbers into a price target or trade signal —
//	the model can quote them but cannot project them.
type MasterTechnicalBlock struct {
	// AsOf is the timestamp of the most recent bar used in the
	// snapshot computation (UTC, RFC3339 format). Empty when
	// computation degraded to fallback (no bars).
	AsOf string `json:"asof,omitempty"`

	// BarsUsed is the number of OHLC bars that fed the computation.
	// <60 means several derived indicators (SMA200, etc.) may be
	// zero — see indicator.MinBarsForFullSnapshot.
	BarsUsed int `json:"bars_used,omitempty"`

	// Latest close + short-window returns. PctChangeNd is the
	// percentage change between the latest close and the close N
	// bars ago, expressed as a decimal (0.05 = +5%). Zero when
	// not enough history is available.
	LastClose      float64 `json:"last_close,omitempty"`
	PctChange1D    float64 `json:"pct_change_1d,omitempty"`
	PctChange5D    float64 `json:"pct_change_5d,omitempty"`
	PctChange20D   float64 `json:"pct_change_20d,omitempty"`
	PctChange52WHi float64 `json:"pct_change_from_52w_high,omitempty"` // negative when price is below 52w high

	// Moving averages — all in raw price units.
	SMA20  float64 `json:"sma20,omitempty"`
	SMA50  float64 `json:"sma50,omitempty"`
	SMA200 float64 `json:"sma200,omitempty"`
	// MAAlignment is the algorithmic classification of the moving
	// average stack: "bullish" when SMA20>SMA50>SMA200, "bearish"
	// when reversed, "mixed" otherwise. Empty when not enough
	// bars to compute SMA200.
	MAAlignment string `json:"ma_alignment,omitempty"`

	// Momentum & volatility.
	RSI14        float64 `json:"rsi14,omitempty"`
	RSI14Zone    string  `json:"rsi14_zone,omitempty"` // "overbought" | "oversold" | "neutral"
	MACDLine     float64 `json:"macd_line,omitempty"`
	MACDSignal   float64 `json:"macd_signal,omitempty"`
	MACDHist     float64 `json:"macd_hist,omitempty"`
	MACDCross    string  `json:"macd_cross,omitempty"` // "bullish" | "bearish" | ""
	ATR14PctOfPx float64 `json:"atr14_pct_of_price,omitempty"`

	// KDJ — the de-facto-standard Chinese broker indicator.
	// J > 90 → "hot"; J < 10 → "cool" (informational only,
	// not actionable).
	KDJK float64 `json:"kdj_k,omitempty"`
	KDJD float64 `json:"kdj_d,omitempty"`
	KDJJ float64 `json:"kdj_j,omitempty"`

	// Volume.
	Volume         float64 `json:"volume,omitempty"`
	RelativeVolume float64 `json:"relative_volume,omitempty"` // latest / 20-bar SMA

	// Support / resistance band over the prior 20 bars (the
	// current bar is EXCLUDED so the breakout classification
	// makes sense). Both zero when fewer than 21 bars exist.
	Support       float64 `json:"support,omitempty"`
	Resistance    float64 `json:"resistance,omitempty"`
	SRWindow      int     `json:"sr_window,omitempty"`
	BreakoutState string  `json:"breakout_state,omitempty"` // "above_resistance" | "below_support" | "near_resistance" | "near_support" | ""

	// Tags is the prompt-friendly bullet list the wiring layer
	// derives from the snapshot — e.g. "RSI14: 76.3 (overbought)",
	// "MACD: bullish cross at latest bar". The master prompt
	// renders these directly; the frontend can use the same list
	// as readable bullet items so the UI doesn't have to know
	// the formatting rules.
	Tags []string `json:"tags,omitempty"`
}

// YearlyMetricsLite mirrors fundamental.YearlyMetrics one-to-one
// but lives in the agent package so analyst / master code doesn't
// have to import the upstream fetcher package. The wiring layer
// copies fields across at request build time.
type YearlyMetricsLite struct {
	Year              int
	ReturnOnEquity    float64
	ReturnOnCapital   float64
	GrossMargin       float64
	OperatingMargin   float64
	ProfitMargin      float64
	FreeCashFlow      float64
	EPS               float64
	BookValuePerShare float64
	DividendPerShare  float64
	CurrentRatio      float64
	DebtToEquity      float64
	RevenueGrowthYoY  float64
	EarningsGrowthYoY float64
}

// RulePriorBlock is the precomputed verdict on each persona
// must_have_criteria. The advisor wiring layer builds one of these
// per (persona, symbol) at request time and hands it to the
// MasterAgent so the prompt can pin hard rules.
type RulePriorBlock struct {
	// Persona is the persona key the prior was computed for
	// (e.g. "buffett"). Helps debug when the wiring layer
	// reuses one block across multiple personas.
	Persona string
	// Items lists every criterion, in deterministic order, with
	// the threshold, the observed value, and PASS/FAIL/UNKNOWN.
	Items []RulePriorItem
	// Notes carries free-form caveats the wiring layer wants
	// to surface to the LLM (e.g. "ROE history only spans 4
	// years; computed average over 4y not 10y").
	Notes []string
}

// RulePriorItem is one criterion evaluation.
type RulePriorItem struct {
	Key       string  // e.g. "ROE_10yr_avg"
	Required  string  // e.g. ">= 15%"
	Observed  string  // e.g. "12.4% (4yr avg)"
	Status    string  // PASS / FAIL / UNKNOWN
	Detail    string  // optional human-readable detail
	ValueLow  float64 // raw value (low end for range checks)
	ValueHigh float64 // raw value (high end for range checks)
}

// QualityScoreLite mirrors internal/quality.Score's prompt-facing
// fields. We don't import the quality package here so analyst.go
// stays in the domain layer with no upward deps.
type QualityScoreLite struct {
	ProfitabilityZ float64
	GrowthZ        float64
	SafetyZ        float64
	CompositeZ     float64
	Quartile       int
}

// SentimentBlock carries pre-scored social/news items + the
// aggregate the sentiment analyst reasons over.
type SentimentBlock struct {
	// Aggregate is the symbol-scoped aggregate from
	// internal/sentiment.Aggregator. ZeroValue → analyst
	// reports neutral with low confidence.
	Aggregate SentimentAggregateLite
	// RecentItems is up to ~10 most-recent scored items the
	// analyst can quote in its thesis. Each item is already
	// scored so the analyst doesn't re-run the scorer.
	RecentItems []SentimentItemLite
	// SourceBreakdown counts items per source. Useful for the
	// analyst to flag "all bullish but only from xueqiu, no
	// reuters / bloomberg coverage" caveats.
	SourceBreakdown map[string]int
}

// SentimentAggregateLite mirrors internal/sentiment.AggregateScore.
type SentimentAggregateLite struct {
	Average  float64
	Count    int
	Polarity string // "bullish", "bearish", "neutral", "strongly bullish", "strongly bearish"
}

// SentimentItemLite is one scored news / social item.
type SentimentItemLite struct {
	Title       string
	Source      string
	Score       float64 // [-1, 1]
	PublishedAt time.Time
	URL         string
}

// NewsBlock is the raw headline feed the news analyst dissects
// for narrative catalysts.
type NewsBlock struct {
	Headlines []NewsHeadline
	// MaterialEventTags lists pre-detected event tags ("earnings",
	// "m_and_a", "regulator_action", "downgrade", …) the wiring
	// layer extracted via keyword spotting. The analyst uses
	// these as priority hints.
	MaterialEventTags []string
}

// NewsHeadline is a single headline with provenance.
type NewsHeadline struct {
	Title       string
	Source      string
	Summary     string
	PublishedAt time.Time
	URL         string
	Language    string
}

// TechnicalBlock carries the quant snapshot + signals the
// technical analyst reasons over.
type TechnicalBlock struct {
	Snapshot QuantSnapshotLite
	// Signals is the per-indicator value map ("rsi14", "macd_hist",
	// "ma50_over_ma200", …). The analyst doesn't recompute; the
	// wiring layer fills this from internal/quantsnapshot +
	// internal/regime.
	Signals map[string]float64
	// PriceHistorySpark is the last ~30 closing prices (oldest
	// first) so the analyst can describe the shape without
	// inventing numbers. nil → omitted from prompt.
	PriceHistorySpark []float64
}

// QuantSnapshotLite mirrors internal/quantsnapshot.Snapshot's
// prompt-facing fields.
type QuantSnapshotLite struct {
	Regime                 string
	Close                  float64
	ATR14                  float64
	ATRPct                 float64
	PositionSizeCeilingPct float64
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

// AnalystReport is the structured output every AnalystAgent
// produces. It is intentionally narrower than ResearchBrief:
// price targets and horizon defaults live downstream (the
// Bull/Bear researcher in S8.2 sets them based on combined
// analyst input).
type AnalystReport struct {
	ID          string            `json:"id"`
	AgentID     string            `json:"agent_id"`
	AgentName   string            `json:"agent_name"`
	Category    AnalystCategory   `json:"category"`
	Symbol      string            `json:"symbol"`
	AsOf        time.Time         `json:"asof"`
	GeneratedAt time.Time         `json:"generated_at"`

	// Direction reuses ResearchDirection from researcher.go so
	// downstream code doesn't deal with two enums.
	Direction Direction `json:"direction"`

	// Confidence is the analyst's calibrated 0–100 conviction in
	// the directional call. Used both by the Bull/Bear weighting
	// in S8.2 and by the reputation calibration in S8.4.
	Confidence int `json:"confidence"`

	// Thesis is a one-paragraph narrative of the call. Required.
	Thesis string `json:"thesis"`

	// KeyFindings are 1–5 bullet-style observations. Required at
	// least one.
	KeyFindings []string `json:"key_findings"`

	// Risks are 0–5 named risk factors. Empty list means "no
	// material risks identified" — distinct from nil.
	Risks []string `json:"risks"`

	// DataPoints are typed observations the analyst cited. Reused
	// from researcher.DataPoint so the prompt-render code in
	// pm.go can stay the same.
	DataPoints []DataPoint `json:"data_points,omitempty"`

	// Sources are URLs / citations supporting the report.
	Sources []string `json:"sources,omitempty"`

	// PromptTokens / CompletionTokens are populated when the LLM
	// client returned usage metadata. Zero when unknown / unset.
	// Used by the reputation accumulator and the budget meter.
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`

	// LLMModel is the model id that produced the thesis, or
	// "fallback" when the deterministic path ran.
	LLMModel string `json:"llm_model,omitempty"`
}

// Direction is an alias for the existing researcher Direction
// enum. Defined here so analyst.go doesn't force every consumer
// to know about researcher.go.
type Direction = ResearchDirection

// Validate enforces the must-have fields a report needs to be
// safely persisted and consumed by Bull/Bear.
func (r AnalystReport) Validate() error {
	if strings.TrimSpace(r.Symbol) == "" {
		return errors.New("analyst: report.Symbol required")
	}
	if !r.Category.IsValid() {
		return fmt.Errorf("analyst: report.Category %q invalid", r.Category)
	}
	switch r.Direction {
	case DirectionBullish, DirectionBearish, DirectionNeutral:
	default:
		return fmt.Errorf("analyst: report.Direction %q invalid", r.Direction)
	}
	if r.Confidence < 0 || r.Confidence > 100 {
		return fmt.Errorf("analyst: report.Confidence %d out of [0,100]", r.Confidence)
	}
	if strings.TrimSpace(r.Thesis) == "" {
		return errors.New("analyst: report.Thesis required")
	}
	if len(r.KeyFindings) == 0 {
		return errors.New("analyst: report.KeyFindings must have at least one entry")
	}
	return nil
}

// ToBrief adapts a report to the legacy ResearchBrief shape so
// the dormant RoundtableEngine + any test code wired to
// ResearcherAgent keeps working unchanged. Mapping:
//
//	Category → Focus (fundamentals → fundamental, etc.)
//	Thesis   → Thesis (verbatim)
//	KeyFindings → Catalysts
//	Risks    → Risks
//	DataPoints / Sources → carried verbatim
//
// Price target / horizon are not set: they belong to the
// downstream Bull/Bear researcher's role, not an individual
// analyst's.
func (r AnalystReport) ToBrief() ResearchBrief {
	var focus ResearchFocus
	switch r.Category {
	case CategoryFundamentals:
		focus = FocusFundamental
	case CategoryTechnical:
		focus = FocusQuant
	case CategoryNews, CategorySentiment:
		focus = FocusStock
	}
	return ResearchBrief{
		ID:          r.ID,
		AgentID:     r.AgentID,
		AgentName:   r.AgentName,
		Focus:       focus,
		Symbol:      r.Symbol,
		Direction:   r.Direction,
		Confidence:  r.Confidence,
		Thesis:      r.Thesis,
		Catalysts:   append([]string(nil), r.KeyFindings...),
		Risks:       append([]string(nil), r.Risks...),
		DataPoints:  append([]DataPoint(nil), r.DataPoints...),
		Sources:     append([]string(nil), r.Sources...),
		GeneratedAt: r.GeneratedAt,
	}
}

// ---------------------------------------------------------------------------
// Interface
// ---------------------------------------------------------------------------

// AnalystAgent is the first-class S8 agent interface. Each
// specialised analyst implements Analyze for the symbol carried
// in input.Symbol and returns one AnalystReport.
type AnalystAgent interface {
	ID() string
	Name() string
	Category() AnalystCategory
	Persona() string
	Analyze(ctx context.Context, input AnalystInput) (AnalystReport, error)
}

// AnalystOption is the constructor option shape shared by all
// four implementations. Concrete analysts add their own option
// constructors in analyst_*.go files but go through this struct
// to set common knobs (logger, clock, persona).
type AnalystOption func(*analystBase)

// analystBase holds the fields every analyst shares. Concrete
// types embed it; ID/Name/Category/Persona are served from here.
type analystBase struct {
	mu      sync.RWMutex
	id      string
	name    string
	persona string
	fundID  string
	llm     LLMClient
	logger  *slog.Logger
	now     func() time.Time
}

// ID returns the analyst's identifier.
func (a *analystBase) ID() string { return a.id }

// Name returns the analyst's display name.
func (a *analystBase) Name() string { return a.name }

// Persona returns the persona snippet appended to the system prompt.
func (a *analystBase) Persona() string { return a.persona }

// WithAnalystLogger overrides the default logger.
func WithAnalystLogger(l *slog.Logger) AnalystOption {
	return func(b *analystBase) {
		if l != nil {
			b.logger = l
		}
	}
}

// WithAnalystPersona sets an analyst persona snippet. Same
// convention as WithResearcherPersona.
func WithAnalystPersona(p string) AnalystOption {
	return func(b *analystBase) {
		b.persona = strings.TrimSpace(p)
	}
}

// WithAnalystClock injects a deterministic clock for tests.
func WithAnalystClock(now func() time.Time) AnalystOption {
	return func(b *analystBase) {
		if now != nil {
			b.now = now
		}
	}
}

// newAnalystBase wires the shared fields. Concrete constructors
// call this before applying their own options.
func newAnalystBase(id, name, fundID string, llm LLMClient, opts ...AnalystOption) *analystBase {
	b := &analystBase{
		id:     id,
		name:   name,
		fundID: fundID,
		llm:    llm,
		logger: slog.Default(),
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// ---------------------------------------------------------------------------
// Structured-output helpers (S8.1 transitional)
// ---------------------------------------------------------------------------

// llmJSONReport is the wire shape the analyst prompts ask the
// LLM to return. S8.3 will replace the manual JSON parsing with
// native function-calling / response_format=json_schema, but
// keeping the shape here lets us swap implementations later
// without changing prompt wording or the parse-result contract.
type llmJSONReport struct {
	Direction   string   `json:"direction"`
	Confidence  int      `json:"confidence"`
	Thesis      string   `json:"thesis"`
	KeyFindings []string `json:"key_findings"`
	Risks       []string `json:"risks"`
}

// callLLMForReport asks the LLM to return a JSON envelope and
// parses it tolerantly: we accept markdown fences, leading prose,
// and trailing commentary. Errors are bubbled up so the caller
// can fall back to the deterministic path.
//
// S8.3: when the underlying LLM client implements
// SchemaLLMClient (i.e. supports native structured output),
// we route through CompleteWithSchema with the canonical
// AnalystReportJSONSchema so providers like OpenAI / Gemini
// produce strict, schema-conformant JSON. The tolerant
// post-parse stays in place so a non-strict provider's
// freeform JSON still round-trips.
func (b *analystBase) callLLMForReport(ctx context.Context, sys, user string) (llmJSONReport, error) {
	if b.llm == nil {
		return llmJSONReport{}, errors.New("analyst: no LLM configured")
	}
	var (
		raw string
		err error
	)
	if schemaClient, ok := b.llm.(SchemaLLMClient); ok {
		raw, err = schemaClient.CompleteWithSchema(ctx, sys, user, AnalystReportJSONSchema)
	} else {
		raw, err = b.llm.Complete(ctx, sys, user)
	}
	if err != nil {
		return llmJSONReport{}, fmt.Errorf("analyst: llm call: %w", err)
	}
	out, perr := parseLLMJSONReport(raw)
	if perr != nil {
		return llmJSONReport{}, fmt.Errorf("analyst: parse llm json: %w", perr)
	}
	return out, nil
}

// parseLLMJSONReport extracts a JSON object from the LLM reply
// even when wrapped in ```json fences or surrounded by chat
// preamble. The contract is forgiving but strict on the shape:
// once we have a JSON object the field types must match.
func parseLLMJSONReport(raw string) (llmJSONReport, error) {
	body := strings.TrimSpace(raw)
	if body == "" {
		return llmJSONReport{}, errors.New("empty llm reply")
	}
	// Strip ``` fences (with or without `json` tag).
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
	// Take the substring between the first `{` and last `}`.
	start := strings.Index(body, "{")
	end := strings.LastIndex(body, "}")
	if start < 0 || end < 0 || end <= start {
		return llmJSONReport{}, errors.New("no json object found")
	}
	body = body[start : end+1]
	var out llmJSONReport
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return llmJSONReport{}, err
	}
	return out, nil
}

// normaliseDirection coerces an LLM's free-form direction string
// into the canonical Direction enum. Unknown values map to
// DirectionNeutral so we never block on a bad LLM token.
func normaliseDirection(s string) Direction {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "bull", "bullish", "buy", "long", "positive", "up":
		return DirectionBullish
	case "bear", "bearish", "sell", "short", "negative", "down":
		return DirectionBearish
	default:
		return DirectionNeutral
	}
}

// clampConfidence enforces the 0..100 range and applies the
// same 20-floor researcher.go uses. A flat market means low
// conviction, not zero.
func clampConfidence(c int) int {
	if c > 100 {
		return 100
	}
	if c < 20 {
		return 20
	}
	return c
}

// mergeDirectionWithRule keeps the deterministic direction
// derived from numbers when the LLM disagrees by more than one
// step. If the LLM says bullish and the rule says bearish we
// trust the rule (numbers are anchors). When they merely differ
// in conviction the LLM thesis wins.
func mergeDirectionWithRule(rule, llm Direction) Direction {
	if rule == llm || llm == DirectionNeutral || rule == DirectionNeutral {
		return rule
	}
	// rule != llm and neither is neutral → conflict; rule wins.
	return rule
}
