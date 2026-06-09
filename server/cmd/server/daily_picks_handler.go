// daily_picks_handler.go — HTTP surface for the /api/daily-picks
// publisher product. Three endpoints:
//
//	GET  /api/daily-picks                    — browse grid
//	GET  /api/daily-picks/{date}/{symbol}    — single-stock deep dive (quota-gated)
//	POST /api/daily-picks/_admin/run-once    — admin: trigger one wave
//
// Tier model (this file is the SINGLE source of truth for who
// sees what):
//
//	Free   : list capped at pick_date <= today-14d, detail same lag,
//	         no per-day quota on detail because they can't even reach
//	         today's rows in the first place.
//	Basic  : list shows today's rows; detail capped at 10 / day.
//	Pro    : list shows today; detail capped at 30 / day.
//	Enterprise : list shows today; detail uncapped.
//
//	The list ENDPOINT serves the same rows to every tier — what
//	differs is the pick_date upper-bound filter. This preserves the
//	publisher-mode invariant ("one row, all readers") while still
//	giving paid tiers something to pay for (timeliness).

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/advisor"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/dailypicks"
	"github.com/fundai/server/internal/subscription"
)

// freeTierLagDays is the time-lag applied to free readers. 14d
// matches the figure baked into the product spec and is the
// shortest gap that still creates a meaningful upgrade prompt.
const freeTierLagDays = 14

// dailyPickDetailQuotaByTier maps subscription plan to the per-day
// cap on detail reads. Tiers omitted from this map (or unknown) get
// the free tier's cap. Enterprise gets -1 = unlimited.
var dailyPickDetailQuotaByTier = map[subscription.PlanTier]int{
	subscription.PlanFree:       0, // free can't reach today's rows so this is unreachable; we keep it 0 for safety
	subscription.PlanPro:        30,
	subscription.PlanPremium:    30,
	subscription.PlanEnterprise: -1,
}

// basicTierDetailQuota is the cap for the "basic" tier in the
// product spec. The current subscription package doesn't model
// basic separately — we treat plan_tier IS NULL / "" as free
// and "pro" / "premium" as the paid tiers. If a "basic" tier is
// added later, update this map and the lookup in resolveQuota.

// dailyPicksHandler bundles the three endpoints. Constructed by
// the wiring layer once advisor.Service + dailypicks.Repo +
// SubscriptionService are all ready.
type dailyPicksHandler struct {
	advisor *advisor.Service
	picks   *dailypicks.Repo
	subs    *subscription.SubscriptionService
	// db is used only to check the users.role column for the
	// admin gate on the /run-once endpoint. Kept narrow on
	// purpose — this handler should NOT grow general-purpose
	// SQL access.
	db *sql.DB
	// loop is optional — only set when the binary owns the
	// nightly wave. Without it, the /run-once admin endpoint
	// returns 503. (A future replica split could hand the loop
	// to one leader and the handler to several reader replicas.)
	loop  *dailyPicksLoop
	clock func() time.Time
}

func newDailyPicksHandler(adv *advisor.Service, picks *dailypicks.Repo, subs *subscription.SubscriptionService, db *sql.DB, loop *dailyPicksLoop) *dailyPicksHandler {
	return &dailyPicksHandler{
		advisor: adv,
		picks:   picks,
		subs:    subs,
		db:      db,
		loop:    loop,
		clock:   time.Now,
	}
}

// userHasAdminRole is the in-handler admin gate. Mirrors
// adminHandler.userIsAdmin in admin_fx.go but defined here so this
// file doesn't reach into the admin package's internals — keeps
// the daily-picks surface self-contained.
func (h *dailyPicksHandler) userHasAdminRole(ctx context.Context, userID string) bool {
	if h == nil || h.db == nil {
		return false
	}
	var role string
	err := h.db.QueryRowContext(ctx, `SELECT role FROM users WHERE id = $1`, userID).Scan(&role)
	if err != nil {
		return false
	}
	return strings.EqualFold(role, "admin") || strings.EqualFold(role, "super_admin")
}

// --- Endpoint: GET /api/daily-picks ---------------------------------------

// dailyPicksListResponse is the wire shape the browse grid renders.
type dailyPicksListResponse struct {
	Picks            []dailyPickRow `json:"picks"`
	TotalCount       int            `json:"total_count"`
	Tier             string         `json:"tier"`
	FreeLagDays      int            `json:"free_lag_days"`
	NewestAvailable  string         `json:"newest_available_date,omitempty"` // ISO yyyy-mm-dd
	NewestForTier    string         `json:"newest_for_tier_date,omitempty"`
	// UpgradeRequiredForToday is true when the requester is on a
	// free tier and today's row set exists but is hidden — the
	// frontend uses this to render an upgrade overlay over the
	// "today" tab. False when there's nothing to upgrade for
	// (e.g. the loop hasn't run yet) so we don't pester the user
	// for content that doesn't exist.
	UpgradeRequiredForToday bool `json:"upgrade_required_for_today"`
}

// dailyPickRow is the per-card shape the browse grid renders. We
// project a small subset of the full daily_picks row so the wire
// payload stays bounded — the full ConsultResponse only ships on
// the detail endpoint.
type dailyPickRow struct {
	Symbol             string  `json:"symbol"`
	SymbolName         string  `json:"symbol_name,omitempty"`
	Market             string  `json:"market"`
	PresetKey          string  `json:"preset_key"`
	PickDate           string  `json:"pick_date"` // ISO yyyy-mm-dd
	AggregateVerdict   string  `json:"aggregate_verdict"`
	AggregateScore     int     `json:"aggregate_score"`
	Consensus          float64 `json:"consensus"`
	// HeadlineThesis is the verbatim thesis sentence of the
	// highest-confidence master, redacted by compliance.Scan at
	// computation time. UIs render this as the card subtitle so
	// the grid is browsable without opening every detail.
	HeadlineThesis string `json:"headline_thesis,omitempty"`
	HasError       bool   `json:"has_error,omitempty"`
}

func (h *dailyPicksHandler) handleList(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "login required"))
		return
	}
	tier := h.resolveTier(r.Context(), userID)

	q := r.URL.Query()
	market := strings.TrimSpace(q.Get("market"))
	if market == "" {
		market = "us_equity" // sensible default for the publisher v1
	}
	preset := strings.TrimSpace(q.Get("preset"))
	if preset == "" {
		preset = "disruptive"
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	offset, _ := strconv.Atoi(q.Get("offset"))

	// Tier-driven time lag is the SECURITY-CRITICAL slot —
	// the SQL upper bound here is what gates free users away
	// from today's rows. Don't move this filter to the front
	// end; the API must enforce.
	maxDate := h.maxAllowedDateForTier(tier)
	// requestedDate optionally narrows further. If the requested
	// day is later than the tier permits, clamp instead of 404 —
	// the frontend gets back "the newest you're allowed" along
	// with the explicit "you wanted X but get Y" signal.
	var requestedDate time.Time
	if s := strings.TrimSpace(q.Get("date")); s != "" {
		if d, err := time.Parse("2006-01-02", s); err == nil {
			requestedDate = d
		}
	}
	pinDate := time.Time{}
	if !requestedDate.IsZero() {
		if requestedDate.After(maxDate) {
			pinDate = maxDate
		} else {
			pinDate = requestedDate
		}
	}

	rows, err := h.picks.List(r.Context(), dailypicks.ListFilter{
		Market:      market,
		PresetKey:   preset,
		PickDate:    pinDate,
		MaxPickDate: maxDate,
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}

	// Determine "newest_available_date" (across ALL tiers, for
	// upgrade-prompt accuracy) by querying without the tier lag
	// gate. This is one extra small query per browse — cheap and
	// it's the only way to honestly tell the user "hey there IS
	// a newer one, upgrade to see it".
	var newestAvailable time.Time
	allTimeRows, _ := h.picks.List(r.Context(), dailypicks.ListFilter{
		Market:    market,
		PresetKey: preset,
		Limit:     1,
	})
	if len(allTimeRows) > 0 {
		newestAvailable = allTimeRows[0].PickDate
	}

	upgradeForToday := false
	if tier == subscription.PlanFree && !newestAvailable.IsZero() {
		upgradeForToday = newestAvailable.After(maxDate)
	}

	wire := dailyPicksListResponse{
		Picks:                   projectPickRows(rows),
		TotalCount:              len(rows),
		Tier:                    string(tier),
		FreeLagDays:             freeTierLagDays,
		UpgradeRequiredForToday: upgradeForToday,
	}
	if !newestAvailable.IsZero() {
		wire.NewestAvailable = newestAvailable.UTC().Format("2006-01-02")
	}
	if len(rows) > 0 {
		wire.NewestForTier = rows[0].PickDate.UTC().Format("2006-01-02")
	}
	writeJSON(w, http.StatusOK, wire)
}

// --- Endpoint: GET /api/daily-picks/{date}/{symbol} ----------------------

// dailyPickDetailResponse wraps the full ConsultResponse JSON
// already stored in daily_picks.result_json plus a quota footer so
// the frontend can render the "you've used X of Y today" line.
type dailyPickDetailResponse struct {
	Pick           json.RawMessage `json:"pick"`
	Symbol         string          `json:"symbol"`
	SymbolName     string          `json:"symbol_name,omitempty"`
	Market         string          `json:"market"`
	PresetKey      string          `json:"preset_key"`
	PickDate       string          `json:"pick_date"`
	Tier           string          `json:"tier"`
	QuotaUsedToday int             `json:"quota_used_today"`
	QuotaCapToday  int             `json:"quota_cap_today"` // -1 = unlimited
}

func (h *dailyPicksHandler) handleDetail(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "login required"))
		return
	}
	tier := h.resolveTier(r.Context(), userID)

	dateStr, symbol, ok := parseDetailPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorPayload("bad_path", "expected /api/daily-picks/{yyyy-mm-dd}/{symbol}"))
		return
	}
	pickDate, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("bad_date", "date must be yyyy-mm-dd"))
		return
	}
	// Tier gate — same maxDate as the list view. Free users that
	// guess a today URL get the same 403 they'd get from the
	// frontend's hidden card.
	if pickDate.After(h.maxAllowedDateForTier(tier)) {
		writeJSON(w, http.StatusForbidden, errorPayload("upgrade_required", "this date is not available on your tier"))
		return
	}

	market := strings.TrimSpace(r.URL.Query().Get("market"))
	if market == "" {
		market = "us_equity"
	}
	preset := strings.TrimSpace(r.URL.Query().Get("preset"))
	if preset == "" {
		preset = "disruptive"
	}

	// Per-day per-stock quota. We count distinct (user, day,
	// stock) detail-opens — opening the SAME stock twice in one
	// day does NOT double-charge (matches the user mental model
	// of "I want to re-read the report I bought").
	cap := h.quotaCapForTier(tier)
	used, qerr := h.countDetailReadsToday(r.Context(), userID)
	if qerr != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("quota_check_failed", qerr.Error()))
		return
	}
	if cap >= 0 && used >= cap {
		writeJSON(w, http.StatusTooManyRequests, errorPayload(
			"daily_detail_quota_exhausted",
			fmt.Sprintf("free tier detail quota %d/day reached; upgrade to read more", cap),
		))
		return
	}

	row, err := h.picks.Get(r.Context(), symbol, market, preset, pickDate)
	if errors.Is(err, dailypicks.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errorPayload("not_found", "no pick for that symbol on that day"))
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("read_failed", err.Error()))
		return
	}

	// Record the read. We do this BEFORE writing the response
	// (and accept the small cost of a row-write per detail
	// fetch) so a flaky network connection that drops the body
	// still consumes quota — that matches how Seeking Alpha
	// counts article opens.
	if err := h.recordDetailRead(r.Context(), userID, symbol, pickDate); err != nil {
		// Non-fatal: log + serve the content, since quota
		// drift is a smaller harm than blocking a paid user
		// from reading what they paid for.
		slog.Warn("daily_picks_handler.detail.quota_record_failed",
			"user_id", userID, "symbol", symbol, "err", err)
	}
	// Re-count AFTER recording so re-opens of the same stock
	// on the same day don't over-report by 1. The semantics we
	// want: QuotaUsedToday == count of DISTINCT (stock, day) pairs
	// the user has opened today. Re-reading AAPL twice stays at 1.
	finalUsed, _ := h.countDetailReadsToday(r.Context(), userID)

	writeJSON(w, http.StatusOK, dailyPickDetailResponse{
		Pick:           json.RawMessage(row.ResultJSON),
		Symbol:         row.Symbol,
		SymbolName:     row.SymbolName,
		Market:         row.Market,
		PresetKey:      row.PresetKey,
		PickDate:       row.PickDate.UTC().Format("2006-01-02"),
		Tier:           string(tier),
		QuotaUsedToday: finalUsed,
		QuotaCapToday:  cap,
	})
}

// --- Endpoint: POST /api/daily-picks/_admin/run-once ----------------------

func (h *dailyPicksHandler) handleAdminRunOnce(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "login required"))
		return
	}
	// Admin gate — same role check the existing admin handlers
	// use. Production should bolt this onto the routing layer
	// instead of in-handler so it can't be forgotten on a new
	// route; in-handler is the v1 form-factor.
	if !h.userHasAdminRole(r.Context(), userID) {
		writeJSON(w, http.StatusForbidden, errorPayload("admin_only", "admin role required"))
		return
	}
	if h.loop == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("loop_unavailable", "daily picks loop not wired in this binary"))
		return
	}

	// Mode switch: `?sync=1` keeps the legacy synchronous behaviour
	// (useful for unit tests and tiny watchlists where the caller
	// genuinely wants the count back). Default is async fire-and-
	// forget because a real fleet — 4 presets × 50 stocks × ~30s
	// per LLM call — easily exceeds any client/proxy timeout. The
	// admin doesn't actually need the count synchronously; they
	// poll the list endpoint to see new rows appear.
	if r.URL.Query().Get("sync") == "1" {
		n, err := h.loop.RunOnce(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("run_failed", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"mode":          "sync",
			"picks_written": n,
		})
		return
	}

	// Async path: detach from the request lifecycle (caller may
	// disconnect long before the wave finishes) by using a fresh
	// background context.
	//
	// Timeout strategy: NO outer deadline. RunOnce internally
	// wraps each runWatchlistWave with the configured WaveTimeout,
	// so a stuck LLM call can't run forever; but the OUTER ctx
	// has to be free-running because watchlists run sequentially
	// (4 presets × 1h-wave-cap = up to 4h total at worst case).
	// Earlier versions wrapped the outer ctx with WaveTimeout
	// directly, which caused the 1h deadline to kill garp at
	// 18/50 and prevent macro from starting at all.
	go func() {
		ctx := context.Background()
		started := time.Now()
		n, err := h.loop.RunOnce(ctx)
		if err != nil {
			slog.Error("daily_picks_admin.runonce_failed",
				"err", err,
				"picks_written", n,
				"elapsed_ms", time.Since(started).Milliseconds())
			return
		}
		slog.Info("daily_picks_admin.runonce_done",
			"picks_written", n,
			"elapsed_ms", time.Since(started).Milliseconds())
	}()

	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"mode":   "async",
		"status": "kicked_off",
		"hint":   "poll GET /api/daily-picks to see rows appear, or check app logs for daily_picks_admin.runonce_done",
	})
}

// --- helpers --------------------------------------------------------------

func (h *dailyPicksHandler) resolveTier(ctx context.Context, userID string) subscription.PlanTier {
	if h.subs == nil {
		return subscription.PlanFree
	}
	sub, err := h.subs.GetUserSubscription(ctx, userID)
	if err != nil || sub == nil {
		return subscription.PlanFree
	}
	return sub.PlanTier
}

func (h *dailyPicksHandler) maxAllowedDateForTier(tier subscription.PlanTier) time.Time {
	today := h.clock().UTC().Truncate(24 * time.Hour)
	if tier == subscription.PlanFree {
		return today.AddDate(0, 0, -freeTierLagDays)
	}
	return today
}

func (h *dailyPicksHandler) quotaCapForTier(tier subscription.PlanTier) int {
	if cap, ok := dailyPickDetailQuotaByTier[tier]; ok {
		return cap
	}
	// Unknown tier → conservatively gate it as free.
	return dailyPickDetailQuotaByTier[subscription.PlanFree]
}

// --- per-stock detail quota (lightweight, table-less) ---------------------

// countDetailReadsToday / recordDetailRead use a tiny rolling
// in-memory map keyed by (user, day) — sufficient for v1 single-
// binary deployment. A migration adding a real `daily_pick_reads`
// audit table is the v1.5 follow-up so a multi-replica deploy
// shares quota state across pods.
//
// Important: we count DISTINCT (user, symbol, day) so re-opening
// the same article doesn't double-charge. The map's value is a
// set of "<symbol>@<yyyy-mm-dd>" strings per user.

func (h *dailyPicksHandler) countDetailReadsToday(_ context.Context, userID string) (int, error) {
	detailQuotaMu.Lock()
	defer detailQuotaMu.Unlock()
	day := h.clock().UTC().Format("2006-01-02")
	bucket := detailQuotaStore[userID+"|"+day]
	return len(bucket), nil
}

func (h *dailyPicksHandler) recordDetailRead(_ context.Context, userID, symbol string, pickDate time.Time) error {
	detailQuotaMu.Lock()
	defer detailQuotaMu.Unlock()
	day := h.clock().UTC().Format("2006-01-02")
	key := userID + "|" + day
	bucket := detailQuotaStore[key]
	if bucket == nil {
		bucket = map[string]struct{}{}
		detailQuotaStore[key] = bucket
	}
	bucket[strings.ToUpper(symbol)+"@"+pickDate.UTC().Format("2006-01-02")] = struct{}{}
	return nil
}

// --- pure helpers ---------------------------------------------------------

// parseDetailPath extracts (date, symbol) from
// /api/daily-picks/{date}/{symbol}. Returns ok=false on shape
// mismatch so the handler can reply 400 with a useful message.
func parseDetailPath(path string) (date, symbol string, ok bool) {
	const prefix = "/api/daily-picks/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], strings.ToUpper(strings.TrimSpace(parts[1])), true
}

// projectPickRows converts repo rows to the wire shape, extracting
// the headline thesis from the embedded JSON. Cheap because the
// JSON is already in memory.
func projectPickRows(rows []dailypicks.PickRow) []dailyPickRow {
	out := make([]dailyPickRow, 0, len(rows))
	for _, r := range rows {
		row := dailyPickRow{
			Symbol:           r.Symbol,
			SymbolName:       r.SymbolName,
			Market:           r.Market,
			PresetKey:        r.PresetKey,
			PickDate:         r.PickDate.UTC().Format("2006-01-02"),
			AggregateVerdict: r.AggregateVerdict,
			AggregateScore:   r.AggregateScore,
			Consensus:        r.Consensus,
			HasError:         r.ErrorReason != "",
		}
		if t := extractHeadlineThesis(r.ResultJSON); t != "" {
			row.HeadlineThesis = t
		}
		out = append(out, row)
	}
	return out
}

// extractHeadlineThesis pulls the thesis of the FIRST master_report
// without doing a full unmarshal. The browse grid renders only this
// one sentence per card; parsing the full ConsultResponse 50 times
// per browse request would be wasteful. If parsing fails (malformed
// or missing field) we return empty string and let the UI fall back
// to the verdict pill alone.
func extractHeadlineThesis(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	var head struct {
		MasterReports []struct {
			Thesis string `json:"thesis"`
		} `json:"master_reports"`
	}
	if err := json.Unmarshal(blob, &head); err != nil {
		return ""
	}
	if len(head.MasterReports) == 0 {
		return ""
	}
	return strings.TrimSpace(head.MasterReports[0].Thesis)
}

// --- module-level quota state --------------------------------------------

// detailQuotaStore is the in-memory per-user detail-read tracker.
// Cleared at process restart by design — v1 is single-binary, and
// resetting on restart is a defensible UX (users get a fresh
// allotment when we ship a patch, which they'll notice as a
// surprise gift, not a regression).
//
// v1.5 follow-up: persist into a daily_pick_reads audit table so
// (a) quota survives restart and (b) it's queryable for the
// "you've read N today across 2 sessions" UX.
var (
	detailQuotaStore = make(map[string]map[string]struct{})
	detailQuotaMu    sync.Mutex
)
