// paper_trading_public_repo.go — read-only queries that back the
// public Stage-4 track-record endpoint.
//
// Why a separate file from paper_trading_repo.go
//
//   The base repo's PaperPortfolioRow + ListPortfolios + GetPortfolio
//   speak the operator-facing column set (no methodology, no
//   inception_date, no public_track_record flag). The public
//   surface needs all three of those + a partial-index-friendly
//   WHERE clause. Keeping the public read path in its own file
//   makes the column-set explicit (a missed column == a compliance
//   leak) and means operator CRUD touches don't risk regressing
//   the marketing path.
//
//   Both files compile into the same `repository` package; from
//   the caller's POV this is the same *PaperTradingRepo, just with
//   one more pair of methods.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PublicPaperPortfolioRow embeds PaperPortfolioRow + the three
// migration-111 columns. Embedding (rather than a parallel struct)
// keeps the field set in sync — when the base row grows a column,
// the public row picks it up for free.
type PublicPaperPortfolioRow struct {
	PaperPortfolioRow
	PublicTrackRecord bool
	Methodology       string
	InceptionDate     sql.NullTime
}

// listPublicPaperPortfolioColumns is the canonical column list used
// by both LIST and GET so the two queries can never drift. Mirrors
// PaperPortfolioRow's fields + the three new columns from migration
// 111. Centralised so a future ADD COLUMN only needs one edit.
const listPublicPaperPortfolioColumns = `
	id, name, strategy, market, benchmark_symbol,
	initial_capital, current_nav, cash_balance,
	created_at, last_rebalance_at,
	public_track_record, methodology, inception_date
`

// ListPublicTrackRecord returns every portfolio currently flagged
// for public visibility, oldest-inception first. The order matters
// for the marketing surface: the longest-running strategies are
// the most credible to a first-time visitor.
func (r *PaperTradingRepo) ListPublicTrackRecord(ctx context.Context) ([]PublicPaperPortfolioRow, error) {
	q := `
		SELECT ` + listPublicPaperPortfolioColumns + `
		FROM paper_portfolios
		WHERE public_track_record = TRUE
		ORDER BY COALESCE(inception_date, created_at::date) ASC, created_at ASC
	`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list public paper portfolios: %w", err)
	}
	defer rows.Close()
	out := make([]PublicPaperPortfolioRow, 0, 32)
	for rows.Next() {
		row, err := scanPublicPaperPortfolio(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate public paper portfolios: %w", err)
	}
	return out, nil
}

// GetPublicTrackRecord fetches one portfolio by id. Unlike the
// LIST variant we do NOT filter on public_track_record here — the
// caller (service) gets the flag back and decides whether to
// surface ErrTrackRecordNotPublic. That way the same query also
// serves admin previews of "what would my public page look like
// if I flipped the flag".
//
// Returns (nil, nil) when the id is unknown — pure sql.ErrNoRows
// is squashed because the service treats "not found" and "not
// public" identically on the public surface.
func (r *PaperTradingRepo) GetPublicTrackRecord(ctx context.Context, portfolioID string) (*PublicPaperPortfolioRow, error) {
	if strings.TrimSpace(portfolioID) == "" {
		return nil, errors.New("portfolio id required")
	}
	q := `
		SELECT ` + listPublicPaperPortfolioColumns + `
		FROM paper_portfolios
		WHERE id = $1
	`
	row, err := scanPublicPaperPortfolio(r.db.QueryRowContext(ctx, q, portfolioID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// NavHistoryStats returns (count, last_snapshot_date) for one
// portfolio. Used by the public summary list so the cards can
// show "X data points, last updated YYYY-MM-DD" without paying
// the full nav-curve fetch cost.
//
// last is the zero time when count == 0; the caller is expected
// to nil-check via IsZero rather than a separate flag.
func (r *PaperTradingRepo) NavHistoryStats(ctx context.Context, portfolioID string) (count int, last time.Time, err error) {
	if strings.TrimSpace(portfolioID) == "" {
		return 0, time.Time{}, errors.New("portfolio id required")
	}
	const q = `
		SELECT COUNT(*)::INT, COALESCE(MAX(snapshot_date), '0001-01-01'::date)
		FROM paper_nav_history
		WHERE portfolio_id = $1
	`
	if err := r.db.QueryRowContext(ctx, q, portfolioID).Scan(&count, &last); err != nil {
		return 0, time.Time{}, fmt.Errorf("nav history stats: %w", err)
	}
	if last.Year() < 2 {
		// Sentinel sweep — the COALESCE above maps "no rows" to
		// year 1; map back to zero-time so the caller's IsZero
		// branch lights up.
		return count, time.Time{}, nil
	}
	return count, last, nil
}

// scanPublicPaperPortfolio works against both *sql.Row and *sql.Rows.
// The shared interface that both expose is Scan(...) error — we
// reuse the rowScanner local interface declared next door in
// backtest_repo.go so the same scan code serves LIST and GET.
// Saves a copy-paste of the long Scan call.

func scanPublicPaperPortfolio(r rowScanner) (PublicPaperPortfolioRow, error) {
	var p PublicPaperPortfolioRow
	if err := r.Scan(
		&p.ID, &p.Name, &p.Strategy, &p.Market, &p.BenchmarkSymbol,
		&p.InitialCapital, &p.CurrentNav, &p.CashBalance,
		&p.CreatedAt, &p.LastRebalanceAt,
		&p.PublicTrackRecord, &p.Methodology, &p.InceptionDate,
	); err != nil {
		return PublicPaperPortfolioRow{}, err
	}
	return p, nil
}
