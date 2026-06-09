// service.go — request-time orchestrator for /advisor consultations.
//
// Wires PersonaPreset → MasterPanel / TacticPanel, runs the panel,
// persists the result via Repo, and returns the structured response
// the HTTP handler renders. Tactic panel injection lands in Phase 4
// — Phase 1 ships master-panel routing + a typed "tactics not yet
// available" branch for `cn_short` preset.
//
// Dependency injection follows the existing AnalystPanelProvider
// pattern in server/cmd/server/analyst_panel_handler.go: the
// wiring layer hands the service two factories (PanelBuilder /
// FundamentalsLoader) instead of concrete types so the test
// harness can swap in fakes and the production wiring can wire
// different LLM credentials per persona without coupling.

package advisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/agent"
	"github.com/fundai/server/internal/compliance"
)

// jsonMarshal exists as a one-line wrapper so we can swap encoding
// (e.g. to jsoniter) in one place if profiling ever flags it. The
// inline json.Marshal call is exactly the same shape — the wrapper
// is purely a future-proofing seam, not a code path of its own.
func jsonMarshal(v interface{}) ([]byte, error) { return json.Marshal(v) }

// ErrNotReady is returned when Consult is called but the relevant
// panel builder (master / tactic) hasn't been injected yet. Maps
// to HTTP 503 in the handler.
var ErrNotReady = errors.New("advisor: consult service not ready")

// ErrUnsupportedPreset is returned when the preset's agent
// population requires a panel kind that hasn't been wired (e.g.
// `cn_short` in Phase 1 before TacticPanelBuilder lands).
var ErrUnsupportedPreset = errors.New("advisor: preset not supported in this build")

// ConsultRequest is the input to Service.Consult.
//
// CustomMasterKeys / CustomTacticKeys are honoured ONLY when
// PresetKey resolves to a preset whose Kind() == PresetKindEmpty
// (the `custom` row). For every other preset the service uses the
// preset's stored lists verbatim — this prevents a client from
// quietly running a non-conservative master through the
// "conservative" preset and biasing track-record numbers.
type ConsultRequest struct {
	UserID            string
	Symbol            string
	Market            string
	AssetClass        string
	PresetKey         string
	CustomMasterKeys  []string
	CustomTacticKeys  []string
	Notes             string

	// PriceLast / PriceChange are optional — caller supplies them
	// if available so the master prompts can quote a live price.
	PriceLast   float64
	PriceChange float64
	Currency    string
}

// ConsultResponse is the result of a successful consultation.
//
// JSON tags use snake_case because this struct is serialised
// verbatim into daily_picks.result_json (publisher mode) AND the
// /api/daily-picks/{date}/{symbol} detail endpoint serves that
// JSON blob directly to the React frontend, which expects
// snake_case (matches the manual wire types in web/src/lib/api.ts).
// The per-user /api/advisor/consult handler projects through its
// own wire struct (advisor_handler.go advisorConsultResponse)
// so adding json tags here does NOT affect that surface.
type ConsultResponse struct {
	ConsultationID string `json:"consultation_id"`
	Symbol         string `json:"symbol"`
	// SymbolName is the issuer's short Chinese / English name
	// (e.g. "德科立"). Empty when the upstream provider doesn't
	// resolve it. Frontends render "德科立 (688205)" when set,
	// fall back to bare "688205" otherwise.
	SymbolName          string            `json:"symbol_name,omitempty"`
	PresetKey           string            `json:"preset_key"`
	AggregateVerdict    string            `json:"aggregate_verdict"`
	AggregateConfidence int               `json:"aggregate_confidence"`
	ConsensusScore      float64           `json:"consensus_score"`
	MasterReports       []MasterReportRow `json:"master_reports"`
	TacticReports       []TacticReportRow `json:"tactic_reports"`
	// Technical is the price-action / momentum / volatility
	// snapshot that fed the master panel. Persisted into
	// daily_picks.result_json so the frontend detail modal can
	// render the same table (RSI / MACD / KDJ / S・R / breakout)
	// without recomputing from OHLC. nil-omitted via omitempty
	// when the OHLC fetcher couldn't reach the symbol or the
	// wiring layer didn't provide a loader.
	//
	// Compliance: this struct is FACT (closed prices, indicator
	// values, breakout state). No projections. master_agent
	// rule 9 forbids the LLM from spinning these into price
	// targets; the publisher-mode phrase scanner is the second
	// line of defence.
	Technical *agent.MasterTechnicalBlock `json:"technical,omitempty"`
	CreatedAt time.Time                   `json:"created_at"`
}

// MasterReportRow is the read-shape returned for one master agent.
// Mirrors the columns in advisor_master_reports and the in-memory
// agent.MasterReport struct.
//
// JSON tags: see ConsultResponse comment — snake_case is the
// wire shape stored in daily_picks.result_json. The per-user
// surface projects through its own wire shape, unaffected.
type MasterReportRow struct {
	MasterKey    string `json:"master_key"`
	MasterNameZh string `json:"master_name_zh,omitempty"`
	MasterNameEn string `json:"master_name_en,omitempty"`
	// SymbolName mirrors ConsultResponse.SymbolName so consumers
	// rendering a single row (e.g. a notification card) don't need
	// the parent consultation to render "德科立 (688205)". Optional.
	SymbolName     string         `json:"symbol_name,omitempty"`
	Verdict        string         `json:"verdict"`
	Confidence     int            `json:"confidence"`
	Thesis         string         `json:"thesis"`
	KeyReasons     []string       `json:"key_reasons,omitempty"`
	KeyRisks       []string       `json:"key_risks,omitempty"`
	MasterSpecific map[string]any `json:"master_specific,omitempty"`
	RedLinesHit    []string       `json:"red_lines_hit,omitempty"`
	LLMModel       string         `json:"llm_model,omitempty"`
	GeneratedAt    time.Time      `json:"generated_at"`
}

// TacticReportRow is the read-shape for one A-share tactic agent.
// Phase 4 fills these fields; Phase 1 only allocates the type so
// the handler / front-end can render the column as empty.
//
// JSON tags: see ConsultResponse comment.
type TacticReportRow struct {
	TacticKey    string `json:"tactic_key"`
	TacticNameZh string `json:"tactic_name_zh,omitempty"`
	TacticNameEn string `json:"tactic_name_en,omitempty"`
	// SymbolName — see MasterReportRow.SymbolName.
	SymbolName          string    `json:"symbol_name,omitempty"`
	Verdict             string    `json:"verdict"`
	Confidence          int       `json:"confidence"`
	Thesis              string    `json:"thesis"`
	EntryPriceLow       *float64  `json:"entry_price_low,omitempty"`
	EntryPriceHigh      *float64  `json:"entry_price_high,omitempty"`
	StopLossPrice       *float64  `json:"stop_loss_price,omitempty"`
	TargetT1            *float64  `json:"target_t1,omitempty"`
	TargetT3            *float64  `json:"target_t3,omitempty"`
	ExpectedHoldingDays *int      `json:"expected_holding_days,omitempty"`
	Score               float64   `json:"score"`
	KeyReasons          []string  `json:"key_reasons,omitempty"`
	KeyRisks            []string  `json:"key_risks,omitempty"`
	RedLinesHit         []string  `json:"red_lines_hit,omitempty"`
	MarketRegimePass    bool      `json:"market_regime_pass"`
	MarketRegimeReason  string    `json:"market_regime_reason,omitempty"`
	GeneratedAt         time.Time `json:"generated_at"`
}

// FundamentalsLoader is the per-symbol data fetcher the master
// panel uses. Implemented by the wiring layer over fundamental.Service
// so this package doesn't import the data-fetch chain directly.
//
// Returning nil + nil error means "no data available, prompt the
// LLM with an empty block and let the persona handle data_unavailable".
type FundamentalsLoader func(ctx context.Context, symbol, market, assetClass string) (*agent.FundamentalsBlock, error)

// TechnicalLoader is the per-symbol OHLC → indicator.Snapshot
// fetcher the master panel uses for the prompt's
// "--- technical snapshot ---" section. Implemented by the wiring
// layer over ohlc.Fetcher + indicator.Compute so this package
// stays import-clean (no ohlc / indicator dependencies leak in).
//
// Returning nil + nil error means "no bars available, prompt the
// LLM without a technical section" (rule 9 in master_agent.go
// already covers the missing-block case). Returning an error
// triggers a slog.Warn but does NOT fail the consultation —
// fundamental data alone is still a valid basis for a master
// verdict, and a Yahoo throttle should not block /advisor.
type TechnicalLoader func(ctx context.Context, symbol, market, assetClass string) (*agent.MasterTechnicalBlock, error)

// MasterPanelBuilder is the factory the service calls to materialise
// a MasterPanel for a given list of master keys. The wiring layer
// owns LLM-client selection and persona injection — the service
// just hands over the requested keys.
//
// userID is threaded through (Phase B-3) so the wiring layer can
// stamp ChatRequest.UserID on every model call and the
// llm.UserOverrideHook can pick up the user's BYOK key. Empty
// userID disables BYOK routing transparently (the hook falls
// through to fund / platform defaults).
//
// Implementations should be cheap to call (panels are constructed
// once per consultation; LLM clients are reused).
type MasterPanelBuilder func(ctx context.Context, userID string, masterKeys []string) (*agent.MasterPanel, error)

// TacticPanelBuilder is the equivalent for A-share tactics. Phase 1
// leaves this nil so cn_short presets return ErrUnsupportedPreset.
//
// userID is threaded through for the same BYOK-routing reasons
// described on MasterPanelBuilder.
type TacticPanelBuilder func(ctx context.Context, userID string, tacticKeys []string) (*agent.TacticPanel, error)

// TacticDataLoader fetches the per-symbol structural data the tactic
// agents need: intraday snapshot + market regime + sector ranking +
// any hard-risk veto reasons. The loader populates the supplied
// TacticInput in place and returns it; the wiring layer owns the
// upstream provider (cnmarketstructure + risk packages).
//
// Returning a partial input is fine — missing fields cause the
// per-tactic agent to SKIP with data_unavailable rather than the
// whole consultation failing.
type TacticDataLoader func(ctx context.Context, in agent.TacticInput) (agent.TacticInput, error)

// PhraseViolationSink lets the service report compliance.Scan
// violations to whoever wants them (Postgres audit table, slog,
// no-op in tests). Always called best-effort; failure must not
// fail the consultation.
type PhraseViolationSink func(ctx context.Context, userID, surface, sourceEntity, sourceID string, redacted string, violations []compliance.Violation)

// Service is the public façade.
type Service struct {
	repo             *Repo
	presets          PresetLookup
	loadFundamental  FundamentalsLoader
	loadTechnical    TechnicalLoader
	loadTacticData   TacticDataLoader
	buildMasterPanel MasterPanelBuilder
	buildTacticPanel TacticPanelBuilder

	// complianceMode determines whether the per-master LLM
	// outputs get run through the phrase scanner before being
	// returned to the user. Defaults to ModePublisher (the
	// strictest); the wiring layer can call WithComplianceMode
	// to override once Form ADV is filed.
	complianceMode compliance.Mode
	violationSink  PhraseViolationSink

	// picksRepo (optional) is the shared-cache publisher repo.
	// Set via WithPicksRepo by the wiring layer. When nil,
	// PublishConsult returns an error — keeps the per-user code
	// path completely unaffected by the publisher surface so a
	// degraded boot without the picks tables (e.g. running an
	// older migration) doesn't break /advisor.
	picksRepo PicksRepoIface

	clock func() time.Time
}

// PicksRepoIface is the abstract publisher cache the service
// writes to in publisher mode. Declared here as a narrow interface
// (rather than depending on the concrete dailypicks.Repo) so this
// package doesn't take a build-time import on dailypicks — which
// would create an awkward import cycle once the daily-picks loop
// reaches in for advisor.Service. The wiring layer satisfies this
// with a thin adapter over dailypicks.Repo.
type PicksRepoIface interface {
	UpsertPick(ctx context.Context, in PicksUpsertInput) (int64, error)
}

// PicksUpsertInput is the wire-shape the service passes to the
// publisher repo. Mirrors dailypicks.SaveInput field-for-field;
// the wiring layer maps the two structurally.
type PicksUpsertInput struct {
	Symbol           string
	SymbolName       string
	Market           string
	PresetKey        string
	PickDate         time.Time
	ResultJSON       []byte
	AggregateVerdict string
	AggregateScore   int
	Consensus        float64
	LLMCostUSD       float64
	ErrorReason      string
}

// ServiceOption configures construction.
type ServiceOption func(*Service)

// WithFundamentalsLoader injects the data-fetcher. Without one the
// service still works but master prompts get an empty fundamentals
// block (persona has to mark data_unavailable on every check).
func WithFundamentalsLoader(f FundamentalsLoader) ServiceOption {
	return func(s *Service) {
		if f != nil {
			s.loadFundamental = f
		}
	}
}

// WithTechnicalLoader injects the OHLC → indicator.Snapshot
// fetcher. Without one the master prompts simply omit the
// "--- technical snapshot ---" section (rule 9 in
// master_agent.go covers the missing-block case). Wiring layer
// is expected to pass a cached fetcher so 50 symbols × 4 presets
// in one cron tick don't fan out into 200 Yahoo chart requests.
func WithTechnicalLoader(f TechnicalLoader) ServiceOption {
	return func(s *Service) {
		if f != nil {
			s.loadTechnical = f
		}
	}
}

// WithMasterPanelBuilder injects the panel factory. Without one
// the service returns ErrNotReady on master-style presets.
func WithMasterPanelBuilder(b MasterPanelBuilder) ServiceOption {
	return func(s *Service) {
		if b != nil {
			s.buildMasterPanel = b
		}
	}
}

// WithTacticPanelBuilder injects the A-share tactic panel factory.
// Phase 1 wiring leaves this nil so cn_short returns 501.
func WithTacticPanelBuilder(b TacticPanelBuilder) ServiceOption {
	return func(s *Service) {
		if b != nil {
			s.buildTacticPanel = b
		}
	}
}

// WithTacticDataLoader injects the per-symbol structural-data
// fetcher (intraday / regime / sectors / hard-risk). Without one
// tactic agents see an empty TacticInput and degrade to SKIP with
// data_unavailable.
func WithTacticDataLoader(l TacticDataLoader) ServiceOption {
	return func(s *Service) {
		if l != nil {
			s.loadTacticData = l
		}
	}
}

// WithClock injects a deterministic clock for tests.
func WithClock(c func() time.Time) ServiceOption {
	return func(s *Service) {
		if c != nil {
			s.clock = c
		}
	}
}

// WithComplianceMode pins the compliance posture. Calling with
// the zero Mode is a no-op so the wiring layer can pass a
// possibly-empty env value without resetting the default. In
// the absence of a call the service runs in ModePublisher (the
// strictest, which is the safe floor).
func WithComplianceMode(m compliance.Mode) ServiceOption {
	return func(s *Service) {
		if m == "" {
			return
		}
		s.complianceMode = m
	}
}

// WithPhraseViolationSink injects the audit hook for
// compliance.Scan results. nil disables auditing (rewrites still
// happen, they just don't get persisted). The wiring layer's
// implementation typically writes to compliance_phrase_violations.
func WithPhraseViolationSink(sink PhraseViolationSink) ServiceOption {
	return func(s *Service) {
		s.violationSink = sink
	}
}

// WithPicksRepo injects the publisher cache repo. Without it
// PublishConsult returns an error; Consult is unaffected. This
// is the seam the daily-picks loop hangs off — it's deliberately
// optional so wiring in older deployments (no migration 106) keeps
// /advisor working.
func WithPicksRepo(p PicksRepoIface) ServiceOption {
	return func(s *Service) {
		if p != nil {
			s.picksRepo = p
		}
	}
}

// NewService wires the minimum-viable service. Builders are
// injected via With* options so the production wiring can defer
// them until everything they depend on (LLM router, fundamental
// service) is ready.
func NewService(repo *Repo, opts ...ServiceOption) *Service {
	s := &Service{
		repo:           repo,
		presets:        repo,
		clock:          time.Now,
		complianceMode: compliance.DefaultMode,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Presets exposes the read interface for the handler's
// /api/advisor/presets endpoint.
func (s *Service) Presets() PresetLookup {
	if s == nil {
		return nil
	}
	return s.presets
}

// Repo returns the underlying repo so the handler can serve list /
// detail reads without going through the service layer's Consult
// codepath.
func (s *Service) Repo() *Repo {
	if s == nil {
		return nil
	}
	return s.repo
}

// panelResult is the in-memory output of the master + tactic
// orchestration. It's everything we need to either save to
// advisor_consultations (per-user mode, Consult) OR daily_picks
// (publisher mode, PublishConsult) — extracting it keeps the
// orchestration single-sourced so a fix to the panel pipeline
// applies to both surfaces.
type panelResult struct {
	Symbol     string
	Name       string
	Preset     PersonaPreset
	AggVerdict string
	AggConf    int
	Consensus  float64
	MasterRows []MasterReportRow
	TacticRows []TacticReportRow
	// Technical mirrors the snapshot fed into the master prompt
	// so we can serialise it into ConsultResponse / daily_picks
	// result_json without re-fetching. nil when the wiring
	// layer didn't supply a technical loader or the loader
	// returned no bars.
	Technical *agent.MasterTechnicalBlock
	AsOf      time.Time
}

// runPanels is the shared orchestration body. It validates the
// inputs, resolves the preset, runs the master + tactic panels,
// applies fundamentals enrichment, and projects the agent-layer
// shape onto the service-layer rows. It does NOT persist —
// persistence is the caller's choice (advisor_consultations vs
// daily_picks).
//
// userID is threaded through to the LLM panel builder so per-user
// budget enforcement still applies. In publisher mode the loop
// passes the synthetic publisher user id (s. PublishConsult below).
func (s *Service) runPanels(ctx context.Context, userID string, req ConsultRequest) (*panelResult, error) {
	if s == nil || s.repo == nil {
		return nil, ErrNotReady
	}
	symbol := strings.ToUpper(strings.TrimSpace(req.Symbol))
	if symbol == "" {
		return nil, errors.New("advisor: Symbol required")
	}

	preset, err := s.presets.Get(ctx, req.PresetKey)
	if err != nil {
		return nil, err
	}

	masterKeys, tacticKeys := s.resolveKeys(preset, req)

	asOf := s.clock()
	mIn := agent.MasterInput{
		Symbol:      symbol,
		AssetClass:  req.AssetClass,
		Market:      req.Market,
		AsOf:        asOf,
		PriceLast:   req.PriceLast,
		PriceChange: req.PriceChange,
		Currency:    req.Currency,
		Notes:       req.Notes,
	}
	if s.loadFundamental != nil {
		fb, ferr := s.loadFundamental(ctx, symbol, req.Market, req.AssetClass)
		if ferr == nil {
			mIn.Fundamentals = fb
			// Propagate the issuer name onto the master input so the
			// prompt prefixes the symbol with e.g. "name: 德科立".
			// Loader-resolved is preferred over caller-supplied so we
			// always reflect what the data provider thinks the
			// company actually is, but we don't OVERWRITE a non-empty
			// caller name with an empty loader result — keeps the
			// in-memory mock loaders (which return nil Name) from
			// blanking the UI label callers passed in.
			if fb != nil && strings.TrimSpace(fb.Name) != "" {
				mIn.Name = fb.Name
			}
		}
	}
	// Best-effort technical snapshot. A loader error or nil block
	// is treated as "snapshot unavailable" — fundamentals alone
	// are still a valid basis for a master verdict, and a Yahoo
	// throttle should never fail a consultation. The loader
	// itself owns swallowing transient errors; what reaches this
	// branch is a hard upstream failure.
	if s.loadTechnical != nil {
		if tb, terr := s.loadTechnical(ctx, symbol, req.Market, req.AssetClass); terr == nil && tb != nil {
			mIn.Technical = tb
		}
	}

	var (
		masterRows []MasterReportRow
		tacticRows []TacticReportRow
		aggVerdict string
		aggConf    int
		consensus  float64
	)

	if len(masterKeys) > 0 {
		if s.buildMasterPanel == nil {
			return nil, ErrNotReady
		}
		panel, perr := s.buildMasterPanel(ctx, userID, masterKeys)
		if perr != nil {
			return nil, fmt.Errorf("advisor: build master panel: %w", perr)
		}
		mp, rerr := panel.Run(ctx, mIn)
		if rerr != nil {
			return nil, fmt.Errorf("advisor: run master panel: %w", rerr)
		}
		masterRows = s.masterReportsFromAgent(ctx, userID, mp.Reports)
		aggVerdict = mp.Aggregate.Verdict
		aggConf = mp.Aggregate.Confidence
		consensus = mp.Aggregate.Consensus
	}

	if len(tacticKeys) > 0 {
		if s.buildTacticPanel == nil {
			// Phase 1: cn_short preset has no builder → record the
			// consultation with an empty tactic section and mark
			// the aggregate as SKIP so the UI can show "本预设需要
			// 在 Phase 4 后才能使用".
			if len(masterRows) == 0 {
				return nil, ErrUnsupportedPreset
			}
		} else {
			panel, perr := s.buildTacticPanel(ctx, userID, tacticKeys)
			if perr != nil {
				return nil, fmt.Errorf("advisor: build tactic panel: %w", perr)
			}
			tIn := agent.TacticInput{
				Symbol:      symbol,
				Name:        mIn.Name,
				Market:      req.Market,
				AsOf:        asOf,
				PriceLast:   req.PriceLast,
				PriceChange: req.PriceChange,
				Notes:       req.Notes,
			}
			if s.loadTacticData != nil {
				enriched, lerr := s.loadTacticData(ctx, tIn)
				if lerr == nil {
					tIn = enriched
				}
			}
			tp, rerr := panel.Run(ctx, tIn)
			if rerr != nil {
				return nil, fmt.Errorf("advisor: run tactic panel: %w", rerr)
			}
			tacticRows = s.tacticReportsFromAgent(ctx, userID, tp.Reports)
			if len(masterRows) == 0 {
				aggVerdict = tp.Aggregate.Verdict
				aggConf = tp.Aggregate.Confidence
				consensus = tp.Aggregate.Consensus
			}
		}
	}

	if aggVerdict == "" {
		aggVerdict = "HOLD"
		aggConf = 20
	}

	return &panelResult{
		Symbol:     symbol,
		Name:       mIn.Name,
		Preset:     preset,
		AggVerdict: aggVerdict,
		AggConf:    aggConf,
		Consensus:  consensus,
		MasterRows: masterRows,
		TacticRows: tacticRows,
		Technical:  mIn.Technical,
		AsOf:       asOf,
	}, nil
}

// Consult is the per-request orchestrator (per-USER mode). Saves to
// advisor_consultations so the user sees the result in their
// personal history.
func (s *Service) Consult(ctx context.Context, req ConsultRequest) (ConsultResponse, error) {
	if s == nil || s.repo == nil {
		return ConsultResponse{}, ErrNotReady
	}
	if strings.TrimSpace(req.UserID) == "" {
		return ConsultResponse{}, errors.New("advisor: UserID required")
	}

	pr, err := s.runPanels(ctx, req.UserID, req)
	if err != nil {
		return ConsultResponse{}, err
	}

	saveIn := SaveConsultationInput{
		UserID:              req.UserID,
		Symbol:              pr.Symbol,
		SymbolName:          pr.Name,
		Market:              req.Market,
		AssetClass:          req.AssetClass,
		PresetKey:           pr.Preset.Key,
		AggregateVerdict:    pr.AggVerdict,
		AggregateConfidence: pr.AggConf,
		ConsensusScore:      pr.Consensus,
		Notes:               req.Notes,
		MasterReports:       pr.MasterRows,
		TacticReports:       pr.TacticRows,
	}
	if req.PriceLast > 0 {
		p := req.PriceLast
		saveIn.PriceAtConsult = &p
	}
	saved, err := s.repo.SaveConsultation(ctx, saveIn)
	if err != nil {
		return ConsultResponse{}, err
	}

	return ConsultResponse{
		ConsultationID:      saved.ID,
		Symbol:              pr.Symbol,
		SymbolName:          pr.Name,
		PresetKey:           pr.Preset.Key,
		AggregateVerdict:    pr.AggVerdict,
		AggregateConfidence: pr.AggConf,
		ConsensusScore:      pr.Consensus,
		MasterReports:       pr.MasterRows,
		TacticReports:       pr.TacticRows,
		Technical:           pr.Technical,
		CreatedAt:           saved.CreatedAt,
	}, nil
}

// PublishConsultInput is the publisher-mode equivalent of
// ConsultRequest. No UserID — the publisher pipeline runs under a
// synthetic publisher identity (see PublisherUserID below) so that
// LLM cost is billed against a single publisher budget and not
// against any individual reader.
type PublishConsultInput struct {
	Symbol    string
	Market    string
	PresetKey string
	// SymbolNameHint is the resolved issuer name when the caller
	// already has it (e.g. the loop pre-loaded the watchlist with
	// a name column). When empty the fundamentals loader is the
	// only source.
	SymbolNameHint string
}

// PublishConsultOutput carries both the in-memory ConsultResponse
// shape (so the loop can log a human summary) and the metadata
// needed to UPSERT the daily_picks row (aggregate score and
// consensus are computed during the run and would otherwise have
// to be re-derived from the JSON payload).
type PublishConsultOutput struct {
	Response ConsultResponse
	// PanelResultJSON is the JSONB body the caller writes into
	// daily_picks.result_json. Pre-marshalled so the caller doesn't
	// re-encode something subtly different from what the API will
	// later serve.
	PanelResultJSON []byte
}

// PublisherUserID is the synthetic user-id the publisher pipeline
// passes into the LLM panel builders. Migration 107 inserts the
// matching users + subscriptions rows under this UUID so the
// LLM tier gate (subscription.CheckModelAccess) and the budget
// service can both look up a real enterprise-plan row instead of
// blowing up with PG 22P02 on a non-UUID sentinel.
//
// The UUID is intentionally all-zeros-but-one — visibly synthetic
// in any audit query, so a reviewer who finds "what did this
// user do?" never has to wonder which real person ran the call.
//
// Wiring contract: this constant MUST match migration 107's
// INSERT INTO users (id, ...) value. If you change one, change
// the other.
const PublisherUserID = "00000000-0000-0000-0000-000000000001"

// PublishConsult runs the same persona-panel pipeline as Consult
// but writes the result into daily_picks (the shared publisher
// cache) instead of advisor_consultations (the per-user history).
//
// Caching:
//
//	Returns ErrNotReady when picksRepo is nil. The CALLER is
//	responsible for deciding when to call this — typically the
//	daily picks loop after a cache-miss check via picksRepo.Get.
//	PublishConsult itself does NOT short-circuit on an existing
//	row; if you call it twice for the same (symbol, market,
//	preset, pickDate) you'll burn LLM dollars twice. The loop
//	owns the cache-miss gate.
//
// Pick date:
//
//	The pick_date stamped on the row is the calling clock's UTC
//	date. The loop is expected to invoke PublishConsult per
//	(symbol) on the trading day it represents.
func (s *Service) PublishConsult(ctx context.Context, in PublishConsultInput) (*PublishConsultOutput, error) {
	if s == nil || s.repo == nil {
		return nil, ErrNotReady
	}
	if s.picksRepo == nil {
		return nil, errors.New("advisor: picks repo not wired; PublishConsult unavailable")
	}
	symbol := strings.ToUpper(strings.TrimSpace(in.Symbol))
	if symbol == "" {
		return nil, errors.New("advisor: Symbol required")
	}
	if strings.TrimSpace(in.Market) == "" {
		return nil, errors.New("advisor: Market required")
	}
	if strings.TrimSpace(in.PresetKey) == "" {
		return nil, errors.New("advisor: PresetKey required")
	}

	req := ConsultRequest{
		UserID:    PublisherUserID,
		Symbol:    symbol,
		Market:    in.Market,
		PresetKey: in.PresetKey,
		// Notes intentionally empty in publisher mode — the prompt
		// must be identical for every reader, and per-user notes
		// would silently personalise the output.
	}
	pr, err := s.runPanels(ctx, PublisherUserID, req)
	if err != nil {
		return nil, err
	}

	// SymbolNameHint wins over loader-resolved when both exist,
	// because the loop's hint typically comes from the watchlist
	// curation (i.e. an admin verified the name) and the loader
	// can be transiently stale.
	displayName := pr.Name
	if hint := strings.TrimSpace(in.SymbolNameHint); hint != "" {
		displayName = hint
	}

	pickDate := s.clock().UTC().Truncate(24 * time.Hour)

	resp := ConsultResponse{
		ConsultationID:      "", // publisher rows are keyed by daily_picks.id, not advisor_consultations.id
		Symbol:              pr.Symbol,
		SymbolName:          displayName,
		PresetKey:           pr.Preset.Key,
		AggregateVerdict:    pr.AggVerdict,
		AggregateConfidence: pr.AggConf,
		ConsensusScore:      pr.Consensus,
		MasterReports:       pr.MasterRows,
		TacticReports:       pr.TacticRows,
		Technical:           pr.Technical,
		CreatedAt:           pr.AsOf,
	}
	body, err := marshalPanelResult(resp)
	if err != nil {
		return nil, fmt.Errorf("advisor: marshal publisher result: %w", err)
	}

	// aggregateScore is the headline 0-100 we sort the browse-grid
	// by. We derive it from confidence × consensus rather than
	// using confidence alone because a high-confidence verdict
	// without consensus (one screaming master, the rest abstaining)
	// is empirically less reliable than a moderate-confidence
	// verdict with broad agreement.
	aggScore := 0
	if strings.EqualFold(pr.AggVerdict, "STRONG_BUY") || strings.EqualFold(pr.AggVerdict, "BUY") {
		aggScore = int(float64(pr.AggConf) * (0.5 + 0.5*pr.Consensus))
	} else if strings.EqualFold(pr.AggVerdict, "AVOID") || strings.EqualFold(pr.AggVerdict, "SHORT") {
		// Negative-direction verdicts get a NEGATIVE score so the
		// browse grid orders "strongest avoid → … → strongest buy"
		// naturally when sorted DESC; the UI can flip if needed.
		aggScore = -int(float64(pr.AggConf) * (0.5 + 0.5*pr.Consensus))
	}

	if _, perr := s.picksRepo.UpsertPick(ctx, PicksUpsertInput{
		Symbol:           pr.Symbol,
		SymbolName:       displayName,
		Market:           in.Market,
		PresetKey:        pr.Preset.Key,
		PickDate:         pickDate,
		ResultJSON:       body,
		AggregateVerdict: pr.AggVerdict,
		AggregateScore:   aggScore,
		Consensus:        pr.Consensus,
		LLMCostUSD:       0, // future: thread per-call cost from the LLM adapter
	}); perr != nil {
		return nil, fmt.Errorf("advisor: upsert publisher pick: %w", perr)
	}

	return &PublishConsultOutput{Response: resp, PanelResultJSON: body}, nil
}

// marshalPanelResult is the canonical "what does a publisher row
// look like on disk" serializer. Centralised so the consult
// pipeline and the read path both share one definition — if we
// ever change the JSON shape (e.g. trim fields for size), this is
// the single place to edit.
//
// Compact encoding (no indent) keeps the daily_picks.result_json
// blob small; the read path returns it as raw JSON to the API so
// pretty-printing happens at the rendering edge.
func marshalPanelResult(resp ConsultResponse) ([]byte, error) {
	return jsonMarshal(resp)
}

// resolveKeys honours the custom-preset escape hatch only when the
// preset's stored keys are empty. Everything else gets the stored
// lists verbatim so the UI's "you picked Conservative" label
// matches the actual panel that voted.
func (s *Service) resolveKeys(preset PersonaPreset, req ConsultRequest) (masterKeys, tacticKeys []string) {
	if preset.Kind() == PresetKindEmpty {
		return dedupAndTrim(req.CustomMasterKeys), dedupAndTrim(req.CustomTacticKeys)
	}
	return preset.MasterKeys, preset.TacticKeys
}

// masterReportsFromAgent maps the agent-package shape onto the
// service-package shape. Done here rather than in the agent layer
// so the agent package stays free of advisor-specific types.
//
// SEC-compliance side-effect (Publisher mode only): the master's
// LLM-generated free-form text fields (Thesis, KeyReasons,
// KeyRisks) are passed through compliance.Scan to redact
// forbidden phrases like "we recommend" / "buy now" / "suggested
// position". The redacted text replaces the original BEFORE it
// crosses the service boundary. The raw verdict enum
// (BUY/HOLD/AVOID) stays untouched — it's just an enum, and the
// front-end's i18n layer is what renders it as a model-state
// label rather than a recommendation. Violations are forwarded
// to the configured PhraseViolationSink for audit.
func (s *Service) masterReportsFromAgent(ctx context.Context, userID string, in []agent.MasterReport) []MasterReportRow {
	out := make([]MasterReportRow, 0, len(in))
	for _, r := range in {
		thesis, keyReasons, keyRisks := s.redactMasterText(ctx, userID, r)
		out = append(out, MasterReportRow{
			MasterKey:      r.MasterKey,
			MasterNameZh:   r.MasterNameZh,
			MasterNameEn:   r.MasterNameEn,
			SymbolName:     r.SymbolName,
			Verdict:        r.Verdict,
			Confidence:     r.Confidence,
			Thesis:         thesis,
			KeyReasons:     keyReasons,
			KeyRisks:       keyRisks,
			MasterSpecific: r.MasterSpecific,
			RedLinesHit:    r.RedLinesHit,
			LLMModel:       r.LLMModel,
			GeneratedAt:    r.GeneratedAt,
		})
	}
	return out
}

func (s *Service) tacticReportsFromAgent(ctx context.Context, userID string, in []agent.TacticReport) []TacticReportRow {
	out := make([]TacticReportRow, 0, len(in))
	for _, r := range in {
		thesis, keyReasons, keyRisks := s.redactTacticText(ctx, userID, r)
		out = append(out, TacticReportRow{
			TacticKey:           r.TacticKey,
			TacticNameZh:        r.TacticNameZh,
			TacticNameEn:        r.TacticNameEn,
			SymbolName:          r.SymbolName,
			Verdict:             r.Verdict,
			Confidence:          r.Confidence,
			Thesis:              thesis,
			EntryPriceLow:       r.EntryPriceLow,
			EntryPriceHigh:      r.EntryPriceHigh,
			StopLossPrice:       r.StopLossPrice,
			TargetT1:            r.TargetT1,
			TargetT3:            r.TargetT3,
			ExpectedHoldingDays: r.ExpectedHoldingDays,
			Score:               r.Score,
			KeyReasons:          keyReasons,
			KeyRisks:            keyRisks,
			RedLinesHit:         r.RedLinesHit,
			MarketRegimePass:    r.MarketRegimePass,
			MarketRegimeReason:  r.MarketRegimeReason,
			GeneratedAt:         r.GeneratedAt,
		})
	}
	return out
}

// redactMasterText runs the three free-form text fields through
// the compliance scanner (Publisher mode) and reports all
// violations to the sink under one (userID, surface, master_key)
// tuple. In RIA mode the scanner is a pass-through.
func (s *Service) redactMasterText(ctx context.Context, userID string, r agent.MasterReport) (string, []string, []string) {
	mode := s.activeComplianceMode()
	thesisRes := compliance.MaybeScan(mode, r.Thesis)
	reasonsResults := scanStrings(mode, r.KeyReasons)
	risksResults := scanStrings(mode, r.KeyRisks)
	// Collect every violation so a single audit row reads "in
	// this master's output we redacted N phrases" rather than
	// fragmenting per field.
	all := append([]compliance.Violation{}, thesisRes.Violations...)
	for _, x := range reasonsResults {
		all = append(all, x.Violations...)
	}
	for _, x := range risksResults {
		all = append(all, x.Violations...)
	}
	if len(all) > 0 && s.violationSink != nil {
		s.violationSink(ctx, userID, "advisor", "advisor_master_report", r.MasterKey, thesisRes.Redacted, all)
	}
	return thesisRes.Redacted, joinScanResults(reasonsResults), joinScanResults(risksResults)
}

func (s *Service) redactTacticText(ctx context.Context, userID string, r agent.TacticReport) (string, []string, []string) {
	mode := s.activeComplianceMode()
	thesisRes := compliance.MaybeScan(mode, r.Thesis)
	reasonsResults := scanStrings(mode, r.KeyReasons)
	risksResults := scanStrings(mode, r.KeyRisks)
	all := append([]compliance.Violation{}, thesisRes.Violations...)
	for _, x := range reasonsResults {
		all = append(all, x.Violations...)
	}
	for _, x := range risksResults {
		all = append(all, x.Violations...)
	}
	if len(all) > 0 && s.violationSink != nil {
		s.violationSink(ctx, userID, "advisor", "advisor_tactic_report", r.TacticKey, thesisRes.Redacted, all)
	}
	return thesisRes.Redacted, joinScanResults(reasonsResults), joinScanResults(risksResults)
}

// activeComplianceMode handles the zero-value case (an explicit
// WithComplianceMode was never called). Default = Publisher, the
// strictest mode, so an unconfigured service still gets the
// safest behaviour.
func (s *Service) activeComplianceMode() compliance.Mode {
	if s == nil || s.complianceMode == "" {
		return compliance.DefaultMode
	}
	return s.complianceMode
}

func scanStrings(mode compliance.Mode, in []string) []compliance.ScanResult {
	out := make([]compliance.ScanResult, 0, len(in))
	for _, s := range in {
		out = append(out, compliance.MaybeScan(mode, s))
	}
	return out
}

func joinScanResults(in []compliance.ScanResult) []string {
	out := make([]string, 0, len(in))
	for _, r := range in {
		out = append(out, r.Redacted)
	}
	return out
}
