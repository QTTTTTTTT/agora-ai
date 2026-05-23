// Package cooldown surfaces per-symbol re-entry locks driven by
// the fund's own recent fill history.
//
// Why this exists. Without an explicit gate the PM is free to flip
// a symbol on consecutive sessions — buy on Monday, reduce on
// Tuesday, add back on Wednesday — which is the classic
// "over-trading" pattern Renaissance / DE Shaw / AQR explicitly
// suppress via per-instrument cooldowns. The justification is
// twofold: every fill incurs cost (commission, stamp tax,
// slippage); and the marginal information picked up by the model
// in a 24-hour window rarely justifies a fresh entry on the same
// name. A 24-hour soft lock after any fill is the cheapest signal
// that flushes the worst churn.
//
// Architecture. Sprint B #1 keeps cooldown advisory rather than
// hard-blocking: the service returns a list of "blocked" symbols
// with the rationale, and the PM prompt teaches the LLM to force
// action=watch on those names unless an extreme catalyst justifies
// override. This deliberately keeps the auto-execute gate out of
// the loop — the gate already has its own daily cumulative checks
// and we don't want a second blocker to silently veto a thesis the
// PM has reasoned through. If a future sprint wants hard blocking
// the gate can read the same Service.Lookup output.
//
// I/O contract. The Service depends on a thin *sql.DB read against
// trade_executions and is intentionally side-effect free; it never
// writes to the database. Empty fund_id, empty symbol list, or a
// nil DB all degrade to a nil result so the wiring layer can call
// it unconditionally.
package cooldown

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Lock is the per-symbol cooldown decision rendered into the
// DecisionInput.Cooldowns slice and surfaced to the PM prompt.
//
// All timestamps are UTC. The prompt is responsible for any
// per-locale display formatting.
type Lock struct {
	// Symbol is the upper-cased instrument symbol. Stable
	// identifier the PM prompt joins against
	// DecisionInput.Universe / .Positions.
	Symbol string

	// LastFillSide is the side of the fill that triggered the
	// lock — "buy" or "sell". Lets the prompt distinguish "we
	// just opened, stay put" from "we just took profits, don't
	// re-enter the same flip".
	LastFillSide string

	// LastFillAt is the executed_at timestamp of the triggering
	// fill (or created_at when executed_at is null on
	// simulation rows). UTC.
	LastFillAt time.Time

	// BlockedUntil is LastFillAt + Options.Window. When Now is
	// past this, the symbol is no longer in cooldown and the
	// Service drops it from the result.
	BlockedUntil time.Time

	// HoursSinceFill is a convenience the prompt rounds and
	// displays inline — "filled 8h ago" reads better to the LLM
	// than two RFC-3339 timestamps.
	HoursSinceFill float64

	// HoursRemaining counts down to BlockedUntil. Mirrors
	// HoursSinceFill so the prompt can render either side
	// without arithmetic.
	HoursRemaining float64
}

// Options configures the lookback window and the per-symbol
// cooldown duration. The zero value is fine; withDefaults fills in
// production tunings (LookbackDays=14 keeps the SQL small;
// Window=24h matches the lightest commit Renaissance documents in
// their public papers).
type Options struct {
	// LookbackDays is the upper bound on how far back the SQL
	// query reaches. 14 days is more than enough for a 24h
	// cooldown and leaves headroom for operators who want to
	// extend Window per-fund without rewriting the query.
	// Clamped to [1, 90].
	LookbackDays int

	// Window is the duration after a fill during which the
	// symbol is considered locked. Clamped to [1h, 30 days];
	// the default 24h matches the "one session per direction"
	// rule of thumb.
	Window time.Duration
}

func (o Options) withDefaults() Options {
	if o.LookbackDays <= 0 {
		o.LookbackDays = 14
	}
	if o.LookbackDays > 90 {
		o.LookbackDays = 90
	}
	if o.Window <= 0 {
		o.Window = 24 * time.Hour
	}
	if o.Window < time.Hour {
		o.Window = time.Hour
	}
	if o.Window > 30*24*time.Hour {
		o.Window = 30 * 24 * time.Hour
	}
	return o
}

// Service reads trade_executions for recent fills and emits the
// subset of (fund_id, symbol) pairs still inside the cooldown
// window. The struct is stateless apart from the configured
// Options, so callers can share a single instance fund-wide.
type Service struct {
	db   *sql.DB
	opts Options
}

// NewService is the only constructor. Passing a nil db produces a
// degenerate service whose Lookup is a no-op — useful for tests
// that wire a runtime without a database.
func NewService(db *sql.DB, opts Options) *Service {
	return &Service{db: db, opts: opts.withDefaults()}
}

// Options exposes the effective configuration for diagnostics.
func (s *Service) Options() Options {
	if s == nil {
		return Options{}.withDefaults()
	}
	return s.opts
}

// Lookup returns the symbols in `symbols` that have a fill within
// the cooldown window for `fundID` as of `now`. The result is sorted
// by remaining lock time descending so the prompt surface the
// tightest constraints first.
//
// Returns nil (no error) when:
//   - the service is nil or its db is nil
//   - fundID is blank
//   - symbols is empty after normalisation
//   - no recent fills match
//
// A SQL error bubbles up as an error and the wiring layer is
// expected to log it without aborting the PM run — cooldown is
// advisory, not load-bearing.
func (s *Service) Lookup(ctx context.Context, fundID string, symbols []string, now time.Time) ([]Lock, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	fundID = strings.TrimSpace(fundID)
	if fundID == "" {
		return nil, nil
	}
	wanted := normaliseSymbols(symbols)
	if len(wanted) == 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	since := now.Add(-time.Duration(s.opts.LookbackDays) * 24 * time.Hour)

	// One row per (symbol) with the most recent filled
	// fill: DISTINCT ON keeps the latest, COALESCE collapses
	// executed_at vs created_at so simulation rows (which leave
	// executed_at NULL on some paths) still surface.
	const q = `
		SELECT DISTINCT ON (symbol)
		       symbol, side, COALESCE(executed_at, created_at) AS at
		  FROM trade_executions
		 WHERE fund_id  = $1
		   AND status   = 'filled'
		   AND symbol   = ANY($2)
		   AND COALESCE(executed_at, created_at) >= $3
		 ORDER BY symbol, COALESCE(executed_at, created_at) DESC
	`
	rows, err := s.db.QueryContext(ctx, q, fundID, pq.Array(wanted), since)
	if err != nil {
		return nil, fmt.Errorf("cooldown: query trade_executions: %w", err)
	}
	defer rows.Close()

	locks := make([]Lock, 0, len(wanted))
	for rows.Next() {
		var (
			symbol string
			side   string
			at     time.Time
		)
		if err := rows.Scan(&symbol, &side, &at); err != nil {
			return nil, fmt.Errorf("cooldown: scan: %w", err)
		}
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		side = strings.ToLower(strings.TrimSpace(side))
		if symbol == "" {
			continue
		}
		at = at.UTC()
		blockedUntil := at.Add(s.opts.Window)
		if !blockedUntil.After(now) {
			continue // already expired
		}
		locks = append(locks, Lock{
			Symbol:         symbol,
			LastFillSide:   side,
			LastFillAt:     at,
			BlockedUntil:   blockedUntil,
			HoursSinceFill: now.Sub(at).Hours(),
			HoursRemaining: blockedUntil.Sub(now).Hours(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cooldown: rows: %w", err)
	}

	// Tightest-remaining-window symbols first; ties break by
	// alphabetic symbol so the prompt is deterministic.
	sort.SliceStable(locks, func(i, j int) bool {
		if locks[i].HoursRemaining == locks[j].HoursRemaining {
			return locks[i].Symbol < locks[j].Symbol
		}
		return locks[i].HoursRemaining > locks[j].HoursRemaining
	})
	return locks, nil
}

// normaliseSymbols upper-cases, trims and deduplicates the input
// so the SQL ANY() match is unambiguous. Empty entries are dropped.
func normaliseSymbols(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

