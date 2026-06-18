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
//	Free   : list capped at pick_date <= today-3d AND limited to the
//	         single most-recent allowed pick_date AND clamped to top
//	         3 rows AND restricted to the `disruptive` preset only.
//	         Detail endpoint always 403s with upgrade_required.
//	Pro    : list shows today's rows; detail capped at 30 / day.
//	Premium: list shows today; detail capped at 30 / day.
//	Enterprise : list shows today; detail uncapped.
//
//	The list ENDPOINT serves the same rows to every tier — what
//	differs is the pick_date upper-bound filter, the preset
//	whitelist, and the row count cap. This preserves the
//	publisher-mode invariant ("one row, all readers") while still
//	giving paid tiers something to pay for (timeliness + breadth +
//	depth).

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fundai/server/internal/advisor"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/dailypicks"
	"github.com/fundai/server/internal/subscription"
)

// freeTierLagDays is the time-lag applied to free readers. 3d is
// the v2 product knob — short enough that the lagged data is still
// recognisably "recent", long enough that paid tiers retain a
// meaningful timeliness edge. v1 used 14d; the longer gap was
// converting too few free users because the data felt stale rather
// than tantalising.
const freeTierLagDays = 3

// freeTierVisiblePresets is the whitelist of strategy presets a
// free-tier reader can browse. Disruptive is the only entry: it
// has the shortest LLM fan-out (so it's the freshest each day)
// and Wood-style picks generate the loudest "should I upgrade?"
// signal. Any other preset on the list call returns 403.
var freeTierVisiblePresets = map[string]struct{}{
	"disruptive": {},
}

// freeTierTopN clamps the row count for free-tier list calls.
// Pricing rev (2026-06-15): bumped from 3 → 5 so the free user
// can preview enough names to evaluate fit without giving away
// the full Top 20 paid signal.
const freeTierTopN = 5

// dailyPickDetailQuotaByTier maps subscription plan to the per-day
// cap on detail reads. Tiers omitted from this map (or unknown) get
// the free tier's cap. Team / Enterprise get -1 = unlimited.
//
// Free is 0 because the free path takes a hard 403 in handleDetail
// before this map is even read; the entry stays as a defense-in-
// depth in case that branch is ever bypassed.
var dailyPickDetailQuotaByTier = map[subscription.PlanTier]int{
	subscription.PlanFree:       0, // free can't reach today's rows so this is unreachable; we keep it 0 for safety
	subscription.PlanPro:        30,
	subscription.PlanPremium:    30,
	subscription.PlanTeam:       -1,
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

	// Free-tier guard #1 — preset whitelist. Hard 403 so a
	// URL-tampering reader can't bypass the FE chip-hiding. The
	// FE catches this and resets the URL to ?preset=disruptive.
	if tier == subscription.PlanFree {
		if _, allowed := freeTierVisiblePresets[preset]; !allowed {
			writeJSON(w, http.StatusForbidden, errorPayload(
				"forbidden_preset",
				"this preset requires a paid plan",
			))
			return
		}
	}

	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	offset, _ := strconv.Atoi(q.Get("offset"))

	// Free-tier guard #2 — clamp pagination. The chip UI hides
	// multi-page nav anyway; URL tampering must produce the same
	// row set the FE renders.
	if tier == subscription.PlanFree {
		limit = freeTierTopN
		offset = 0
	}

	// Tier-driven time lag is the SECURITY-CRITICAL slot —
	// the SQL upper bound here is what gates free users away
	// from today's rows. Don't move this filter to the front
	// end; the API must enforce.
	maxDate := h.maxAllowedDateForTier(tier)
	// requestedDate optionally narrows further. If the requested
	// day is later than the tier permits, clamp instead of 404 —
	// the frontend gets back "the newest you're allowed" along
	// with the explicit "you wanted X but get Y" signal.
	//
	// Free tier ignores ?date= entirely — guard #3 below pins to
	// the single most-recent allowed pick_date so the UX matches
	// the "history: latest period only" product copy.
	var requestedDate time.Time
	if tier != subscription.PlanFree {
		if s := strings.TrimSpace(q.Get("date")); s != "" {
			if d, err := time.Parse("2006-01-02", s); err == nil {
				requestedDate = d
			}
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

	// Free-tier guard #3 — pin to the single most-recent allowed
	// pick_date. Without this, a "limit 3" run sorted by
	// (pick_date DESC, score DESC) could spill into the previous
	// day if today's slice has fewer than 3 rows, producing a
	// confusing mixed-date card list. Explicit pinning is two
	// queries instead of one but makes the slice deterministic.
	if tier == subscription.PlanFree && pinDate.IsZero() {
		latest, lerr := h.picks.List(r.Context(), dailypicks.ListFilter{
			Market:      market,
			PresetKey:   preset,
			MaxPickDate: maxDate,
			Limit:       1,
		})
		if lerr != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", lerr.Error()))
			return
		}
		if len(latest) == 0 {
			// No rows in window — return an explicit empty
			// payload (with tier metadata) so the FE can
			// render its empty state without a second
			// fetch.
			writeJSON(w, http.StatusOK, dailyPicksListResponse{
				Picks:                   nil,
				TotalCount:              0,
				Tier:                    string(tier),
				FreeLagDays:             freeTierLagDays,
				UpgradeRequiredForToday: true,
			})
			return
		}
		pinDate = latest[0].PickDate
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

	// Free tier never sees indicator details. The FE renders a
	// "🔒 Upgrade to see indicators" CTA in place of the open-
	// detail button, so this branch is only reachable via direct
	// URL access. Hard 403 so the upgrade story stays consistent
	// across the chip UI and the address bar.
	if tier == subscription.PlanFree {
		writeJSON(w, http.StatusForbidden, errorPayload(
			"upgrade_required",
			"indicator details are not available on the free tier",
		))
		return
	}

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
	// Tier gate — same maxDate as the list view. Paid users that
	// guess a future URL get the upgrade pointer rather than a
	// 404 so the error story is uniform.
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

// ---------------------------------------------------------------------------
// GET /api/daily-picks/status — 进度面板用
//
// 聚合：
//   - 每个 active 的 daily_pick_watchlists 的 symbols 总数 (total)
//   - 当天 daily_picks 已写入的行数 (done) — 当天 = newestAvailable(date)
//     不过为了让用户看到「今天定时器跑没跑」，我们用 today := UTC date
//     直接查；如果 today 行数 = 0 但 yesterday = total，说明今日定时器
//     还没触发（pending）。
//   - 最近一次 computed_at（per preset）
//   - 当天的 error_count（error_reason IS NOT NULL）
//
// 状态推断（前端按 status 字段 + per-preset 进度自己渲染颜色）：
//   - "completed":   today done == today total（全部跑完）
//   - "running":     0 < done < total 且 MAX(computed_at) 距今 < 10 分钟
//   - "pending":     today done == 0（定时器今天还没启动）
//   - "stalled":     0 < done < total 且 MAX(computed_at) 距今 >= 10 分钟
//
// 该端点对所有登录用户开放（不强 plan gate）——它只返回元数据，不返回
// pick 内容。
type dailyPicksStatusPresetView struct {
	Preset      string  `json:"preset"`
	Market      string  `json:"market"`
	Total       int     `json:"total"`
	Done        int     `json:"done"`
	ErrorCount  int     `json:"error_count"`
	LastRunAt   *string `json:"last_run_at,omitempty"`   // RFC-3339, 该 preset 最近一次 computed_at
	Status      string  `json:"status"`                  // pending / running / stalled / completed
}

type dailyPicksStatusResponse struct {
	Today    string                       `json:"today"` // YYYY-MM-DD UTC
	Overall  string                       `json:"overall"` // 同上四种
	TotalAll int                          `json:"total_all"`
	DoneAll  int                          `json:"done_all"`
	Presets  []dailyPicksStatusPresetView `json:"presets"`
}

func (h *dailyPicksHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "daily-picks not wired"))
		return
	}
	ctx := r.Context()
	today := time.Now().UTC().Format("2006-01-02")

	// 1) 取每个 active watchlist 的 (preset, market, total)
	wlRows, err := h.db.QueryContext(ctx, `
		SELECT preset_key, market, COALESCE(array_length(symbols, 1), 0)
		  FROM daily_pick_watchlists
		 WHERE active = TRUE
		 ORDER BY preset_key, market`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("status_failed", err.Error()))
		return
	}
	defer wlRows.Close()
	type wlKey struct{ preset, market string }
	totals := make(map[wlKey]int)
	for wlRows.Next() {
		var preset, market string
		var n int
		if err := wlRows.Scan(&preset, &market, &n); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("status_scan_failed", err.Error()))
			return
		}
		totals[wlKey{preset, market}] += n
	}

	// 2) 取当天每个 (preset, market) 已完成数 + 错误数 + last computed_at
	doneRows, err := h.db.QueryContext(ctx, `
		SELECT preset_key, market,
		       COUNT(*),
		       COUNT(*) FILTER (WHERE error_reason IS NOT NULL AND error_reason <> ''),
		       MAX(computed_at)
		  FROM daily_picks
		 WHERE pick_date = $1::date
		 GROUP BY preset_key, market`, today)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("done_failed", err.Error()))
		return
	}
	defer doneRows.Close()
	type doneStat struct {
		done, errCount int
		lastAt         sql.NullTime
	}
	dones := make(map[wlKey]doneStat)
	for doneRows.Next() {
		var preset, market string
		var done, ec int
		var last sql.NullTime
		if err := doneRows.Scan(&preset, &market, &done, &ec, &last); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("done_scan_failed", err.Error()))
			return
		}
		dones[wlKey{preset, market}] = doneStat{done: done, errCount: ec, lastAt: last}
	}

	now := time.Now().UTC()
	out := dailyPicksStatusResponse{Today: today, Presets: []dailyPicksStatusPresetView{}}
	totalAll := 0
	doneAll := 0
	anyRunning := false
	anyStalled := false
	allCompleted := true

	// preset 排序：disruptive / conservative / garp / macro 与前端 UI 一致
	order := []string{"disruptive", "conservative", "garp", "macro"}
	rank := func(p string) int {
		for i, k := range order {
			if k == p {
				return i
			}
		}
		return len(order) + 1
	}
	keys := make([]wlKey, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := rank(keys[i].preset), rank(keys[j].preset)
		if ri != rj {
			return ri < rj
		}
		if keys[i].preset != keys[j].preset {
			return keys[i].preset < keys[j].preset
		}
		return keys[i].market < keys[j].market
	})

	for _, k := range keys {
		total := totals[k]
		ds := dones[k]
		view := dailyPicksStatusPresetView{
			Preset:     k.preset,
			Market:     k.market,
			Total:      total,
			Done:       ds.done,
			ErrorCount: ds.errCount,
		}
		if ds.lastAt.Valid {
			s := ds.lastAt.Time.UTC().Format(time.RFC3339)
			view.LastRunAt = &s
		}
		// 状态推断
		switch {
		case total > 0 && ds.done >= total:
			view.Status = "completed"
		case ds.done == 0:
			view.Status = "pending"
			allCompleted = false
		case ds.lastAt.Valid && now.Sub(ds.lastAt.Time) < 10*time.Minute:
			view.Status = "running"
			anyRunning = true
			allCompleted = false
		default:
			view.Status = "stalled"
			anyStalled = true
			allCompleted = false
		}
		out.Presets = append(out.Presets, view)
		totalAll += total
		doneAll += ds.done
	}

	out.TotalAll = totalAll
	out.DoneAll = doneAll
	switch {
	case totalAll == 0:
		out.Overall = "pending"
	case allCompleted:
		out.Overall = "completed"
	case anyRunning:
		out.Overall = "running"
	case anyStalled:
		out.Overall = "stalled"
	default:
		out.Overall = "pending"
	}

	writeJSON(w, http.StatusOK, out)
}

