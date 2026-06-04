// Package attribution turns the closed_lots ledger into actionable
// insight. It's the read side of Phase 3A-5: the strategy sleeves
// (Phase 3A-4) and the lot ledger (Phase 3A-1) write structured
// per-trade outcomes; attribution slices those outcomes by sleeve,
// regime, signal source, and exit reason, then derives "lessons"
// — short, deterministic statements like "trend sleeve in
// regime=range has lost money for 12 consecutive lots; recommend
// disabling".
//
// Three layers:
//
//  1. Aggregator (pure). Takes SleeveRegimeStat rows from
//     repository.LotRepo and folds them into AttributionReport.
//     No I/O; trivially testable.
//
//  2. Lesson generator (pure). Walks the report, applies a
//     short list of statistical thresholds, and emits Lesson
//     records. Each Lesson is fully deterministic — same input
//     produces the same output, which keeps the daily lesson
//     stream idempotent against re-runs.
//
//  3. Service. Wires the two pure layers to LotRepo +
//     MemoryRepo so the daily review hook can call one method:
//     Service.RunAndPersist(fundID).
//
// What this package deliberately does NOT do:
//
//   - Decide which sleeves to disable. We emit advisory Lessons;
//     the strategy.Service consumes them in a future PR. The
//     separation keeps attribution honest: it can flag patterns
//     even when we choose not to act on them.
//   - Run the LLM. Lesson text is template-derived, no
//     generative content. This makes attribution cheap enough
//     to run every fund every day with zero LLM cost.
//   - Compute Sharpe / drawdown / etc. Those are bigger
//     numerics that belong in a separate analytics layer; this
//     PR focuses on the count/win-rate/total-pnl basics that
//     the lesson generator actually uses.
package attribution

import (
	"context"
	"database/sql"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Reports
// ---------------------------------------------------------------------------

// AttributionReport is the structured view the API surfaces and
// the lesson generator consumes. It's the union of the three
// stat slices the LotRepo can produce — single-axis (sleeve,
// regime) and the cross-tab (sleeve × regime). The cross-tab
// is the most useful slice for human readers; the single-axis
// rollups exist for quick totals on dashboards.
//
// Window is the closed-at window applied when the report was
// generated. GeneratedAt is set by the Service so dashboards
// can show "as of X minutes ago" freshness.
type AttributionReport struct {
	FundID       string
	Window       Window
	GeneratedAt  time.Time
	BySleeve     []repository.SleeveStat
	ByRegime     []repository.RegimeStat
	BySleeveRegime []repository.SleeveRegimeStat

	// OpenLotCount + EarliestOpenedAt make the "insufficient
	// data" lesson concrete instead of generic. When the fund
	// has no closed lots yet (Window has nothing in
	// BySleeve*/ByRegime*), the lesson generator surfaces these
	// two numbers so the operator reads "7 lots opened since
	// 2026-05-12, waiting for the first closed roundtrip" —
	// proof that attribution IS running, just waiting on
	// realised P&L. Without these, the dashboard's
	// "insufficient_data" lesson reads like the agent stopped
	// working, when in fact it's actively tracking positions.
	OpenLotCount      int
	EarliestOpenedAt  sql.NullTime
}

// Window describes the closed-at filter used to build the report.
// Days is the operator-facing label; Since is the absolute
// timestamp passed to the SQL layer.
type Window struct {
	Days  int
	Since time.Time
}

// HasData reports whether the report has anything meaningful in
// it — handy for the lesson generator and the API to short-
// circuit "no closed trades yet" responses without spelling out
// the same `len(x) > 0` chains everywhere.
func (r AttributionReport) HasData() bool {
	return len(r.BySleeve) > 0 || len(r.ByRegime) > 0 || len(r.BySleeveRegime) > 0
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

// DefaultLookbackDays is the report window when callers don't
// specify one. 30 calendar days is enough for ~20 A-share
// trading sessions or a full month of crypto data — a sweet
// spot between "too noisy for thresholds" and "stale".
const DefaultLookbackDays = 30

// DefaultMinSampleSize is the smallest closed-lot count a
// (sleeve, regime) cell needs before the lesson generator will
// fire on it. Below this threshold the variance is too high to
// distinguish "losing strategy" from "unlucky streak".
const DefaultMinSampleSize = 5

// DefaultMaxLessonsPerRun caps how many lessons one Service run
// can emit per fund. Without the cap a pathological config
// could spam the memory store with hundreds of low-quality
// observations on day one of deployment; with the cap we surface
// the worst offenders first and revisit the rest on the next
// daily run.
const DefaultMaxLessonsPerRun = 20

// ---------------------------------------------------------------------------
// Lesson types
// ---------------------------------------------------------------------------

// Severity orders Lesson urgency. Critical = "do this now",
// warning = "noteworthy", info = "tracked for the record".
// The Service writes everything to the memory store but the API
// can filter by severity for the dashboard's banner area.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// LessonKind classifies the pattern that triggered the Lesson.
// Used as the primary key fragment for idempotency: the Service
// dedupes (Kind, Tags) before insert so a re-run of the same
// trading day doesn't double-write.
type LessonKind string

const (
	// LessonSleeveRegimeLoser is the LEGACY single-tier "you're
	// losing money" lesson. Retained as a constant so old memory
	// rows whose template_key is "attribution.lesson.sleeve_regime_loser"
	// keep rendering against the i18n dictionary; new runs DO NOT
	// emit it — they pick one of the three tiered kinds below.
	LessonSleeveRegimeLoser LessonKind = "sleeve_regime_loser"

	// LessonSleeveRegimeObserving (severity=info) fires for small
	// samples (5–10 closed lots) with a sub-par win rate. The
	// advice is "track only" — variance is too high to recommend
	// any portfolio change. This is the lesson a real research
	// team would write in a journal, not a directive.
	LessonSleeveRegimeObserving LessonKind = "sleeve_regime_observing"

	// LessonSleeveRegimeThrottle (severity=warning) fires once the
	// sample reaches 10–30 closed lots and the win rate stays
	// below ThrottleWinRateMax. The advice is "reduce sizing /
	// raise confidence threshold / try a shorter-horizon variant"
	// — real-team-style risk management, not a kill switch.
	LessonSleeveRegimeThrottle LessonKind = "sleeve_regime_throttle"

	// LessonSleeveRegimePause (severity=critical) fires only when
	// the sample is large enough (30+ closed lots) AND the win
	// rate is well below PauseWinRateMax AND cumulative P&L is
	// negative. This is the only tier that recommends actually
	// pausing the (sleeve, regime) combination — by that sample
	// size the signal is statistically meaningful.
	LessonSleeveRegimePause LessonKind = "sleeve_regime_pause"

	// LessonSleeveRegimeWinner is the mirror: at least
	// MinSampleSize trades, win-rate above 65%, AND total P&L
	// positive. Surfaces strategies the LLM PM might want to
	// scale into.
	LessonSleeveRegimeWinner LessonKind = "sleeve_regime_winner"

	// LessonSleeveOverall is the fallback we emit when the
	// regime detector failed to classify any of a sleeve's
	// closed lots (every BySleeveRegime row for that sleeve
	// has regime="" / "unspecified"). Per-regime lessons are
	// skipped in that case because a per-regime claim against
	// a placeholder label would be misleading — but staying
	// silent is worse: the fund's sleeve IS losing money, the
	// operator just doesn't know it because the regime detector
	// hasn't been wired up yet. This lesson surfaces the
	// sleeve-wide rollup with body text that explicitly says
	// "regime detector unavailable, here's the overall picture
	// — calibrate the detector before drawing per-regime
	// conclusions". Severity=warning; we don't recommend
	// pausing without a regime breakdown.
	LessonSleeveOverall LessonKind = "sleeve_overall"

	// LessonInsufficientData fires once per Service run if the
	// fund has zero closed lots in the window. Lets the
	// dashboard distinguish "no data yet" from "data exists,
	// nothing notable". One lesson per (fund, window) — not
	// repeated.
	LessonInsufficientData LessonKind = "insufficient_data"
)

// Lesson is the deterministic output of the lesson generator.
// It carries enough context for downstream consumers (the
// memory store, the LLM PM context builder, the dashboard) to
// understand WHY the lesson exists without re-running the
// aggregation.
//
// Tags are normalised to lower-case "key:value" pairs so a
// future query layer can filter by "sleeve:trend" or
// "regime:range" without re-parsing free text.
type Lesson struct {
	Kind     LessonKind
	Severity Severity
	Title    string
	Body     string
	Tags     []string

	Sleeve         string
	Regime         string
	TradeCount     int
	WinRate        float64
	TotalPnL       float64
	AvgPnLPct      float64
	AvgHoldingDays float64

	// TemplateKey + Payload are the i18n render contract (migration
	// 085). buildXxxLesson sets both alongside the legacy English
	// Title / Body so:
	//   * Old replays / API consumers that only read Title+Body keep
	//     working unchanged.
	//   * New UIs render TemplateKey via shared/api-client/src/i18n.ts
	//     against the user's locale, interpolating Payload with
	//     Intl.NumberFormat. Title+Body become a fallback for the
	//     "template not in dictionary yet" case.
	//
	// TemplateKey is "" for any Lesson that the i18n pipeline does
	// not (yet) cover — those rows are persisted with NULL columns
	// and the UI falls back to Content. We keep this opt-in instead
	// of an enum-of-required-keys so adding a future Lesson type
	// doesn't ripple through five files just to deploy.
	TemplateKey string
	// Payload is template-specific. Each LessonKind has its own
	// schema documented next to its build* helper. The map values
	// are deliberately stored as their native Go types (int, float,
	// string) — buildMemoryRow marshals them to JSON once at write
	// time. Nil = no payload.
	Payload map[string]any
}

// ---------------------------------------------------------------------------
// LotRepo dependency surface
// ---------------------------------------------------------------------------

// LotStatsRepo is the narrow interface the attribution Service
// needs from repository.LotRepo. Defining it locally lets tests
// stub the three queries without spinning up a database.
//
// Production implementation: *repository.LotRepo satisfies it
// directly via the methods added in PR-3A-1 + this PR.
type LotStatsRepo interface {
	StatsBySleeve(ctx context.Context, fundID string, since time.Time) ([]repository.SleeveStat, error)
	StatsByRegime(ctx context.Context, fundID string, since time.Time) ([]repository.RegimeStat, error)
	StatsBySleeveRegime(ctx context.Context, fundID string, since time.Time) ([]repository.SleeveRegimeStat, error)
	// OpenLotInventory returns the count + earliest opened_at of
	// still-open lots. Used to enrich the insufficient_data
	// lesson so users see what the agent is currently watching.
	OpenLotInventory(ctx context.Context, fundID string) (int, sql.NullTime, error)
}

// MemoryWriter is the narrow Create-only interface the Service
// uses for lesson persistence. We never read memories back
// from attribution; the existing /api/funds/:id/reflections
// pipeline does that.
type MemoryWriter interface {
	Create(ctx context.Context, m *repository.Memory) (string, error)
	ListByFund(ctx context.Context, fundID, layer string, limit int) ([]repository.Memory, error)
}
