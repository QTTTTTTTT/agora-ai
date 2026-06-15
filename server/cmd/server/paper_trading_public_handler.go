// paper_trading_public_handler.go — HTTP surface for the public
// Stage-4 track record.
//
// Routes
//   GET /api/papertrading/public/track-record           (list)
//   GET /api/papertrading/public/track-record/{portfolioId}
//
// Authentication
//   Both endpoints are intentionally unauthenticated — they are
//   the SEC Publisher's-Exclusion advertising surface and must be
//   identical for every viewer. Auth would re-introduce the
//   "personalised advice" risk the exclusion is supposed to avoid.
//   isPublicRoute() in main.go is patched to allow the prefix.
//
// Caching
//   The service computes the metrics in process; no cache layer
//   is added here on purpose — the nav history is at most a few
//   hundred rows per portfolio, and the page is dwarfed by the
//   marketing chrome that wraps it. Add a cache when traffic
//   actually warrants it.
//
// Compliance footer
//   The disclosure block lives next to the data in the response
//   payload (see internal/papertrading/public_track_record.go).
//   Clients SHOULD render the entire `disclosure.statements`
//   array; truncating is a compliance bug, not a design choice.

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/papertrading"
	"github.com/fundai/server/internal/repository"
)

// buildPaperTradingPublicHandler is the wiring shim called from
// router.go. Returns nil when the Services bundle hasn't given us
// a DB pool — the registration site treats nil as "skip the
// routes" so a degraded boot doesn't 5xx the SPA.
func buildPaperTradingPublicHandler(svc *Services) *paperTradingPublicHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	repo := repository.NewPaperTradingRepo(svc.DB)
	service := papertrading.New(repo, papertrading.StubOTSClient{}, nil)
	h := newPaperTradingPublicHandler(service)
	// Master-team backtest runner shares the same OHLC chain the
	// rest of the platform uses (Yahoo + Akshare + Eastmoney +
	// Binance, cache-wrapped). nil fetcher is a soft-degrade —
	// the endpoint just returns 503 so the SPA falls back to a
	// quiet placeholder card.
	if fetcher := buildOHLCFetcherFromEnv(); fetcher != nil && h != nil {
		h.masterBacktest = papertrading.NewMasterBacktestRunner(fetcher, nil)
	}
	return h
}

// paperTradingPublicHandler is the thin HTTP shim. It holds the
// SAME service the operator-side adapter uses — there's no second
// service object, just a different code path that calls the public
// methods.
type paperTradingPublicHandler struct {
	svc            *papertrading.Service
	masterBacktest *papertrading.MasterBacktestRunner
}

func newPaperTradingPublicHandler(svc *papertrading.Service) *paperTradingPublicHandler {
	if svc == nil {
		return nil
	}
	return &paperTradingPublicHandler{svc: svc}
}

// registerPublicPaperTradingRoutes wires the two GET endpoints
// onto the mux. nil handler is a no-op so deployments without the
// Stage-4 service silently omit the routes (they'll fall through
// to the SPA fallback / 404).
func registerPublicPaperTradingRoutes(mux *http.ServeMux, h *paperTradingPublicHandler) {
	if mux == nil || h == nil {
		return
	}
	mux.HandleFunc("GET /api/papertrading/public/track-record", h.handleList)
	mux.HandleFunc("GET /api/papertrading/public/track-record/{portfolioId}", h.handleGet)
	// Master-team factor backtest. Public on purpose: same data
	// for every viewer keeps the SEC Publisher's-Exclusion
	// posture intact — this is a marketing curve, not advice.
	mux.HandleFunc("GET /api/papertrading/public/master-backtest", h.handleMasterBacktest)
}

// handleList returns every portfolio currently flagged for public
// display. 200 with `{ "trackRecords": [...] }` even when the list
// is empty — a missing entry is a real signal ("nothing public yet")
// not an error.
func (h *paperTradingPublicHandler) handleList(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "paper trading not configured"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), publicTrackTimeout)
	defer cancel()
	rows, err := h.svc.ListPublicTrackRecord(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if rows == nil {
		// Make sure JSON encoding emits `[]`, not `null`, so
		// strict clients don't crash on .map() of null.
		rows = []*papertrading.PublicTrackRecordSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"trackRecords": rows,
		"meta": map[string]any{
			"surface": "publishers_exclusion",
			"count":   len(rows),
		},
	})
}

// handleGet returns the detail for one portfolio. 404 covers both
// "no such id" and "id exists but not public" — keeping them
// indistinguishable prevents enumerating the internal portfolio
// space through the public surface.
func (h *paperTradingPublicHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "paper trading not configured"))
		return
	}
	portfolioID := strings.TrimSpace(r.PathValue("portfolioId"))
	if portfolioID == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("bad_input", "portfolio id required"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), publicTrackTimeout)
	defer cancel()
	rec, err := h.svc.GetPublicTrackRecord(ctx, portfolioID)
	if err != nil {
		switch {
		case errors.Is(err, papertrading.ErrTrackRecordNotPublic):
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "no public track record for that portfolio"))
		case errors.Is(err, papertrading.ErrServiceUnconfigured):
			writeJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "paper trading not configured"))
		default:
			writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		}
		return
	}
	if rec == nil {
		writeJSON(w, http.StatusNotFound, errorPayload("not_found", "no public track record for that portfolio"))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// publicTrackTimeout caps how long we wait on the DB for the
// public path. Generous enough to cover a cold cache pull on the
// nav history (~365 rows worst case) but small enough that a
// pathological deadlock can't pile up requests on the public
// surface.
const publicTrackTimeout = 5 * time.Second

// masterBacktestTimeout is wider than publicTrackTimeout because
// a cold-cache run pulls ~12 daily OHLC histories from upstream
// providers (Yahoo). The runner caches results for 6h so warm
// hits return in milliseconds.
const masterBacktestTimeout = 30 * time.Second

// handleMasterBacktest returns the master-team factor backtest
// curve plus benchmark curves for the /papertrading SPA.
//
// Query params (all optional):
//
//	start=YYYY-MM-DD       default 2015-01-01
//	end=YYYY-MM-DD         default today
//	initial=100000         default $100k
//	benchmarks=SPY,QQQ     default SPY,QQQ
func (h *paperTradingPublicHandler) handleMasterBacktest(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.masterBacktest == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "master backtest not configured (no OHLC fetcher)"))
		return
	}
	q := r.URL.Query()
	req := papertrading.MasterBacktestRequest{}
	if s := strings.TrimSpace(q.Get("start")); s != "" {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("bad_input", "start must be YYYY-MM-DD"))
			return
		}
		req.Start = t
	}
	if s := strings.TrimSpace(q.Get("end")); s != "" {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("bad_input", "end must be YYYY-MM-DD"))
			return
		}
		req.End = t
	}
	if s := strings.TrimSpace(q.Get("initial")); s != "" {
		var v float64
		if _, err := fmtSscan(s, &v); err == nil && v > 0 {
			req.InitialCapital = v
		}
	}
	if s := strings.TrimSpace(q.Get("benchmarks")); s != "" {
		parts := strings.Split(s, ",")
		req.Benchmarks = parts
	}

	ctx, cancel := context.WithTimeout(r.Context(), masterBacktestTimeout)
	defer cancel()
	result, err := h.masterBacktest.Run(ctx, req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// fmtSscan is a tiny wrapper around fmt.Sscan to keep the import
// list visible at the top of the file.
func fmtSscan(s string, v *float64) (int, error) {
	return fmt.Sscan(s, v)
}
