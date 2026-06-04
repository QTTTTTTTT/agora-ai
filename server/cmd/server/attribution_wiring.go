// attribution_wiring.go is the bridge between the Phase 3A-5
// attribution.Service domain layer and the HTTP / DB plumbing
// that already exists in this binary.
//
// Two responsibilities:
//
//  1. Build an *attribution.Service from raw repos + a clock,
//     including the dependency-injection plumbing the rest of
//     this file shares.
//
//  2. Provide an *attributionServiceAdapter that satisfies
//     api.AttributionService, applies the standard fund
//     access check, calls the domain service, and translates
//     the domain types into api.AttributionResponse DTOs.
//
// The domain service has no SQL of its own — it talks to the
// repositories through the small interfaces in
// internal/attribution/attribution.go. Keeping the SQL behind
// repositories means this file can stay a thin translation
// layer: zero business logic, only mapping.
package main

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/attribution"
	"github.com/fundai/server/internal/repository"
)

// attributionServiceAdapter wraps an *attribution.Service so it
// can be plugged into api.FundHandler via WithAttributionService.
// Returns nil when persistence isn't available — the handler
// then degrades to a 503 just like the other optional services.
type attributionServiceAdapter struct {
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	memoryRepo  *repository.MemoryRepo
	service     *attribution.Service
}

// newAttributionServiceAdapter builds the adapter from a raw
// *sql.DB. nil-safe: a nil db returns nil so callers can wire
// it unconditionally and rely on the handler's nil-guard.
func newAttributionServiceAdapter(db *sql.DB) *attributionServiceAdapter {
	if db == nil {
		return nil
	}
	lotRepo := repository.NewLotRepo(db)
	memoryRepo := repository.NewMemoryRepo(db)
	svc := attribution.NewService(lotRepo, memoryRepo)
	if svc == nil {
		return nil
	}
	return &attributionServiceAdapter{
		fundRepo:    repository.NewFundRepo(db),
		companyRepo: repository.NewFundCompanyRepo(db),
		memoryRepo:  memoryRepo,
		service:     svc,
	}
}

// Service exposes the underlying domain service so other wiring
// code (e.g. the daily review hook) can call RunAndPersist
// without going through the HTTP-shaped adapter. Returns nil
// when the adapter itself is nil.
func (a *attributionServiceAdapter) Service() *attribution.Service {
	if a == nil {
		return nil
	}
	return a.service
}

// GetAttribution is the api.AttributionService implementation.
// It enforces fund-level access using the same authorizer the
// rest of the fund-scoped endpoints share, then translates the
// domain report + recent persisted lessons into the response DTO.
func (a *attributionServiceAdapter) GetAttribution(userID, fundID string, days int) (*api.AttributionResponse, error) {
	if a == nil || a.service == nil {
		return nil, api.ErrNotFound
	}
	ctx := context.Background()
	fund, err := authorizeFundAccess(ctx, a.fundRepo, a.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	if days <= 0 {
		days = attribution.DefaultLookbackDays
	}
	report, err := a.service.BuildReport(ctx, fund.ID, days)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return a.composeResponse(ctx, fund.ID, report), nil
}

// RefreshAttribution is the operator-driven "run it now" path.
// It forces the attribution service to rebuild the report AND
// persist any new lessons immediately, then returns the same DTO
// shape as GetAttribution. The two-step (run -> compose) keeps
// the response on the same wire format as the GET path so the
// UI can render the response without branching on the trigger.
//
// Persistence failure inside RunAndPersist is surfaced to the
// caller (unlike the daily-review hook which swallows it) so the
// operator who pressed "refresh" sees the diagnostic.
func (a *attributionServiceAdapter) RefreshAttribution(userID, fundID string, days int) (*api.AttributionResponse, error) {
	if a == nil || a.service == nil {
		return nil, api.ErrNotFound
	}
	ctx := context.Background()
	fund, err := authorizeFundAccess(ctx, a.fundRepo, a.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	if days <= 0 {
		days = attribution.DefaultLookbackDays
	}
	report, _, err := a.service.RunAndPersist(ctx, fund.ID, "", days)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return a.composeResponse(ctx, fund.ID, report), nil
}

// composeResponse is the shared translation between an in-memory
// AttributionReport (built either by GetAttribution or
// RunAndPersist) and the wire DTO. Centralising it ensures the
// two endpoints emit identical envelopes.
func (a *attributionServiceAdapter) composeResponse(ctx context.Context, fundID string, report *attribution.AttributionReport) *api.AttributionResponse {
	resp := &api.AttributionResponse{
		FundID:         fundID,
		WindowDays:     report.Window.Days,
		Since:          report.Window.Since,
		GeneratedAt:    report.GeneratedAt,
		BySleeve:       sleeveStatsToDTO(report.BySleeve),
		ByRegime:       regimeStatsToDTO(report.ByRegime),
		BySleeveRegime: sleeveRegimeStatsToDTO(report.BySleeveRegime),
		Lessons:        []api.AttributionLessonDTO{},
	}
	// Lessons aren't part of the domain report — they live in
	// the memory store. Pull the most recent batch for this fund
	// and translate the rows in place. We don't fail the response
	// on listing errors so the stats still render even if the
	// memory tier is briefly unavailable.
	if rows, listErr := a.memoryRepo.ListByFund(ctx, fundID, attribution.MemoryLayer, 50); listErr == nil {
		resp.Lessons = memoryRowsToLessonDTO(rows)
	}
	return resp
}

// ---------------------------------------------------------------------------
// Conversions
// ---------------------------------------------------------------------------

func sleeveStatsToDTO(stats []repository.SleeveStat) []api.SleeveStatDTO {
	out := make([]api.SleeveStatDTO, 0, len(stats))
	for _, s := range stats {
		out = append(out, api.SleeveStatDTO{
			Sleeve:         emptyToUnspecified(s.Sleeve),
			TradeCount:     s.TradeCount,
			WinCount:       s.WinCount,
			LossCount:      s.LossCount,
			TotalPnL:       s.TotalPnL,
			AvgPnLPct:      s.AvgPnLPct,
			WinRate:        s.WinRate,
			MedianHoldDays: s.MedianHoldDays,
		})
	}
	return out
}

func regimeStatsToDTO(stats []repository.RegimeStat) []api.RegimeStatDTO {
	out := make([]api.RegimeStatDTO, 0, len(stats))
	for _, s := range stats {
		out = append(out, api.RegimeStatDTO{
			Regime:     emptyToUnspecified(s.Regime),
			TradeCount: s.TradeCount,
			WinCount:   s.WinCount,
			LossCount:  s.LossCount,
			TotalPnL:   s.TotalPnL,
			AvgPnLPct:  s.AvgPnLPct,
			WinRate:    s.WinRate,
		})
	}
	return out
}

func sleeveRegimeStatsToDTO(stats []repository.SleeveRegimeStat) []api.SleeveRegimeStatDTO {
	out := make([]api.SleeveRegimeStatDTO, 0, len(stats))
	for _, s := range stats {
		out = append(out, api.SleeveRegimeStatDTO{
			Sleeve:         emptyToUnspecified(s.Sleeve),
			Regime:         emptyToUnspecified(s.Regime),
			TradeCount:     s.TradeCount,
			WinCount:       s.WinCount,
			LossCount:      s.LossCount,
			TotalPnL:       s.TotalPnL,
			AvgPnLPct:      s.AvgPnLPct,
			WinRate:        s.WinRate,
			AvgHoldingDays: s.AvgHoldingDays,
		})
	}
	return out
}

func memoryRowsToLessonDTO(rows []repository.Memory) []api.AttributionLessonDTO {
	out := make([]api.AttributionLessonDTO, 0, len(rows))
	for _, row := range rows {
		dto := api.AttributionLessonDTO{
			Kind:      lessonKindFromTags(row.Tags),
			Severity:  severityFromTags(row.Tags),
			Title:     row.Title.String,
			Body:      row.Content,
			Tags:      row.Tags,
			CreatedAt: row.CreatedAt,
		}
		// S15 i18n contract: surface template_key + payload exactly
		// as they were persisted so the StrategyAttributionPanel
		// renders in the user's locale via lessonRenderer.ts. Legacy
		// rows (template_key NULL, written before migration 085)
		// leave both fields zero — omitempty drops them off the wire
		// and the frontend keeps using Title/Body unchanged.
		if row.TemplateKey.Valid && strings.TrimSpace(row.TemplateKey.String) != "" {
			dto.TemplateKey = row.TemplateKey.String
			if len(row.Payload) > 0 {
				// Pass the raw jsonb bytes through verbatim. We avoid
				// re-marshalling so the JSON encoder emits the exact
				// bytes Postgres stored, which keeps the field-set
				// stable for the frontend snapshot tests.
				dto.Payload = row.Payload
			}
		}
		out = append(out, dto)
	}
	// Newest-first for the UI rail.
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// lessonKindFromTags decodes the lesson kind out of the tag set
// the lesson generator wrote. Falls back to "info" when tags are
// missing or unrecognised so the DTO field is never empty.
func lessonKindFromTags(tags []string) string {
	for _, t := range tags {
		switch t {
		case "loser":
			return string(attribution.LessonSleeveRegimeLoser)
		case "winner":
			return string(attribution.LessonSleeveRegimeWinner)
		case "insufficient_data":
			return string(attribution.LessonInsufficientData)
		}
	}
	return string(attribution.LessonInsufficientData)
}

// severityFromTags mirrors the lesson generator's severity gate:
// loser → critical, winner → info, anything else → info.
func severityFromTags(tags []string) string {
	for _, t := range tags {
		if t == "loser" {
			return string(attribution.SeverityCritical)
		}
	}
	return string(attribution.SeverityInfo)
}

func emptyToUnspecified(s string) string {
	if s == "" {
		return "(unspecified)"
	}
	return s
}

// runDailyAttribution is the hook the daily-review pipeline calls
// after a fund's daily run completes. It's deliberately a free
// function (not on the adapter) so the workflow side can call it
// with whatever clock context it wants. nil-safe: zero-deps run
// without touching anything.
func runDailyAttribution(svc *attribution.Service, fundID string) {
	if svc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := svc.RunAndPersist(ctx, fundID, "", attribution.DefaultLookbackDays); err != nil {
		// Soft failure: an attribution stall must never block the
		// trading workflow itself. Log at warn and move on.
		_ = err
	}
}
