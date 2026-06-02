// analyst_panel_handler.go — S8.1 per-fund analyst panel REST.
//
// Routes
//
//   POST /api/funds/{fundId}/analysts/run
//        Body: AnalystRunRequest (JSON)
//        Synchronously runs the four-analyst panel for one
//        symbol on the input the caller provides, persists the
//        result, and returns it.
//
//   GET  /api/funds/{fundId}/analysts/panels[?symbol=X&from=...&to=...&limit=N&include=children]
//        Lists historical panel reports for a fund.
//
//   GET  /api/funds/{fundId}/analysts/panels/{panelId}
//        Fetches one panel report + the four per-category
//        reports inside it.
//
// Auth: fund-level access; admin-only writes are not used here.

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/agent"
	"github.com/fundai/server/internal/analystreport"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

type analystPanelHandler struct {
	repo        *analystreport.Repo
	panel       *agent.AnalystPanel
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	now         func() time.Time
}

// AnalystPanelProvider is the wiring-layer hook used to obtain
// the fund-specific panel at request time. Different funds can
// run different personas / LLM bindings; the wiring layer
// supplies a factory rather than a singleton panel.
type AnalystPanelProvider func(fundID string) *agent.AnalystPanel

// newAnalystPanelHandler builds the handler. Returns nil when
// the repo is absent so the wiring layer can no-op without
// crashing on startup.
func newAnalystPanelHandler(svc *Services, provider AnalystPanelProvider) *analystPanelHandler {
	if svc == nil || svc.DB == nil || svc.AnalystReportRepo == nil {
		return nil
	}
	return &analystPanelHandler{
		repo:        svc.AnalystReportRepo,
		panel:       nil, // resolved per-request via provider
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
		now:         time.Now,
	}
}

func (h *analystPanelHandler) RegisterRoutes(mux *http.ServeMux, provider AnalystPanelProvider) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/funds/{fundId}/analysts/run",
		h.wrapRun(provider))
	mux.HandleFunc("GET /api/funds/{fundId}/analysts/panels",
		h.handleListPanels)
	mux.HandleFunc("GET /api/funds/{fundId}/analysts/panels/{panelId}",
		h.handleGetPanel)
}

// --- Wire shapes (mirrored in shared/api-client) ---------------------------

type analystRunRequest struct {
	Symbol     string `json:"symbol"`
	AssetClass string `json:"asset_class,omitempty"`
	Market     string `json:"market,omitempty"`
	AsOf       string `json:"asof,omitempty"`
	Notes      string `json:"notes,omitempty"`
	Persist    bool   `json:"persist"`

	// Prices / volume (shared block).
	PriceLast   float64 `json:"price_last,omitempty"`
	PriceChange float64 `json:"price_change,omitempty"`
	Volume      float64 `json:"volume,omitempty"`
	AvgVolume   float64 `json:"avg_volume,omitempty"`

	Fundamentals *fundamentalsBlockWire `json:"fundamentals,omitempty"`
	Sentiment    *sentimentBlockWire    `json:"sentiment,omitempty"`
	News         *newsBlockWire         `json:"news,omitempty"`
	Technical    *technicalBlockWire    `json:"technical,omitempty"`
}

type fundamentalsBlockWire struct {
	QualityScore  *qualityScoreWire `json:"quality_score,omitempty"`
	Metrics       map[string]float64 `json:"metrics,omitempty"`
	IndustryPeers []string `json:"industry_peers,omitempty"`
	FilingsURL    string `json:"filings_url,omitempty"`
}

type qualityScoreWire struct {
	ProfitabilityZ float64 `json:"profitability_z"`
	GrowthZ        float64 `json:"growth_z"`
	SafetyZ        float64 `json:"safety_z"`
	CompositeZ     float64 `json:"composite_z"`
	Quartile       int     `json:"quartile"`
}

type sentimentBlockWire struct {
	Aggregate       sentimentAggregateWire `json:"aggregate"`
	RecentItems     []sentimentItemWire    `json:"recent_items,omitempty"`
	SourceBreakdown map[string]int         `json:"source_breakdown,omitempty"`
}

type sentimentAggregateWire struct {
	Average  float64 `json:"average"`
	Count    int     `json:"count"`
	Polarity string  `json:"polarity"`
}

type sentimentItemWire struct {
	Title       string    `json:"title"`
	Source      string    `json:"source"`
	Score       float64   `json:"score"`
	PublishedAt time.Time `json:"published_at,omitempty"`
	URL         string    `json:"url,omitempty"`
}

type newsBlockWire struct {
	Headlines         []newsHeadlineWire `json:"headlines,omitempty"`
	MaterialEventTags []string           `json:"material_event_tags,omitempty"`
}

type newsHeadlineWire struct {
	Title       string    `json:"title"`
	Source      string    `json:"source"`
	Summary     string    `json:"summary,omitempty"`
	PublishedAt time.Time `json:"published_at,omitempty"`
	URL         string    `json:"url,omitempty"`
	Language    string    `json:"language,omitempty"`
}

type technicalBlockWire struct {
	Snapshot          quantSnapshotWire   `json:"snapshot"`
	Signals           map[string]float64  `json:"signals,omitempty"`
	PriceHistorySpark []float64           `json:"price_history_spark,omitempty"`
}

type quantSnapshotWire struct {
	Regime                 string  `json:"regime"`
	Close                  float64 `json:"close"`
	ATR14                  float64 `json:"atr14"`
	ATRPct                 float64 `json:"atr_pct"`
	PositionSizeCeilingPct float64 `json:"position_size_ceiling_pct"`
}

// PanelReport wire output.

type analystReportWire struct {
	ID               string             `json:"id,omitempty"`
	AgentID          string             `json:"agent_id"`
	AgentName        string             `json:"agent_name"`
	Category         string             `json:"category"`
	Symbol           string             `json:"symbol"`
	AsOf             string             `json:"asof"`
	GeneratedAt      string             `json:"generated_at"`
	Direction        string             `json:"direction"`
	Confidence       int                `json:"confidence"`
	Thesis           string             `json:"thesis"`
	KeyFindings      []string           `json:"key_findings"`
	Risks            []string           `json:"risks"`
	DataPoints       []analystDataPointWire `json:"data_points,omitempty"`
	Sources          []string           `json:"sources,omitempty"`
	PromptTokens     int                `json:"prompt_tokens,omitempty"`
	CompletionTokens int                `json:"completion_tokens,omitempty"`
	LLMModel         string             `json:"llm_model,omitempty"`
}

type analystDataPointWire struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Source string `json:"source,omitempty"`
}

type analystPanelWire struct {
	ID                 string                   `json:"id,omitempty"`
	FundID             string                   `json:"fund_id"`
	Symbol             string                   `json:"symbol"`
	AsOf               string                   `json:"asof"`
	GeneratedAt        string                   `json:"generated_at"`
	AggregateDirection string                   `json:"aggregate_direction"`
	AggregateConfidence int                     `json:"aggregate_confidence"`
	CategoriesVoted    int                      `json:"categories_voted"`
	PerCategoryVotes   map[string]int           `json:"per_category_votes"`
	Reports            []analystReportWire      `json:"reports"`
}

// --- Handlers --------------------------------------------------------------

func (h *analystPanelHandler) wrapRun(provider AnalystPanelProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.handleRun(w, r, provider)
	}
}

func (h *analystPanelHandler) handleRun(w http.ResponseWriter, r *http.Request, provider AnalystPanelProvider) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	var req analystRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(req.Symbol) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("symbol_required", ""))
		return
	}
	if provider == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("panel_unavailable", "no analyst panel configured for this deployment"))
		return
	}
	panel := provider(fundID)
	if panel == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("panel_unavailable", "no analyst panel for this fund"))
		return
	}
	input := buildAnalystInput(req, h.now())
	rep, err := panel.RunSymbol(r.Context(), input)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("panel_run_failed", err.Error()))
		return
	}
	rep.FundID = fundID
	var panelID string
	if req.Persist {
		if pid, perr := h.repo.SavePanel(r.Context(), rep); perr != nil {
			// Persist failure shouldn't drop the result.
			writeJSON(w, http.StatusOK, map[string]any{
				"panel":         projectPanel(rep, ""),
				"persist_error": perr.Error(),
			})
			return
		} else {
			panelID = pid
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"panel": projectPanel(rep, panelID),
	})
}

func (h *analystPanelHandler) handleListPanels(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	q := r.URL.Query()
	params := analystreport.ListPanelsParams{
		FundID:          fundID,
		Symbol:          strings.TrimSpace(q.Get("symbol")),
		IncludeChildren: q.Get("include") == "children",
	}
	if v := strings.TrimSpace(q.Get("from")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			params.AsOfFrom = t
		}
	}
	if v := strings.TrimSpace(q.Get("to")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			params.AsOfTo = t
		}
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			params.Limit = n
		}
	}
	rows, err := h.repo.ListPanels(r.Context(), params)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]analystPanelWire, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectPanelRow(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"panels": out})
}

func (h *analystPanelHandler) handleGetPanel(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	panelID := strings.TrimSpace(r.PathValue("panelId"))
	if fundID == "" || panelID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId and panelId required"))
		return
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	row, err := h.repo.GetPanel(r.Context(), panelID)
	if err != nil {
		if errors.Is(err, analystreport.ErrNotFound) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("panel_not_found", panelID))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if row.FundID != fundID {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("panel_not_found", panelID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"panel": projectPanelRow(row)})
}

// --- Wire builders ---------------------------------------------------------

func buildAnalystInput(req analystRunRequest, defaultAsOf time.Time) agent.AnalystInput {
	asof := defaultAsOf
	if v := strings.TrimSpace(req.AsOf); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			asof = t
		} else if t, err := time.Parse("2006-01-02", v); err == nil {
			asof = t
		}
	}
	in := agent.AnalystInput{
		Symbol:      strings.ToUpper(strings.TrimSpace(req.Symbol)),
		AssetClass:  req.AssetClass,
		Market:      req.Market,
		AsOf:        asof,
		Notes:       req.Notes,
		PriceLast:   req.PriceLast,
		PriceChange: req.PriceChange,
		Volume:      req.Volume,
		AvgVolume:   req.AvgVolume,
	}
	if req.Fundamentals != nil {
		in.Fundamentals = &agent.FundamentalsBlock{
			Metrics:       req.Fundamentals.Metrics,
			IndustryPeers: req.Fundamentals.IndustryPeers,
			FilingsURL:    req.Fundamentals.FilingsURL,
		}
		if q := req.Fundamentals.QualityScore; q != nil {
			in.Fundamentals.QualityScore = &agent.QualityScoreLite{
				ProfitabilityZ: q.ProfitabilityZ,
				GrowthZ:        q.GrowthZ,
				SafetyZ:        q.SafetyZ,
				CompositeZ:     q.CompositeZ,
				Quartile:       q.Quartile,
			}
		}
	}
	if req.Sentiment != nil {
		items := make([]agent.SentimentItemLite, len(req.Sentiment.RecentItems))
		for i, it := range req.Sentiment.RecentItems {
			items[i] = agent.SentimentItemLite{
				Title: it.Title, Source: it.Source, Score: it.Score,
				PublishedAt: it.PublishedAt, URL: it.URL,
			}
		}
		in.Sentiment = &agent.SentimentBlock{
			Aggregate: agent.SentimentAggregateLite{
				Average:  req.Sentiment.Aggregate.Average,
				Count:    req.Sentiment.Aggregate.Count,
				Polarity: req.Sentiment.Aggregate.Polarity,
			},
			RecentItems:     items,
			SourceBreakdown: req.Sentiment.SourceBreakdown,
		}
	}
	if req.News != nil {
		hs := make([]agent.NewsHeadline, len(req.News.Headlines))
		for i, h := range req.News.Headlines {
			hs[i] = agent.NewsHeadline{
				Title: h.Title, Source: h.Source, Summary: h.Summary,
				PublishedAt: h.PublishedAt, URL: h.URL, Language: h.Language,
			}
		}
		in.News = &agent.NewsBlock{
			Headlines:         hs,
			MaterialEventTags: req.News.MaterialEventTags,
		}
	}
	if req.Technical != nil {
		in.Technical = &agent.TechnicalBlock{
			Snapshot: agent.QuantSnapshotLite{
				Regime: req.Technical.Snapshot.Regime,
				Close:  req.Technical.Snapshot.Close,
				ATR14:  req.Technical.Snapshot.ATR14,
				ATRPct: req.Technical.Snapshot.ATRPct,
				PositionSizeCeilingPct: req.Technical.Snapshot.PositionSizeCeilingPct,
			},
			Signals:           req.Technical.Signals,
			PriceHistorySpark: req.Technical.PriceHistorySpark,
		}
	}
	return in
}

func projectPanel(p agent.PanelReport, panelID string) analystPanelWire {
	votes := make(map[string]int, len(p.Aggregate.PerCategoryVotes))
	for k, v := range p.Aggregate.PerCategoryVotes {
		votes[string(k)] = v
	}
	out := analystPanelWire{
		ID:                  panelID,
		FundID:              p.FundID,
		Symbol:              p.Symbol,
		AsOf:                p.AsOf.UTC().Format(time.RFC3339Nano),
		GeneratedAt:         p.GeneratedAt.UTC().Format(time.RFC3339Nano),
		AggregateDirection:  string(p.Aggregate.Direction),
		AggregateConfidence: p.Aggregate.Confidence,
		CategoriesVoted:     p.Aggregate.CategoriesVoted,
		PerCategoryVotes:    votes,
	}
	for _, r := range p.Reports {
		out.Reports = append(out.Reports, projectAnalystReport(r))
	}
	return out
}

func projectAnalystReport(r agent.AnalystReport) analystReportWire {
	w := analystReportWire{
		ID:               r.ID,
		AgentID:          r.AgentID,
		AgentName:        r.AgentName,
		Category:         string(r.Category),
		Symbol:           r.Symbol,
		AsOf:             r.AsOf.UTC().Format(time.RFC3339Nano),
		GeneratedAt:      r.GeneratedAt.UTC().Format(time.RFC3339Nano),
		Direction:        string(r.Direction),
		Confidence:       r.Confidence,
		Thesis:           r.Thesis,
		KeyFindings:      r.KeyFindings,
		Risks:            r.Risks,
		Sources:          r.Sources,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		LLMModel:         r.LLMModel,
	}
	for _, dp := range r.DataPoints {
		w.DataPoints = append(w.DataPoints, analystDataPointWire{
			Name: dp.Name, Value: dp.Value, Source: dp.Source,
		})
	}
	return w
}

func projectPanelRow(row analystreport.PanelRow) analystPanelWire {
	out := analystPanelWire{
		ID:                  row.ID,
		FundID:              row.FundID,
		Symbol:              row.Symbol,
		AsOf:                row.AsOf.UTC().Format(time.RFC3339Nano),
		GeneratedAt:         row.GeneratedAt.UTC().Format(time.RFC3339Nano),
		AggregateDirection:  row.AggDirection,
		AggregateConfidence: row.AggConfidence,
		CategoriesVoted:     row.CategoriesVoted,
		PerCategoryVotes:    row.PerCategoryVote,
	}
	for _, c := range row.Reports {
		w := analystReportWire{
			ID:               c.ID,
			AgentID:          c.AgentID,
			AgentName:        c.AgentName,
			Category:         c.Category,
			Symbol:           c.Symbol,
			AsOf:             c.AsOf.UTC().Format(time.RFC3339Nano),
			GeneratedAt:      c.GeneratedAt.UTC().Format(time.RFC3339Nano),
			Direction:        c.Direction,
			Confidence:       c.Confidence,
			Thesis:           c.Thesis,
			KeyFindings:      c.KeyFindings,
			Risks:            c.Risks,
			Sources:          c.Sources,
			PromptTokens:     c.PromptTokens,
			CompletionTokens: c.CompletionTokens,
			LLMModel:         c.LLMModel,
		}
		for _, dp := range c.DataPoints {
			w.DataPoints = append(w.DataPoints, analystDataPointWire{
				Name: dp.Name, Value: dp.Value, Source: dp.Source,
			})
		}
		out.Reports = append(out.Reports, w)
	}
	return out
}
