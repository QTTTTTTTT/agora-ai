package attribution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// MemoryLayer is the value written into memories.layer for every
// lesson the Service persists. The API layer's reflections /
// learning endpoints already filter by layer, so picking a stable
// string here is a contract; renaming it is a forwards-incompatible
// change.
const MemoryLayer = "attribution"

// Service is the read-write coordinator. Two repos:
//
//   - LotStatsRepo to pull aggregate stats from closed_lots.
//   - MemoryWriter to persist the deterministic lessons we
//     derive from those stats.
//
// Both deps are interfaces so tests can stub them; the wiring
// layer holds the concrete *repository.LotRepo / *repository.MemoryRepo
// pair.
//
// Behaviour:
//
//   - Service is safe for concurrent use; the deps must be too
//     (repository.* types are).
//   - All methods are idempotent against re-runs: lessons are
//     deduped against the memory store before insert, so calling
//     RunAndPersist twice in the same trading day writes nothing
//     the second time.
//   - All errors are wrapped with context. The Service does NOT
//     swallow errors silently — the daily review hook decides
//     whether to abort or carry on.
type Service struct {
	lots    LotStatsRepo
	memory  MemoryWriter
	clock   func() time.Time
	options LessonOptions
}

// Option configures a Service constructor.
type Option func(*Service)

// WithClock replaces time.Now with a deterministic clock. Tests
// use it; production passes nothing and gets wall clock.
func WithClock(c func() time.Time) Option {
	return func(s *Service) { s.clock = c }
}

// WithLessonOptions overrides the default thresholds. Useful
// for backtests that want different sample-size floors.
func WithLessonOptions(o LessonOptions) Option {
	return func(s *Service) { s.options = o }
}

// NewService builds an attribution Service. Both deps are
// required — passing nil for either returns nil so the wiring
// layer can short-circuit cleanly.
func NewService(lots LotStatsRepo, memory MemoryWriter, opts ...Option) *Service {
	if lots == nil || memory == nil {
		return nil
	}
	s := &Service{
		lots:   lots,
		memory: memory,
		clock:  time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// BuildReport pulls the three stat slices from the LotRepo and
// folds them into an AttributionReport with no side effects.
// The HTTP layer's GET /api/funds/:id/attribution path calls
// this directly. days <= 0 falls back to DefaultLookbackDays.
//
// A clean separation from RunAndPersist lets the dashboard
// endpoint refresh without writing to memories every time the
// operator hits F5.
func (s *Service) BuildReport(ctx context.Context, fundID string, days int) (*AttributionReport, error) {
	if s == nil {
		return nil, errors.New("attribution: service nil")
	}
	trimmed := strings.TrimSpace(fundID)
	if trimmed == "" {
		return nil, errors.New("attribution: fund_id required")
	}
	if days <= 0 {
		days = DefaultLookbackDays
	}
	now := s.clock().UTC()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)

	bySleeve, err := s.lots.StatsBySleeve(ctx, trimmed, since)
	if err != nil {
		return nil, fmt.Errorf("attribution: stats by sleeve: %w", err)
	}
	byRegime, err := s.lots.StatsByRegime(ctx, trimmed, since)
	if err != nil {
		return nil, fmt.Errorf("attribution: stats by regime: %w", err)
	}
	bySleeveRegime, err := s.lots.StatsBySleeveRegime(ctx, trimmed, since)
	if err != nil {
		return nil, fmt.Errorf("attribution: stats by sleeve+regime: %w", err)
	}
	// Normalise + dedupe the cross-tab so downstream consumers
	// see stable labels regardless of how the SQL layer happens
	// to render NULLs.
	bySleeveRegime = SortedSleeveRegime(IndexSleeveRegime(bySleeveRegime))

	// Pull open-lot inventory so the insufficient_data lesson and
	// the dashboard can render "watching N positions since X"
	// instead of a generic empty state. Soft failure: a query
	// stall on the inventory row mustn't fail the whole report —
	// we degrade to zero-count and let the lesson generator fall
	// back to its old wording.
	openCount, earliest, invErr := s.lots.OpenLotInventory(ctx, trimmed)
	if invErr != nil {
		openCount, earliest = 0, sql.NullTime{}
	}

	return &AttributionReport{
		FundID:           trimmed,
		Window:           Window{Days: days, Since: since},
		GeneratedAt:      now,
		BySleeve:         bySleeve,
		ByRegime:         byRegime,
		BySleeveRegime:   bySleeveRegime,
		OpenLotCount:     openCount,
		EarliestOpenedAt: earliest,
	}, nil
}

// RunAndPersist is the daily entry point: build the report,
// generate lessons, persist any new ones to memories. Returns
// the report so the caller (typically the daily review hook)
// can log how many lessons fired without re-querying.
//
// `agentID` is optional — pass "" when the lesson is fund-wide
// rather than attributable to a single agent. The memory row's
// agent_id column accepts NULL.
func (s *Service) RunAndPersist(ctx context.Context, fundID, agentID string, days int) (*AttributionReport, []Lesson, error) {
	if s == nil {
		return nil, nil, errors.New("attribution: service nil")
	}
	report, err := s.BuildReport(ctx, fundID, days)
	if err != nil {
		return nil, nil, err
	}
	lessons := GenerateLessons(*report, s.options)
	if len(lessons) == 0 {
		return report, nil, nil
	}
	// Idempotency guard: load today's existing attribution
	// memories and skip lessons whose normalised tag-set we've
	// already persisted. Pulling a single page (the most
	// recent 200) is sufficient — daily volume is < 30 per
	// fund and we only need to compare against today.
	existing, _ := s.memory.ListByFund(ctx, fundID, MemoryLayer, 200)
	seen := indexExistingByTagKey(existing, report.GeneratedAt.UTC())

	tradingDate := dateOnly(report.GeneratedAt)
	persisted := make([]Lesson, 0, len(lessons))
	for _, l := range lessons {
		k := tagKey(l.Tags)
		if _, dup := seen[k]; dup {
			continue
		}
		mem := buildMemoryRow(fundID, agentID, tradingDate, l)
		if _, err := s.memory.Create(ctx, mem); err != nil {
			slog.Warn("attribution: failed to persist lesson",
				"fund_id", fundID,
				"kind", l.Kind,
				"err", err,
			)
			continue
		}
		seen[k] = struct{}{}
		persisted = append(persisted, l)
	}
	return report, persisted, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// indexExistingByTagKey collapses today's already-persisted
// attribution memories into the same hashable key the lesson
// generator emits. trading_date is matched on the date portion
// only so a re-run at 23:59 vs 00:01 doesn't double-write.
func indexExistingByTagKey(memories []repository.Memory, today time.Time) map[string]struct{} {
	out := make(map[string]struct{}, len(memories))
	today = dateOnly(today)
	for _, m := range memories {
		if !m.TradingDate.Valid || !sameDay(m.TradingDate.Time, today) {
			continue
		}
		out[tagKey(m.Tags)] = struct{}{}
	}
	return out
}

// tagKey is the canonical join of a lesson's tag set. We sort
// the tags first so insertion order doesn't change the hash.
func tagKey(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	sorted := append([]string(nil), tags...)
	sort.Strings(sorted)
	return strings.Join(sorted, "|")
}

// buildMemoryRow translates a Lesson into a repository.Memory.
// The mapping is mechanical: visibility=private (lessons are
// fund-internal) and sensitivity=internal (not user PII).
func buildMemoryRow(fundID, agentID string, tradingDate time.Time, l Lesson) *repository.Memory {
	m := &repository.Memory{
		FundID:      fundID,
		Layer:       MemoryLayer,
		Title:       sql.NullString{String: l.Title, Valid: l.Title != ""},
		Content:     l.Body,
		TradingDate: sql.NullTime{Time: tradingDate, Valid: !tradingDate.IsZero()},
		Tags:        append([]string(nil), l.Tags...),
		Visibility:  "private",
		Sensitivity: "internal",
		OriginKind:  "native",
	}
	if trimmed := strings.TrimSpace(agentID); trimmed != "" {
		m.AgentID = sql.NullString{String: trimmed, Valid: true}
	}
	return m
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}
