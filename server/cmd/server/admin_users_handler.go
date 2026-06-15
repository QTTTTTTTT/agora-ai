// admin_users_handler.go — read-only admin user management console
// (v1).
//
// Endpoints (all gated by adminHandler.requireAdmin → role IN
// ('admin','super_admin') verified against users.role on every
// request, the same gate admin_user_roles.go uses):
//
//	GET /api/admin/users/stats        — top-of-page metrics +
//	                                    30-day signup sparkline
//	GET /api/admin/users              — paginated user list
//	GET /api/admin/users/{userId}     — single-user detail with
//	                                    subscription history and
//	                                    LLM consumption breakdown
//
// All three are READ-ONLY. Any role/tier/status mutation continues
// to live on the existing endpoints (admin_user_roles.go for role,
// kyc_handler for KYC, etc.) so this file's surface area stays
// auditably narrow.
//
// Why a separate handler file (vs growing admin_handler.go):
// admin_handler.go is already 1,300+ lines spanning a dozen
// concerns. Pulling user-console queries out keeps the SQL +
// response shapes co-located and makes the test file scope
// obvious — this is the same pattern admin_user_roles.go,
// admin_fx.go and admin_llm_resolve.go follow.

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// planTierMonthlyCents is the v1 pricing reference for MRR rollup.
// Values are USD cents per month. Free is 0; the rest are placeholder
// list-price tiers that match what plans.tsx renders to readers
// today. When a real `plan_prices` table lands the constants below
// should move there and this map can be deleted; the wire format
// (MRRCents) is unchanged so the frontend doesn't need a refactor.
var planTierMonthlyCents = map[string]int64{
	"free":       0,
	"pro":        2900,
	"premium":    9900,
	"enterprise": 29900,
}

// adminUsersListItem is one row in /api/admin/users. Field tags are
// camelCase to match the rest of the admin frontend bundle (see
// adminUserRow in admin_user_roles.go for the same convention) and
// monetary values are sent as raw cent integers — formatting belongs
// to the renderer.
type adminUsersListItem struct {
	ID                   string  `json:"id"`
	Username             string  `json:"username"`
	DisplayName          string  `json:"displayName"`
	Email                string  `json:"email"`
	Role                 string  `json:"role"`
	Status               string  `json:"status"`
	KYCStatus            string  `json:"kycStatus"`
	CreatedAt            string  `json:"createdAt"`
	LastLoginAt          *string `json:"lastLoginAt,omitempty"`
	CurrentTier          string  `json:"currentTier"`
	TierUntil            *string `json:"tierUntil,omitempty"`
	LifetimeLLMCostCents int64   `json:"lifetimeLLMCostCents"`
	LifetimeLLMCalls     int64   `json:"lifetimeLLMCalls"`
}

type adminUsersListResponse struct {
	Users []adminUsersListItem `json:"users"`
	Total int                  `json:"total"`
	Page  int                  `json:"page"`
	Size  int                  `json:"size"`
}

// adminUsersStatsResponse is the dashboard header payload. The
// 30-day signups slice is zero-filled in the handler so the frontend
// chart renders a continuous line even on quiet days; tier
// distribution only includes tiers that actually appear in active
// subscriptions so the pie chart doesn't lead with empty wedges.
type adminUsersStatsResponse struct {
	TotalUsers       int                `json:"totalUsers"`
	ActiveUsers7d    int                `json:"activeUsers7d"`
	NewUsers30d      []adminDailyCount  `json:"newUsers30d"`
	TierDistribution []adminTierCount   `json:"tierDistribution"`
	MRRCents         int64              `json:"mrrCents"`
	AsOf             string             `json:"asOf"`
}

type adminDailyCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type adminTierCount struct {
	Tier  string `json:"tier"`
	Count int    `json:"count"`
}

// adminUserDetailResponse is the per-user drawer payload. Wallet and
// advisor-credit sections are surfaced as zeroed numbers today
// because the underlying tables are empty in production — keeping
// them in the wire shape lets v2 fill them in without a frontend
// breaking change.
type adminUserDetailResponse struct {
	Profile            adminUserDetailProfile      `json:"profile"`
	Subscriptions      []adminUserSubscription     `json:"subscriptions"`
	UsageSummary       adminUserUsageSummary       `json:"usageSummary"`
	WalletBalanceCents int64                       `json:"walletBalanceCents"`
}

type adminUserDetailProfile struct {
	ID            string  `json:"id"`
	Username      string  `json:"username"`
	DisplayName   string  `json:"displayName"`
	Email         string  `json:"email"`
	Phone         string  `json:"phone,omitempty"`
	Role          string  `json:"role"`
	Status        string  `json:"status"`
	KYCStatus     string  `json:"kycStatus"`
	KYCLevel      string  `json:"kycLevel"`
	EmailVerified bool    `json:"emailVerified"`
	CreatedAt     string  `json:"createdAt"`
	LastLoginAt   *string `json:"lastLoginAt,omitempty"`
}

type adminUserSubscription struct {
	PlanTier      string  `json:"planTier"`
	Status        string  `json:"status"`
	StartDate     string  `json:"startDate"`
	EndDate       string  `json:"endDate"`
	PaymentMethod string  `json:"paymentMethod,omitempty"`
	AutoRenew     bool    `json:"autoRenew"`
}

type adminUserUsageSummary struct {
	LifetimeCalls     int64                 `json:"lifetimeCalls"`
	LifetimeCostCents int64                 `json:"lifetimeCostCents"`
	ByStep            []adminUsageBreakdown `json:"byStep"`
	ByProvider        []adminUsageBreakdown `json:"byProvider"`
	Last30d           []adminUsageDayPoint  `json:"last30d"`
}

type adminUsageBreakdown struct {
	Key       string `json:"key"`
	Calls     int64  `json:"calls"`
	CostCents int64  `json:"costCents"`
}

type adminUsageDayPoint struct {
	Date      string `json:"date"`
	Calls     int64  `json:"calls"`
	CostCents int64  `json:"costCents"`
}

func (h *adminHandler) registerUsersAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	// Path-segment ordering matters: register the bare list +
	// stats routes BEFORE the parameterised detail route, otherwise
	// http.ServeMux's longest-prefix dispatcher will route
	// /api/admin/users/stats into handleAdminUserDetail with
	// userId="stats". Go 1.22's pattern matcher handles this
	// correctly when both literal and {param} routes are
	// registered, but registering literals first is the
	// belt-and-braces convention used elsewhere in this codebase.
	mux.HandleFunc("GET /api/admin/users/stats", h.handleAdminUsersStats)
	mux.HandleFunc("GET /api/admin/users", h.handleAdminUsersList)
	mux.HandleFunc("GET /api/admin/users/{userId}", h.handleAdminUserDetail)
}

// handleAdminUsersStats responds with dashboard KPIs the operator
// sees as soon as the page loads. The four queries here all touch
// indexed columns (users.created_at, users.last_login_at,
// subscriptions.status) and complete in <50ms on the seed dataset;
// no caching is layered in because freshness is more useful than
// throughput for a dashboard with one operator.
func (h *adminHandler) handleAdminUsersStats(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	ctx := r.Context()

	totalUsers, err := scanInt(ctx, h.db,
		`SELECT COUNT(*) FROM users WHERE status <> 'deleted'`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	activeUsers7d, err := scanInt(ctx, h.db,
		`SELECT COUNT(*) FROM users
		  WHERE status <> 'deleted'
		    AND last_login_at IS NOT NULL
		    AND last_login_at > NOW() - INTERVAL '7 days'`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	signups, err := loadDailySignups(ctx, h.db, 30)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	tiers, mrr, err := loadTierDistributionAndMRR(ctx, h.db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	writeJSON(w, http.StatusOK, adminUsersStatsResponse{
		TotalUsers:       totalUsers,
		ActiveUsers7d:    activeUsers7d,
		NewUsers30d:      signups,
		TierDistribution: tiers,
		MRRCents:         mrr,
		AsOf:             time.Now().UTC().Format(time.RFC3339),
	})
}

// loadDailySignups reads the per-day new-user count for the last
// `days` days and zero-fills the gaps so the frontend sparkline
// renders a continuous line. The zero-fill is done in Go rather
// than Postgres (generate_series + LEFT JOIN) because the dataset
// is tiny and the explicit Go loop is easier to read in tests.
func loadDailySignups(ctx context.Context, db *sql.DB, days int) ([]adminDailyCount, error) {
	if days <= 0 {
		return []adminDailyCount{}, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT date_trunc('day', created_at)::date AS d, COUNT(*)::int
		  FROM users
		 WHERE status <> 'deleted'
		   AND created_at >= NOW() - ($1 || ' days')::interval
		 GROUP BY d
		 ORDER BY d ASC`, strconv.Itoa(days))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]int, days)
	for rows.Next() {
		var d time.Time
		var c int
		if err := rows.Scan(&d, &c); err != nil {
			return nil, err
		}
		seen[d.UTC().Format("2006-01-02")] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]adminDailyCount, 0, days)
	now := time.Now().UTC()
	for i := days - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		out = append(out, adminDailyCount{Date: day, Count: seen[day]})
	}
	return out, nil
}

// loadTierDistributionAndMRR returns the active-subscription
// distribution by plan_tier together with a USD-cents MRR derived
// from planTierMonthlyCents. Users without any active subscription
// are bucketed as "free". The DISTINCT ON pick is the latest
// end_date so renewing-users with overlapping rows are counted once.
func loadTierDistributionAndMRR(ctx context.Context, db *sql.DB) ([]adminTierCount, int64, error) {
	rows, err := db.QueryContext(ctx, `
		WITH latest_active AS (
			SELECT DISTINCT ON (user_id) user_id, plan_tier
			  FROM subscriptions
			 WHERE status = 'active'
			 ORDER BY user_id, end_date DESC
		),
		users_with_tier AS (
			SELECT u.id, COALESCE(la.plan_tier, 'free') AS tier
			  FROM users u
			  LEFT JOIN latest_active la ON la.user_id = u.id
			 WHERE u.status <> 'deleted'
		)
		SELECT tier, COUNT(*)::int AS c
		  FROM users_with_tier
		 GROUP BY tier
		 ORDER BY c DESC`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]adminTierCount, 0, 4)
	var mrr int64
	for rows.Next() {
		var tier string
		var c int
		if err := rows.Scan(&tier, &c); err != nil {
			return nil, 0, err
		}
		out = append(out, adminTierCount{Tier: tier, Count: c})
		if price, ok := planTierMonthlyCents[tier]; ok {
			mrr += price * int64(c)
		}
	}
	return out, mrr, rows.Err()
}

// handleAdminUsersList responds with one paginated table page.
// Pagination is OFFSET/LIMIT (acceptable while the user table is
// O(thousands); a keyset paginator becomes worthwhile only past ~10k
// rows, which we'll cross long before we need it for unrelated
// reasons). Search and tier filters are bound parameters so this
// query is safe against injection.
func (h *adminHandler) handleAdminUsersList(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	ctx := r.Context()
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	tier := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("tier")))
	page, size := parsePageAndSize(r.URL.Query().Get("page"), r.URL.Query().Get("size"))

	// Wrap q with %…% wildcards inside Go so the SQL stays the same
	// shape whether or not q is empty. The handler-side `q == ''`
	// branch in the WHERE clause means an empty string skips the
	// LIKE check entirely.
	needle := ""
	if q != "" {
		needle = "%" + q + "%"
	}

	const listSQL = `
		WITH agg AS (
			SELECT user_id,
			       SUM(cost_cents)::bigint AS lifetime_cost_cents,
			       COUNT(*)::bigint        AS lifetime_calls
			  FROM usage_entries
			 GROUP BY user_id
		),
		active_sub AS (
			SELECT DISTINCT ON (user_id) user_id, plan_tier, end_date
			  FROM subscriptions
			 WHERE status = 'active'
			 ORDER BY user_id, end_date DESC
		)
		SELECT u.id, COALESCE(u.username, ''), COALESCE(u.display_name, ''),
		       COALESCE(u.email, ''), COALESCE(u.role, 'user'), u.status,
		       COALESCE(u.kyc_status, 'unverified'),
		       u.created_at, u.last_login_at,
		       COALESCE(s.plan_tier, 'free') AS current_tier,
		       s.end_date AS tier_until,
		       COALESCE(a.lifetime_cost_cents, 0)::bigint,
		       COALESCE(a.lifetime_calls, 0)::bigint
		  FROM users u
		  LEFT JOIN active_sub s ON s.user_id = u.id
		  LEFT JOIN agg        a ON a.user_id = u.id
		 WHERE u.status <> 'deleted'
		   AND ($1 = ''
		        OR LOWER(COALESCE(u.email, ''))        LIKE $1
		        OR LOWER(COALESCE(u.display_name, '')) LIKE $1
		        OR LOWER(COALESCE(u.username, ''))     LIKE $1)
		   AND ($2 = '' OR COALESCE(s.plan_tier, 'free') = $2)
		 ORDER BY u.created_at DESC
		 LIMIT $3 OFFSET $4`

	rows, err := h.db.QueryContext(ctx, listSQL, needle, tier, size, (page-1)*size)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	defer rows.Close()

	out := make([]adminUsersListItem, 0, size)
	for rows.Next() {
		var item adminUsersListItem
		var createdAt time.Time
		var lastLogin sql.NullTime
		var tierUntil sql.NullTime
		if err := rows.Scan(&item.ID, &item.Username, &item.DisplayName,
			&item.Email, &item.Role, &item.Status, &item.KYCStatus,
			&createdAt, &lastLogin,
			&item.CurrentTier, &tierUntil,
			&item.LifetimeLLMCostCents, &item.LifetimeLLMCalls); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
		item.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		if lastLogin.Valid {
			s := lastLogin.Time.UTC().Format(time.RFC3339)
			item.LastLoginAt = &s
		}
		if tierUntil.Valid {
			s := tierUntil.Time.UTC().Format(time.RFC3339)
			item.TierUntil = &s
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	// Total is queried separately so paging doesn't need a window
	// function. With users-table cardinality being O(thousands)
	// the COUNT is cheap; we'll switch to keyset+approximate-count
	// when the cost actually shows up.
	total, err := scanInt(ctx, h.db, `
		SELECT COUNT(*) FROM users u
		  LEFT JOIN (
		    SELECT DISTINCT ON (user_id) user_id, plan_tier
		      FROM subscriptions WHERE status = 'active'
		      ORDER BY user_id, end_date DESC
		  ) s ON s.user_id = u.id
		 WHERE u.status <> 'deleted'
		   AND ($1 = ''
		        OR LOWER(COALESCE(u.email, ''))        LIKE $1
		        OR LOWER(COALESCE(u.display_name, '')) LIKE $1
		        OR LOWER(COALESCE(u.username, ''))     LIKE $1)
		   AND ($2 = '' OR COALESCE(s.plan_tier, 'free') = $2)`,
		needle, tier)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	writeJSON(w, http.StatusOK, adminUsersListResponse{
		Users: out,
		Total: total,
		Page:  page,
		Size:  size,
	})
}

// handleAdminUserDetail returns one user's full picture in a single
// payload. Four small queries instead of one big JOIN because the
// shapes are heterogeneous (profile is one row, subs and usage
// breakdowns are slices) and serialising them flat on Postgres-side
// gains us no measurable speed at this dataset size.
func (h *adminHandler) handleAdminUserDetail(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	userID := strings.TrimSpace(r.PathValue("userId"))
	if userID == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_id", "missing userId"))
		return
	}
	ctx := r.Context()

	profile, err := loadAdminUserProfile(ctx, h.db, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "user does not exist or is deleted"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	subs, err := loadAdminUserSubscriptions(ctx, h.db, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	usage, err := loadAdminUserUsageSummary(ctx, h.db, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	wallet, err := scanInt64(ctx, h.db,
		`SELECT COALESCE(SUM(balance_minor), 0)::bigint
		   FROM wallet_accounts WHERE user_id = $1`, userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}

	writeJSON(w, http.StatusOK, adminUserDetailResponse{
		Profile:            profile,
		Subscriptions:      subs,
		UsageSummary:       usage,
		WalletBalanceCents: wallet,
	})
}

func loadAdminUserProfile(ctx context.Context, db *sql.DB, userID string) (adminUserDetailProfile, error) {
	var p adminUserDetailProfile
	var createdAt time.Time
	var lastLogin sql.NullTime
	var phone sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT id, COALESCE(username, ''), COALESCE(display_name, ''),
		       COALESCE(email, ''), phone,
		       COALESCE(role, 'user'), status,
		       COALESCE(kyc_status, 'unverified'),
		       COALESCE(kyc_level, 'tier1_basic'),
		       email_verified,
		       created_at, last_login_at
		  FROM users
		 WHERE id = $1 AND status <> 'deleted'`, userID).
		Scan(&p.ID, &p.Username, &p.DisplayName, &p.Email, &phone,
			&p.Role, &p.Status, &p.KYCStatus, &p.KYCLevel, &p.EmailVerified,
			&createdAt, &lastLogin)
	if err != nil {
		return adminUserDetailProfile{}, err
	}
	if phone.Valid {
		p.Phone = phone.String
	}
	p.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	if lastLogin.Valid {
		s := lastLogin.Time.UTC().Format(time.RFC3339)
		p.LastLoginAt = &s
	}
	return p, nil
}

func loadAdminUserSubscriptions(ctx context.Context, db *sql.DB, userID string) ([]adminUserSubscription, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT plan_tier, status, start_date, end_date,
		       COALESCE(payment_method, ''), auto_renew
		  FROM subscriptions
		 WHERE user_id = $1
		 ORDER BY start_date DESC
		 LIMIT 50`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]adminUserSubscription, 0, 4)
	for rows.Next() {
		var s adminUserSubscription
		var start, end time.Time
		if err := rows.Scan(&s.PlanTier, &s.Status, &start, &end,
			&s.PaymentMethod, &s.AutoRenew); err != nil {
			return nil, err
		}
		s.StartDate = start.UTC().Format(time.RFC3339)
		s.EndDate = end.UTC().Format(time.RFC3339)
		out = append(out, s)
	}
	return out, rows.Err()
}

// loadAdminUserUsageSummary fans out three small queries against
// usage_entries (lifetime totals, breakdown by step, breakdown by
// provider, last-30-day daily). All slice fields are initialised to
// non-nil empty so the frontend can render skeleton tables without
// a null guard.
func loadAdminUserUsageSummary(ctx context.Context, db *sql.DB, userID string) (adminUserUsageSummary, error) {
	out := adminUserUsageSummary{
		ByStep:     []adminUsageBreakdown{},
		ByProvider: []adminUsageBreakdown{},
		Last30d:    []adminUsageDayPoint{},
	}

	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)::bigint, COALESCE(SUM(cost_cents), 0)::bigint
		  FROM usage_entries WHERE user_id = $1`, userID).
		Scan(&out.LifetimeCalls, &out.LifetimeCostCents); err != nil {
		return out, err
	}

	stepRows, err := db.QueryContext(ctx, `
		SELECT step_name, COUNT(*)::bigint, COALESCE(SUM(cost_cents), 0)::bigint
		  FROM usage_entries
		 WHERE user_id = $1
		 GROUP BY step_name
		 ORDER BY 3 DESC
		 LIMIT 30`, userID)
	if err != nil {
		return out, err
	}
	for stepRows.Next() {
		var b adminUsageBreakdown
		if err := stepRows.Scan(&b.Key, &b.Calls, &b.CostCents); err != nil {
			stepRows.Close()
			return out, err
		}
		out.ByStep = append(out.ByStep, b)
	}
	stepRows.Close()
	if err := stepRows.Err(); err != nil {
		return out, err
	}

	provRows, err := db.QueryContext(ctx, `
		SELECT model_provider, COUNT(*)::bigint, COALESCE(SUM(cost_cents), 0)::bigint
		  FROM usage_entries
		 WHERE user_id = $1
		 GROUP BY model_provider
		 ORDER BY 3 DESC
		 LIMIT 30`, userID)
	if err != nil {
		return out, err
	}
	for provRows.Next() {
		var b adminUsageBreakdown
		if err := provRows.Scan(&b.Key, &b.Calls, &b.CostCents); err != nil {
			provRows.Close()
			return out, err
		}
		out.ByProvider = append(out.ByProvider, b)
	}
	provRows.Close()
	if err := provRows.Err(); err != nil {
		return out, err
	}

	dayRows, err := db.QueryContext(ctx, `
		SELECT date_trunc('day', created_at)::date AS d,
		       COUNT(*)::bigint,
		       COALESCE(SUM(cost_cents), 0)::bigint
		  FROM usage_entries
		 WHERE user_id = $1
		   AND created_at >= NOW() - INTERVAL '30 days'
		 GROUP BY d
		 ORDER BY d ASC`, userID)
	if err != nil {
		return out, err
	}
	defer dayRows.Close()
	for dayRows.Next() {
		var p adminUsageDayPoint
		var d time.Time
		if err := dayRows.Scan(&d, &p.Calls, &p.CostCents); err != nil {
			return out, err
		}
		p.Date = d.UTC().Format("2006-01-02")
		out.Last30d = append(out.Last30d, p)
	}
	return out, dayRows.Err()
}

// parsePageAndSize parses ?page= and ?size= with safe defaults.
// `page` clamps to >= 1; `size` clamps to [1, 200] so a malicious
// or buggy client can't drag the server through a 1M-row OFFSET.
func parsePageAndSize(rawPage, rawSize string) (page, size int) {
	page = 1
	size = 50
	if v, err := strconv.Atoi(strings.TrimSpace(rawPage)); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(rawSize)); err == nil {
		switch {
		case v < 1:
			size = 1
		case v > 200:
			size = 200
		default:
			size = v
		}
	}
	return page, size
}

// scanInt and scanInt64 are tiny helpers that keep the handler
// bodies focused on the response shape rather than the four-line
// QueryRow-Scan dance. They take the SQL inline so the read order
// at the call site mirrors what Postgres sees.
func scanInt(ctx context.Context, db *sql.DB, q string, args ...any) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func scanInt64(ctx context.Context, db *sql.DB, q string, args ...any) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
