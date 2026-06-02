// fx_repo.go — DB-backed FX rate store (P1-4).

package fx

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrRateNotFound is returned when no row matches the lookup.
// Callers (NAV) usually fall through to a configured default
// (1.0 if the conversion is base→base; otherwise propagate).
var ErrRateNotFound = errors.New("fx: rate not found in store")

// Repo wraps the fx_rates table. Stateless aside from the *sql.DB
// handle, so it's safe to share a singleton across handlers.
type Repo struct {
	db *sql.DB
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// UpsertParams is the input for Insert.
type UpsertParams struct {
	Base      string
	Quote     string
	Rate      float64
	RateAt    time.Time
	Source    string
	CreatedBy string
	Metadata  map[string]any
}

// Upsert writes one rate row. The (base, quote, rate_at, source)
// uniqueness lets us re-run a fetch without exploding the table —
// duplicates collapse on conflict.
//
// Validation: rate must be > 0, currencies must be supported and
// distinct, source must be in the closed vocabulary.
func (r *Repo) Upsert(ctx context.Context, p UpsertParams) (string, error) {
	if r == nil || r.db == nil {
		return "", fmt.Errorf("fx_repo: nil db")
	}
	base := canonicalCurrency(p.Base)
	quote := canonicalCurrency(p.Quote)
	if base == "" || quote == "" {
		return "", fmt.Errorf("fx_repo: empty currency")
	}
	if base == quote {
		return "", fmt.Errorf("fx_repo: base == quote (%s)", base)
	}
	if !IsSupported(base) || !IsSupported(quote) {
		return "", fmt.Errorf("fx_repo: unsupported pair %s/%s", base, quote)
	}
	if p.Rate <= 0 {
		return "", fmt.Errorf("fx_repo: non-positive rate %g", p.Rate)
	}
	source := strings.ToLower(strings.TrimSpace(p.Source))
	if source == "" {
		source = "manual"
	}
	switch source {
	case "yahoo", "manual", "eod", "override":
	default:
		return "", fmt.Errorf("fx_repo: invalid source %q", source)
	}
	rateAt := p.RateAt
	if rateAt.IsZero() {
		rateAt = time.Now().UTC()
	}
	var metadataBytes []byte
	if len(p.Metadata) > 0 {
		b, err := json.Marshal(p.Metadata)
		if err != nil {
			return "", fmt.Errorf("fx_repo: marshal metadata: %w", err)
		}
		metadataBytes = b
	}

	var id string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO fx_rates
		    (base_currency, quote_currency, rate, rate_at, source,
		     created_by, metadata)
		 VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid,
		         COALESCE($7::jsonb, '{}'::jsonb))
		 ON CONFLICT (base_currency, quote_currency, rate_at, source)
		   DO UPDATE SET rate = EXCLUDED.rate,
		                 metadata = EXCLUDED.metadata
		 RETURNING id`,
		base, quote, p.Rate, rateAt.UTC(), source, p.CreatedBy, metadataBytes,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("fx_repo: upsert: %w", err)
	}
	return id, nil
}

// Latest returns the most recent rate for a directly-stored pair.
// Sources are preferred in the order: manual > override > yahoo
// > eod, so an operator's manual correction always wins. The
// preference list is encoded directly in the ORDER BY rather
// than relying on Postgres CASE because the cardinality is small
// and the explicit form makes the intent obvious.
//
// Returns ErrRateNotFound when no row matches.
func (r *Repo) Latest(ctx context.Context, base, quote string) (*Rate, error) {
	return r.AsOf(ctx, base, quote, time.Now().UTC())
}

// AsOf returns the most recent rate at or before `at`. Used by
// historical NAV reconstruction so a rerun produces the same
// numbers regardless of how many new rates were ingested between
// snapshot writes.
func (r *Repo) AsOf(ctx context.Context, base, quote string, at time.Time) (*Rate, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("fx_repo: nil db")
	}
	b := canonicalCurrency(base)
	q := canonicalCurrency(quote)
	if SameCurrency(b, q) {
		return &Rate{Base: b, Quote: q, Rate: 1.0, RateAt: at, Source: "identity"}, nil
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT base_currency, quote_currency, rate, rate_at, source
		   FROM fx_rates
		  WHERE base_currency = $1
		    AND quote_currency = $2
		    AND rate_at <= $3
		  ORDER BY
		      CASE source
		          WHEN 'manual'   THEN 0
		          WHEN 'override' THEN 1
		          WHEN 'yahoo'    THEN 2
		          WHEN 'eod'      THEN 3
		          ELSE 9
		      END,
		      rate_at DESC
		  LIMIT 1`,
		b, q, at.UTC(),
	)
	var out Rate
	if err := row.Scan(&out.Base, &out.Quote, &out.Rate, &out.RateAt, &out.Source); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRateNotFound
		}
		return nil, fmt.Errorf("fx_repo: as_of: %w", err)
	}
	return &out, nil
}

// Convert returns `amount` re-denominated from `from` into `to`,
// triangulating through USD when needed. Used by the NAV
// aggregator + cash_ledger summary.
//
// Triangulation rules:
//   - from == to                : amount, identity rate, no-op
//   - from == USD or to == USD  : single AsOf lookup
//   - else                      : two AsOf lookups (USD/from,
//                                  USD/to) and a divide
//
// Returns ErrRateNotFound when any leg is missing — callers can
// decide whether to fall back to a stale rate, mark the snapshot
// "FX-stale", or fail loudly.
func (r *Repo) Convert(ctx context.Context, amount float64, from, to string, at time.Time) (float64, *Rate, error) {
	from = canonicalCurrency(from)
	to = canonicalCurrency(to)
	if SameCurrency(from, to) {
		return amount, &Rate{Base: from, Quote: to, Rate: 1.0, RateAt: at, Source: "identity"}, nil
	}
	// Direct lookup first.
	if direct, err := r.AsOf(ctx, from, to, at); err == nil {
		return amount * direct.Rate, direct, nil
	}
	// Direct missing — triangulate through USD.
	usdToFrom, errA := r.AsOf(ctx, AnchorCurrency, from, at)
	usdToTo, errB := r.AsOf(ctx, AnchorCurrency, to, at)
	if errA != nil || errB != nil {
		// Try the inverse direction (some sources only store one
		// side, e.g. CNY/USD instead of USD/CNY).
		fromToUsd, errC := r.AsOf(ctx, from, AnchorCurrency, at)
		toToUsd, errD := r.AsOf(ctx, to, AnchorCurrency, at)
		if errC == nil && errD == nil {
			r1 := 1.0 / fromToUsd.Rate
			r2 := 1.0 / toToUsd.Rate
			synth := r2 / r1
			return amount * synth, &Rate{
				Base:   from,
				Quote:  to,
				Rate:   synth,
				RateAt: olderOf(fromToUsd.RateAt, toToUsd.RateAt),
				Source: "triangulated_inverse",
			}, nil
		}
		return 0, nil, ErrRateNotFound
	}
	rate, ok := computeTriangulated(usdToFrom, usdToTo)
	if !ok {
		return 0, nil, ErrRateNotFound
	}
	return amount * rate, &Rate{
		Base:   from,
		Quote:  to,
		Rate:   rate,
		RateAt: olderOf(usdToFrom.RateAt, usdToTo.RateAt),
		Source: "triangulated",
	}, nil
}

func olderOf(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// ListRecentParams scopes the admin-page list.
type ListRecentParams struct {
	Limit int
	Pair  string // optional, "USD/CNY"
}

// ListRecent returns the latest N rows, newest first. Used by the
// admin FX page to render the "what did we last fetch" panel.
func (r *Repo) ListRecent(ctx context.Context, p ListRecentParams) ([]Rate, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("fx_repo: nil db")
	}
	limit := p.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT base_currency, quote_currency, rate, rate_at, source
	         FROM fx_rates`
	args := []any{}
	if pair := strings.TrimSpace(p.Pair); pair != "" {
		parts := strings.Split(pair, "/")
		if len(parts) == 2 {
			q += ` WHERE base_currency = $1 AND quote_currency = $2`
			args = append(args, canonicalCurrency(parts[0]), canonicalCurrency(parts[1]))
		}
	}
	q += fmt.Sprintf(` ORDER BY rate_at DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fx_repo: list_recent: %w", err)
	}
	defer rows.Close()
	out := make([]Rate, 0, limit)
	for rows.Next() {
		var rt Rate
		if err := rows.Scan(&rt.Base, &rt.Quote, &rt.Rate, &rt.RateAt, &rt.Source); err != nil {
			return nil, fmt.Errorf("fx_repo: scan: %w", err)
		}
		out = append(out, rt)
	}
	return out, rows.Err()
}
