// embed_quota_handler.go — admin-only JSON view onto the W4-23
// embedquota.Limiter.
//
// WHY THIS EXISTS
// ---------------
// /api/metrics already emits every embedquota number in
// Prometheus text format (W6-2 gauges, W8-1 counters, W9-1 +
// W10-1 histograms). That's the right shape for scraping but a
// terrible shape for an operator who just got a "throttled"
// alert at 2 AM and wants to know "what's actually happening
// right now". This endpoint serves the SAME numbers as small
// JSON, plus the configured caps and a freshness timestamp, so
// the Admin UI panel (W11-2) can render a one-liner answer
// without grepping Prometheus text.
//
// Mirrors the W8-2 memreembed_handler.go pattern: super-admin
// gated, read-only, always-registers (when the limiter is nil
// the handler reports `enabled=false` so the Admin UI doesn't
// have to special-case a 404 route).
//
// The histogram surfaces are intentionally truncated to a few
// summary numbers (count + sum + a single tail percentile
// estimate) — the full bucket counts are only useful for
// Prometheus's histogram_quantile() and would clutter a JSON
// surface meant for human reading. The Admin UI panel that
// consumes this endpoint pairs with the Grafana dashboard for
// the deep view.

package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/fundai/server/internal/embedquota"
	"github.com/fundai/server/internal/embedquotaobs"
)

type embedQuotaHandler struct {
	limiter  *embedquota.Limiter
	recorder *embedquotaobs.Recorder
}

func newEmbedQuotaHandler(svc *Services) *embedQuotaHandler {
	if svc == nil {
		return nil
	}
	// Allow a nil limiter: handler still registers and reports
	// `enabled=false` so the Admin UI panel can render a
	// "embed quota disabled" state instead of a route 404.
	// Same convention for the W14-1 per-fund recorder: nil =>
	// the per-fund route reports `enabled=false` rather than a
	// 404.
	return &embedQuotaHandler{
		limiter:  svc.EmbedLimiter,
		recorder: svc.EmbedQuotaRecorder,
	}
}

func (h *embedQuotaHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/embed-quota/status", h.handleStatus)
	// W14-3 — per-fund snapshot. Coexists with /status; the
	// aggregate route is unchanged so existing dashboards and
	// the W11-2 sparkline keep working.
	mux.HandleFunc("GET /api/admin/embed-quota/per-fund", h.handlePerFund)
}

// embedQuotaStatus is the wire shape served by the handler.
//
// `Enabled` is the discriminator: when false, every numeric
// field is zero and the UI should render a "limiter disabled"
// state rather than mistake the zeros for "perfectly healthy".
//
// The histogram tail estimates (`AcquireWaitP99Seconds` and
// `CallTokensP99`) are computed server-side from the cumulative
// buckets so the UI doesn't have to ship a histogram math
// helper. They're estimates — the bucket boundaries are coarse
// (10 buckets for wait, 7 for tokens), so the percentile is
// pinned to the next bucket boundary above the true value.
// That's good enough for "is the tail growing?"; not for SLO
// math, which lives in Prometheus where buckets sum across
// scrapes.
type embedQuotaStatus struct {
	Enabled bool `json:"enabled"`

	// Live state (mirrors HealthSnapshot).
	Status            string  `json:"status"`
	TokensTodayUsed   int     `json:"tokensTodayUsed"`
	TokensDailyMax    int     `json:"tokensDailyMax"`
	TokensTodayShare  float64 `json:"tokensTodayShare"`
	CallsLastMinute   int     `json:"callsLastMinute"`
	CallsPerMinuteMax int     `json:"callsPerMinuteMax"`
	SoftLimitFraction float64 `json:"softLimitFraction"`

	// Lifetime backpressure event counters (W8-1).
	ThrottledTotal uint64 `json:"throttledTotal"`
	ExhaustedTotal uint64 `json:"exhaustedTotal"`

	// W9-1 / W10-1 histogram summaries.
	AcquireWaitCount      uint64  `json:"acquireWaitCount"`
	AcquireWaitSumSeconds float64 `json:"acquireWaitSumSeconds"`
	AcquireWaitP99Seconds float64 `json:"acquireWaitP99Seconds"`
	CallTokensCount       uint64  `json:"callTokensCount"`
	CallTokensSum         uint64  `json:"callTokensSum"`
	CallTokensP99         float64 `json:"callTokensP99"`

	// W12-3 — last 7 days of token usage for the Admin UI
	// sparkline. Always exactly tokenHistoryDays elements when
	// `Enabled` is true, ascending by day, today last. When the
	// limiter is disabled we omit the field rather than send an
	// array of zeros, since "no data" and "literal zeros" mean
	// different things to a sparkline renderer.
	TokenHistory []embedquota.DaySnapshot `json:"tokenHistory,omitempty"`

	ObservedAt time.Time `json:"observedAt"`
}

// tokenHistoryDays controls the size of the rolling window the
// Admin UI sparkline gets. 7 is calibrated against:
//   - one full work week's worth of context fits on a small card
//     without horizontal overflow;
//   - sparkline density is comfortable at 7 bars on a 200-300 px
//     wide card;
//   - the JSON response stays small (~330 bytes) even with a
//     verbose date format.
//
// If a longer window becomes useful (e.g. a 30-day capacity
// review tab), prefer making this a query-parameter rather than
// changing the constant — current callers depend on receiving
// exactly the number of days the panel is sized for.
const tokenHistoryDays = 7

func (h *embedQuotaHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}

	out := embedQuotaStatus{
		Enabled:    h != nil && h.limiter != nil,
		ObservedAt: time.Now().UTC(),
	}
	if h != nil && h.limiter != nil {
		health := h.limiter.HealthSnapshot()
		out.Status = string(health.Status)
		out.TokensTodayUsed = health.TokensTodayUsed
		out.TokensDailyMax = health.TokensDailyMax
		out.CallsLastMinute = health.CallsLastMinute
		out.CallsPerMinuteMax = health.CallsPerMinuteMax
		out.SoftLimitFraction = health.SoftLimitFraction
		out.ThrottledTotal = health.ThrottledTotal
		out.ExhaustedTotal = health.ExhaustedTotal
		if health.TokensDailyMax > 0 {
			out.TokensTodayShare = float64(health.TokensTodayUsed) / float64(health.TokensDailyMax)
		}

		waitHist := h.limiter.WaitHistogram()
		out.AcquireWaitCount = waitHist.Count
		out.AcquireWaitSumSeconds = waitHist.SumSeconds
		out.AcquireWaitP99Seconds = histogramP99WaitSeconds(waitHist)

		tokenHist := h.limiter.TokenHistogram()
		out.CallTokensCount = tokenHist.Count
		out.CallTokensSum = tokenHist.Sum
		out.CallTokensP99 = histogramP99Tokens(tokenHist)

		out.TokenHistory = h.limiter.RecentDays(tokenHistoryDays)
	} else {
		// Render the textual status as "unavailable" so the UI
		// can show the same string the limiter would produce
		// for a nil-receiver HealthSnapshot. Avoids one more
		// special-case in the React panel.
		out.Status = string(embedquota.StatusUnavailable)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
	}
}

// histogramP99WaitSeconds returns the smallest bucket boundary
// at or above which the cumulative count covers ≥99% of
// observations. Returns 0 when the histogram is empty (no
// observations yet) — an empty histogram has no defined p99
// and the UI should render "—" in that case.
//
// We could interpolate between buckets for a smoother estimate,
// but the bucket schedule is coarse (factors of ~5x) so any
// interpolation is a polite fiction. Snapping to a bucket
// boundary tells the operator exactly where the limiter's
// observations land in the schedule.
func histogramP99WaitSeconds(h embedquota.WaitHistogramSnapshot) float64 {
	if h.Count == 0 {
		return 0
	}
	target := float64(h.Count) * 0.99
	for _, b := range h.Buckets {
		if float64(b.Count) >= target {
			return b.LeSeconds
		}
	}
	// All finite buckets fell short of 99% → the +Inf bucket
	// holds the tail. We don't know how high it actually
	// reaches, so signal "above the largest finite bucket"
	// using the cap.
	if len(h.Buckets) == 0 {
		return 0
	}
	return h.Buckets[len(h.Buckets)-1].LeSeconds
}

// histogramP99Tokens is the token-histogram twin. Same coarse
// snap-to-bucket-boundary semantics.
func histogramP99Tokens(h embedquota.TokenHistogramSnapshot) float64 {
	if h.Count == 0 {
		return 0
	}
	target := float64(h.Count) * 0.99
	for _, b := range h.Buckets {
		if float64(b.Count) >= target {
			return b.Le
		}
	}
	if len(h.Buckets) == 0 {
		return 0
	}
	return h.Buckets[len(h.Buckets)-1].Le
}

// embedQuotaPerFundResponse is the wire shape for the W14-3
// per-fund admin endpoint.
//
// We deliberately keep the per-fund payload minimal — operators
// arrive here from the aggregate panel having already seen
// "throttle is high overall, drill into per-fund". The list is
// pre-sorted by FundID to give stable UI rendering, and a
// computed P99 pair is included per fund so the UI can colour-
// rank without shipping histogram math.
//
// Note that we do NOT include the raw bucket arrays in this
// JSON: that's what /metrics is for. The admin JSON is
// optimised for "what's the worst-offender right now" and the
// bucket-level detail just clutters the table.
type embedQuotaPerFundResponse struct {
	Enabled    bool                  `json:"enabled"`
	Funds      []embedQuotaFundEntry `json:"funds"`
	ObservedAt time.Time             `json:"observedAt"`
}

// embedQuotaFundEntry is one row in the per-fund table. Mirrors
// the FundSnapshot wire shape from embedquotaobs but trims the
// bucket arrays — see embedQuotaPerFundResponse rationale.
type embedQuotaFundEntry struct {
	FundID                string    `json:"fundId"`
	ThrottledTotal        uint64    `json:"throttledTotal"`
	ExhaustedTotal        uint64    `json:"exhaustedTotal"`
	WaitCount             uint64    `json:"waitCount"`
	AcquireWaitSumSeconds float64   `json:"acquireWaitSumSeconds"`
	AcquireWaitP99Seconds float64   `json:"acquireWaitP99Seconds"`
	CallTokensCount       uint64    `json:"callTokensCount"`
	CallTokensSum         uint64    `json:"callTokensSum"`
	CallTokensP99         float64   `json:"callTokensP99"`
	TokensTodayUsed       int       `json:"tokensTodayUsed"`
	LastSeenAt            time.Time `json:"lastSeenAt"`
}

func (h *embedQuotaHandler) handlePerFund(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}

	out := embedQuotaPerFundResponse{
		Enabled:    h != nil && h.recorder != nil,
		Funds:      []embedQuotaFundEntry{},
		ObservedAt: time.Now().UTC(),
	}

	if out.Enabled {
		snaps := h.recorder.Snapshot()
		out.Funds = make([]embedQuotaFundEntry, 0, len(snaps))
		for _, s := range snaps {
			out.Funds = append(out.Funds, embedQuotaFundEntry{
				FundID:                s.FundID,
				ThrottledTotal:        s.ThrottledTotal,
				ExhaustedTotal:        s.ExhaustedTotal,
				WaitCount:             s.WaitCount,
				AcquireWaitSumSeconds: s.WaitSumSeconds,
				AcquireWaitP99Seconds: bucketP99(s.WaitBuckets, s.WaitCount),
				CallTokensCount:       s.TokenCount,
				CallTokensSum:         s.TokenSum,
				CallTokensP99:         bucketP99(s.TokenBuckets, s.TokenCount),
				TokensTodayUsed:       s.TokensTodayUsed,
				LastSeenAt:            s.LastSeenAt,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
	}
}

// bucketP99 estimates the 99th percentile by snapping to the
// smallest bucket boundary whose cumulative count covers ≥99%
// of observations. Same coarse, snap-to-boundary discipline as
// histogramP99WaitSeconds — see that function's doc for the
// "polite fiction" reasoning. Generic over the BucketCount type
// so wait and token histograms share one helper.
func bucketP99(buckets []embedquotaobs.BucketCount, count uint64) float64 {
	if count == 0 {
		return 0
	}
	target := float64(count) * 0.99
	for _, b := range buckets {
		if float64(b.Count) >= target {
			return b.Le
		}
	}
	if len(buckets) == 0 {
		return 0
	}
	return buckets[len(buckets)-1].Le
}
