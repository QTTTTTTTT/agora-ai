// public_track_record.go — Stage-4 SEC Publisher's-Exclusion compliant
// public track record for paper portfolios.
//
// Why this file is separate from papertrading.go
//
//   The CRUD service (CreatePortfolio / ProposeOrder / SnapshotNAV)
//   is the operator-facing surface and stays auth-gated. The
//   "public" track record is the marketing-facing surface that
//   anonymous prospects can hit BEFORE they sign up. Splitting it
//   into its own file lets us:
//
//     - keep the public path's column-set explicit (it must include
//       the three new opt-in columns from migration 111 — anything
//       else is a compliance leak); and
//
//     - centralise the disclosure block + the performance-metric
//       maths in one auditable place, so legal can review a single
//       handler without trawling through CRUD code.
//
// SEC Publisher's-Exclusion checklist this implementation honours
//
//   1. Identical data for every viewer (no personalisation, no
//      auth-derived filtering).
//   2. Disclosed methodology (the methodology column is mandatory
//      and rendered verbatim — empty string is treated as a config
//      error and the row is skipped from /list).
//   3. Past-performance disclaimer attached to every payload.
//   4. "Not investment advice" disclaimer.
//   5. Generation timestamp so cached / mirrored copies can show
//      staleness.

package papertrading

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ErrTrackRecordNotPublic is returned by GetPublicTrackRecord when
// the portfolio exists but is not flagged for public display. The
// HTTP handler maps this to 404 (we deliberately do NOT 403 — that
// would leak that the ID exists internally).
var ErrTrackRecordNotPublic = errors.New("papertrading: portfolio not flagged for public track record")

// PublicTrackRecordSummary is the wire shape returned by the
// /api/papertrading/public/track-record LIST endpoint. Trimmed down
// to the data needed to render a card-grid: enough to drive a list
// of "strategies you can browse" without paying the cost of the
// full nav curve.
type PublicTrackRecordSummary struct {
	PortfolioID      string     `json:"portfolioId"`
	Name             string     `json:"name"`
	Strategy         string     `json:"strategy"`
	Market           string     `json:"market"`
	BenchmarkSymbol  string     `json:"benchmarkSymbol,omitempty"`
	InceptionDate    *time.Time `json:"inceptionDate,omitempty"`
	InitialCapital   float64    `json:"initialCapital"`
	CurrentNav       float64    `json:"currentNav"`
	CumulativeReturn float64    `json:"cumulativeReturn"`
	DataPoints       int        `json:"dataPoints"`
	LastNavDate      *time.Time `json:"lastNavDate,omitempty"`
}

// PublicTrackRecord is the DETAIL shape returned by the
// /api/papertrading/public/track-record/{id} endpoint. Carries
// everything a prospect needs to evaluate the strategy: header,
// nav curve, derived metrics, methodology body, and the canonical
// disclosure block.
type PublicTrackRecord struct {
	Summary     PublicTrackRecordSummary `json:"summary"`
	Methodology string                   `json:"methodology"`
	NavHistory  []PaperNavPoint          `json:"navHistory"`
	Metrics     PerformanceMetrics       `json:"metrics"`
	Disclosure  ComplianceDisclosure     `json:"disclosure"`
}

// PaperNavPoint is the per-day nav point on the public curve.
// Mirrors repository.PaperNavRow but flattens nullable fields into
// plain floats / dates so the JSON shape is stable for chart libs.
type PaperNavPoint struct {
	Date         time.Time `json:"date"`
	Nav          float64   `json:"nav"`
	DailyReturn  float64   `json:"dailyReturn"`
	BenchmarkNav float64   `json:"benchmarkNav,omitempty"`
}

// PerformanceMetrics are the standard derived numbers a track
// record advertises. All computations are pure functions of the
// nav series — no benchmark / risk-free rate assumed beyond the
// constants noted on each field's comment.
type PerformanceMetrics struct {
	// CumulativeReturn = (currentNav - initialCapital) / initialCapital.
	CumulativeReturn float64 `json:"cumulativeReturn"`
	// AnnualizedReturn = (1+cumulative)^(252/days) - 1. Uses 252
	// trading days to align with US-equity convention; readers
	// outside that universe should treat it as approximate.
	AnnualizedReturn float64 `json:"annualizedReturn"`
	// MaxDrawdown = max peak-to-trough drop on the nav curve.
	// Reported as a positive number (0.15 == 15% drawdown).
	MaxDrawdown float64 `json:"maxDrawdown"`
	// Volatility = stdev(daily_return) * sqrt(252).
	Volatility float64 `json:"volatility"`
	// Sharpe = (mean(daily_return) * 252) / Volatility. Risk-free
	// rate assumed zero — call it "excess return / volatility" if
	// you prefer. Returns 0 when Volatility == 0.
	Sharpe float64 `json:"sharpe"`
	// BestDay / WorstDay are the single largest daily moves.
	BestDay  float64 `json:"bestDay"`
	WorstDay float64 `json:"worstDay"`
	// PositiveDayRatio = positiveDays / totalDays. UI-friendly
	// summary of how often the strategy was up.
	PositiveDayRatio float64 `json:"positiveDayRatio"`
	// SampleSize is the number of daily-return observations used
	// for the metrics. Surface so the consumer can warn when the
	// track record is too short for the figures to mean anything.
	SampleSize int `json:"sampleSize"`
}

// ComplianceDisclosure is the canonical block every public track
// record carries. The Statements slice is intentionally a slice (not
// a single string) so a frontend can render each paragraph in its
// own visual block; clients that need a plain blob can join with
// "\n\n".
type ComplianceDisclosure struct {
	Statements   []string  `json:"statements"`
	GeneratedAt  time.Time `json:"generatedAt"`
	RuleCitation string    `json:"ruleCitation"`
}

// publicDisclosureStatements is the fixed disclosure block.
// Reviewed by the compliance team; do NOT mutate without legal
// sign-off — the wording is what carves out the Publisher's
// Exclusion + Marketing Rule safe harbour.
var publicDisclosureStatements = []string{
	"This page presents the historical net asset value of a hypothetical paper portfolio managed by an automated strategy. No real money was traded; figures are derived from publicly available market data and the strategy's published methodology.",
	"This page is for informational and educational purposes only and does NOT constitute investment advice, a recommendation to buy or sell any security, or an offer of advisory services. The operator is not a registered investment adviser.",
	"Past performance is not indicative of future results. Hypothetical performance has inherent limitations: trades were not actually executed, results may not reflect the impact of material economic and market factors, and the strategy was designed with the benefit of hindsight.",
	"The performance figures shown are identical for every viewer — no personalisation, no individualised advice is provided. Methodology is disclosed below in full.",
}

// publicDisclosureCitation points readers at the legal hook so
// auditors can trace why the disclosure body says what it says.
const publicDisclosureCitation = "SEC Investment Advisers Act of 1940 §202(a)(11)(D) Publisher's Exclusion; SEC Marketing Rule 206(4)-1"

// ListPublicTrackRecord returns the summary view of every portfolio
// flagged public_track_record=TRUE. Rows with empty methodology are
// skipped — the disclosure-completeness gate runs HERE, not in the
// admin write path, so an operator can stage a row, fill in the
// methodology later, and only then the row becomes externally
// visible (no silent half-state).
func (s *Service) ListPublicTrackRecord(ctx context.Context) ([]*PublicTrackRecordSummary, error) {
	if s == nil {
		return nil, ErrServiceUnconfigured
	}
	rows, err := s.repo.ListPublicTrackRecord(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*PublicTrackRecordSummary, 0, len(rows))
	for _, row := range rows {
		if row.Methodology == "" {
			continue
		}
		summary, err := s.buildSummary(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, nil
}

// GetPublicTrackRecord returns the detail view for one portfolio.
// Returns ErrTrackRecordNotPublic when the portfolio exists but the
// public flag is FALSE; also returns it when methodology is empty
// (the disclosure-incomplete state is functionally the same as "not
// public" from the prospect's POV).
//
// Returns (nil, nil) for a totally unknown ID — handlers should map
// that to 404 too. The two cases produce the same response shape on
// purpose so probing for the existence of an internal ID through
// the public surface yields no signal.
func (s *Service) GetPublicTrackRecord(ctx context.Context, portfolioID string) (*PublicTrackRecord, error) {
	if s == nil {
		return nil, ErrServiceUnconfigured
	}
	row, err := s.repo.GetPublicTrackRecord(ctx, portfolioID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	if !row.PublicTrackRecord || row.Methodology == "" {
		return nil, ErrTrackRecordNotPublic
	}
	summary, err := s.buildSummary(ctx, *row)
	if err != nil {
		return nil, err
	}
	navRows, err := s.repo.NavHistory(ctx, portfolioID, 0)
	if err != nil {
		return nil, err
	}
	points := make([]PaperNavPoint, 0, len(navRows))
	for _, r := range navRows {
		p := PaperNavPoint{
			Date: r.SnapshotDate,
			Nav:  r.Nav,
		}
		if r.DailyReturn.Valid {
			p.DailyReturn = r.DailyReturn.Float64
		}
		if r.BenchmarkNav.Valid {
			p.BenchmarkNav = r.BenchmarkNav.Float64
		}
		points = append(points, p)
	}
	metrics := computePerformanceMetrics(row.InitialCapital, navRows)
	return &PublicTrackRecord{
		Summary:     *summary,
		Methodology: row.Methodology,
		NavHistory:  points,
		Metrics:     metrics,
		Disclosure: ComplianceDisclosure{
			Statements:   publicDisclosureStatements,
			GeneratedAt:  s.now().UTC(),
			RuleCitation: publicDisclosureCitation,
		},
	}, nil
}

// buildSummary materialises the list/header card from a repo row.
// We re-query nav history with limit=2 just to find the most recent
// snapshot date — cheap and lets the list endpoint show "last
// updated" without pulling the full curve.
func (s *Service) buildSummary(ctx context.Context, row repository.PublicPaperPortfolioRow) (*PublicTrackRecordSummary, error) {
	cumReturn := 0.0
	if row.InitialCapital > 0 {
		cumReturn = (row.CurrentNav - row.InitialCapital) / row.InitialCapital
	}
	summary := &PublicTrackRecordSummary{
		PortfolioID:      row.ID,
		Name:             row.Name,
		Strategy:         row.Strategy,
		Market:           row.Market,
		InitialCapital:   row.InitialCapital,
		CurrentNav:       row.CurrentNav,
		CumulativeReturn: cumReturn,
	}
	if row.BenchmarkSymbol.Valid {
		summary.BenchmarkSymbol = row.BenchmarkSymbol.String
	}
	if row.InceptionDate.Valid {
		t := row.InceptionDate.Time
		summary.InceptionDate = &t
	}
	// Count data points + last date with one small query rather
	// than pulling the full nav curve. The limit=1 ORDER BY DESC
	// returns the newest row first; we still need the COUNT so we
	// query both via the repo's dedicated helper.
	count, last, err := s.repo.NavHistoryStats(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	summary.DataPoints = count
	if !last.IsZero() {
		t := last
		summary.LastNavDate = &t
	}
	return summary, nil
}

// computePerformanceMetrics is the pure-function maths called by
// GetPublicTrackRecord. Split out so it's unit-testable without a
// DB; the caller hands in initial capital + the raw nav rows in
// ASCENDING date order (which is what the repo returns).
func computePerformanceMetrics(initialCapital float64, navRows []repository.PaperNavRow) PerformanceMetrics {
	out := PerformanceMetrics{}
	if len(navRows) == 0 || initialCapital <= 0 {
		return out
	}
	// Defensively sort by date in case the caller hands in
	// out-of-order rows — keeps the drawdown calc honest.
	sort.Slice(navRows, func(i, j int) bool {
		return navRows[i].SnapshotDate.Before(navRows[j].SnapshotDate)
	})

	lastNav := navRows[len(navRows)-1].Nav
	out.CumulativeReturn = (lastNav - initialCapital) / initialCapital

	// Daily returns: prefer the persisted daily_return column when
	// available, fall back to a computed (nav[i] / nav[i-1] - 1)
	// when the column is NULL (old rows pre-fix-up).
	returns := make([]float64, 0, len(navRows))
	for i, r := range navRows {
		var dr float64
		if r.DailyReturn.Valid {
			dr = r.DailyReturn.Float64
		} else if i > 0 && navRows[i-1].Nav > 0 {
			dr = r.Nav/navRows[i-1].Nav - 1
		} else {
			continue
		}
		returns = append(returns, dr)
	}
	out.SampleSize = len(returns)

	if len(returns) > 0 {
		// Mean
		sum := 0.0
		positive := 0
		out.BestDay = returns[0]
		out.WorstDay = returns[0]
		for _, dr := range returns {
			sum += dr
			if dr > 0 {
				positive++
			}
			if dr > out.BestDay {
				out.BestDay = dr
			}
			if dr < out.WorstDay {
				out.WorstDay = dr
			}
		}
		mean := sum / float64(len(returns))
		out.PositiveDayRatio = float64(positive) / float64(len(returns))

		// Stdev (population) — call sites use this for vol/sharpe,
		// so the choice of N vs N-1 is consistent. N-1 would
		// inflate vol slightly for short series; we accept N for
		// simplicity.
		ss := 0.0
		for _, dr := range returns {
			diff := dr - mean
			ss += diff * diff
		}
		stdev := math.Sqrt(ss / float64(len(returns)))

		const tradingDaysPerYear = 252.0
		out.Volatility = stdev * math.Sqrt(tradingDaysPerYear)
		if out.Volatility > 0 {
			out.Sharpe = (mean * tradingDaysPerYear) / out.Volatility
		}

		// Annualised return — use compounded growth scaled to the
		// number of trading days observed. Falls back to the simple
		// cumulative number when the series is too short for the
		// power to be stable.
		days := float64(len(returns))
		if days >= tradingDaysPerYear/4 { // need at least ~63 days
			out.AnnualizedReturn = math.Pow(1+out.CumulativeReturn, tradingDaysPerYear/days) - 1
		} else {
			out.AnnualizedReturn = out.CumulativeReturn
		}
	}

	// Max drawdown — walk the nav curve, track running peak.
	peak := navRows[0].Nav
	maxDD := 0.0
	for _, r := range navRows {
		if r.Nav > peak {
			peak = r.Nav
		}
		if peak > 0 {
			dd := (peak - r.Nav) / peak
			if dd > maxDD {
				maxDD = dd
			}
		}
	}
	out.MaxDrawdown = maxDD

	return out
}
