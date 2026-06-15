// admin_wsfeed.go — S6.5 WebSocket real-time market-data
// admin REST endpoints.
//
// Endpoints
//
//   GET  /api/admin/wsfeed/status
//   GET  /api/admin/wsfeed/connections
//   GET  /api/admin/wsfeed/subscriptions
//   GET  /api/admin/wsfeed/cache
//   GET  /api/admin/wsfeed/cache/{symbol}
//   POST /api/admin/wsfeed/subscribe         body: {symbol, market}
//   POST /api/admin/wsfeed/unsubscribe       body: {symbol}
//   POST /api/admin/wsfeed/cache/evict       body: {symbol}      ("*" → all)
//   POST /api/admin/wsfeed/reconcile         (kick subscription bridge)
//
// All endpoints require super_admin role. 503 when WS feed is
// disabled or wiring is missing.
//
// Why super_admin
//
//   - subscribe / unsubscribe / cache eviction change what
//     the broker hot path reads. Misuse could mask a stale
//     quote or stop the position refresher from learning a
//     new price.
//   - status / connections / subscriptions / cache reads are
//     not sensitive in themselves but they leak the symbol
//     universe of all funds; we keep them behind the same
//     gate to avoid an information-disclosure footgun.

package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/wsfeed"
)

func (h *adminHandler) registerWSFeedAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/wsfeed/status", h.handleWSFeedStatus)
	mux.HandleFunc("GET /api/admin/wsfeed/connections", h.handleWSFeedConnections)
	mux.HandleFunc("GET /api/admin/wsfeed/subscriptions", h.handleWSFeedSubscriptions)
	mux.HandleFunc("GET /api/admin/wsfeed/cache", h.handleWSFeedCacheList)
	mux.HandleFunc("GET /api/admin/wsfeed/cache/{symbol}", h.handleWSFeedCacheGet)
	mux.HandleFunc("POST /api/admin/wsfeed/subscribe", h.handleWSFeedSubscribe)
	mux.HandleFunc("POST /api/admin/wsfeed/unsubscribe", h.handleWSFeedUnsubscribe)
	mux.HandleFunc("POST /api/admin/wsfeed/cache/evict", h.handleWSFeedCacheEvict)
	mux.HandleFunc("POST /api/admin/wsfeed/reconcile", h.handleWSFeedReconcile)
}

// wsFeedAvailable returns true when the feed is wired. All
// mutators / readers check this first to give a clean 503
// instead of nil-deref.
func (h *adminHandler) wsFeedAvailable() bool {
	return h != nil && h.wsFeedManager != nil
}

func (h *adminHandler) handleWSFeedStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.wsFeedAvailable() {
		// Soft-degrade with HTTP 200 so the /admin SPA can read
		// `enabled: false` and silently hide the WSFeed section
		// instead of rendering a red 503 banner. The reason
		// string stays for operator debugging.
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"reason":  "wsfeed not configured (WSFEED_ENABLED=false or wiring missing)",
		})
		return
	}
	conns := h.wsFeedManager.ConnectionsSnapshot()
	subs := h.wsFeedManager.Subscriptions()
	healthy := 0
	for _, c := range conns {
		if c.State == wsfeed.StateConnected {
			healthy++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":            true,
		"healthy_providers":  healthy,
		"total_providers":    len(conns),
		"subscriptions":      len(subs),
		"cache_symbols":      h.wsFeedCacheSymbols(),
		"dropped_events":     h.wsFeedManager.DroppedEvents(),
		"total_ticks":        h.wsFeedManager.TotalTicks(),
	})
}

func (h *adminHandler) wsFeedCacheSymbols() int {
	if h.wsFeedCache == nil {
		return 0
	}
	return h.wsFeedCache.Stats().Symbols
}

func (h *adminHandler) handleWSFeedConnections(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.wsFeedAvailable() {
		// Soft-degrade — return empty list with HTTP 200 so the
		// admin SPA can render without a red banner. See
		// handleWSFeedStatus for the rationale.
		writeJSON(w, http.StatusOK, map[string]any{"connections": []any{}})
		return
	}
	rows := h.wsFeedManager.ConnectionsSnapshot()
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{
			"provider":         c.Provider,
			"state":            string(c.State),
			"connected_at":     timeOrNil(c.ConnectedAt),
			"disconnected_at":  timeOrNil(c.DisconnectedAt),
			"last_tick_at":     timeOrNil(c.LastTickAt),
			"tick_count":       c.TickCount,
			"reconnect_count":  c.ReconnectCount,
			"last_error":       c.LastError,
			"subscriptions":    c.Subscriptions,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": out})
}

func (h *adminHandler) handleWSFeedSubscriptions(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.wsFeedAvailable() {
		writeJSON(w, http.StatusOK, map[string]any{"subscriptions": []any{}})
		return
	}
	subs := h.wsFeedManager.Subscriptions()
	sort.Slice(subs, func(i, j int) bool { return subs[i].Symbol < subs[j].Symbol })
	out := make([]map[string]any, 0, len(subs))
	for _, s := range subs {
		out = append(out, map[string]any{
			"symbol":       s.Symbol,
			"market":       s.Market,
			"consumers":    s.Consumers,
			"last_tick_at": timeOrNil(s.LastTickAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": out})
}

func (h *adminHandler) handleWSFeedCacheList(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.wsFeedAvailable() || h.wsFeedCache == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"snapshots": []any{},
			"stats":     map[string]any{"symbols": 0, "hits": 0, "misses": 0, "stales": 0, "evicts": 0},
		})
		return
	}
	snaps := h.wsFeedCache.SnapshotAll()
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Symbol < snaps[j].Symbol })
	stats := h.wsFeedCache.Stats()
	out := make([]map[string]any, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, map[string]any{
			"symbol":         s.Symbol,
			"display":        s.DisplaySymbol,
			"market":         s.Market,
			"provider":       s.Provider,
			"last":           s.Last,
			"bid":            s.Bid,
			"ask":            s.Ask,
			"volume":         s.Volume,
			"as_of":          timeOrNil(s.AsOf),
			"received_at":    timeOrNil(s.ReceivedAt),
			"update_kind":    s.LastUpdateKind,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshots": out,
		"stats": map[string]any{
			"symbols": stats.Symbols,
			"hits":    stats.Hits,
			"misses":  stats.Misses,
			"stales":  stats.Stales,
			"evicts":  stats.Evicts,
		},
	})
}

func (h *adminHandler) handleWSFeedCacheGet(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.wsFeedAvailable() || h.wsFeedCache == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "wsfeed cache not configured"})
		return
	}
	sym := strings.ToUpper(strings.TrimSpace(r.PathValue("symbol")))
	if sym == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "symbol required"})
		return
	}
	snap, ok, stale := h.wsFeedCache.Lookup(sym)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no cache entry"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"symbol":      snap.Symbol,
		"display":     snap.DisplaySymbol,
		"market":      snap.Market,
		"provider":    snap.Provider,
		"last":        snap.Last,
		"bid":         snap.Bid,
		"ask":         snap.Ask,
		"volume":      snap.Volume,
		"as_of":       timeOrNil(snap.AsOf),
		"received_at": timeOrNil(snap.ReceivedAt),
		"update_kind": snap.LastUpdateKind,
		"stale":       stale,
	})
}

type wsFeedSymbolBody struct {
	Symbol  string `json:"symbol"`
	Market  string `json:"market"`
}

func (h *adminHandler) handleWSFeedSubscribe(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.wsFeedAvailable() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "wsfeed not configured"})
		return
	}
	var body wsFeedSymbolBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	body.Symbol = strings.ToUpper(strings.TrimSpace(body.Symbol))
	if body.Symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "symbol required"})
		return
	}
	err := h.wsFeedManager.Subscribe(wsfeed.Subscription{
		Symbol:        body.Symbol,
		DisplaySymbol: body.Symbol,
		Market:        strings.ToUpper(strings.TrimSpace(body.Market)),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if h.metrics != nil {
		h.metrics.RecordWSFeedEvent("admin_subscribe")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "symbol": body.Symbol})
}

func (h *adminHandler) handleWSFeedUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.wsFeedAvailable() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "wsfeed not configured"})
		return
	}
	var body wsFeedSymbolBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	body.Symbol = strings.ToUpper(strings.TrimSpace(body.Symbol))
	if body.Symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "symbol required"})
		return
	}
	if err := h.wsFeedManager.Unsubscribe(body.Symbol); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if h.metrics != nil {
		h.metrics.RecordWSFeedEvent("admin_unsubscribe")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "symbol": body.Symbol})
}

func (h *adminHandler) handleWSFeedCacheEvict(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.wsFeedAvailable() || h.wsFeedCache == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "wsfeed cache not configured"})
		return
	}
	var body wsFeedSymbolBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	sym := strings.ToUpper(strings.TrimSpace(body.Symbol))
	if sym == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "symbol required (use '*' to evict all)"})
		return
	}
	evicted := 0
	if sym == "*" {
		for _, s := range h.wsFeedCache.SnapshotAll() {
			h.wsFeedCache.Delete(s.Symbol)
			evicted++
		}
	} else {
		h.wsFeedCache.Delete(sym)
		evicted = 1
	}
	if h.metrics != nil {
		h.metrics.RecordWSFeedEvent("admin_cache_evict")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "evicted": evicted})
}

func (h *adminHandler) handleWSFeedReconcile(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if !h.wsFeedAvailable() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "wsfeed not configured"})
		return
	}
	if h.wsFeedBridge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "wsfeed subscription bridge not configured"})
		return
	}
	// Reconcile synchronously so the operator gets immediate
	// feedback. The bridge is internally idempotent.
	h.wsFeedBridge.reconcileOnce(r.Context())
	if h.metrics != nil {
		h.metrics.RecordWSFeedEvent("admin_reconcile")
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// timeOrNil emits time.Time as an ISO string, or nil if zero.
// JSON-friendly representation for the admin UI.
func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
