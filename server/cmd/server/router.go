// router.go — HTTP router assembly.
//
// Lifted out of main.go to keep `cmd/server/main.go` focused on the
// process entrypoint (config load → DB connect → service init →
// serve loop → graceful shutdown). Routing wiring is its own
// concern — `buildRouter` reads the live `*Services` graph and
// `*Config`, registers every HTTP route, and wraps the mux in the
// middleware chain (path alias → auth → CORS → request log →
// recoverer).
//
// Splitting this off:
//
//   - lets a contributor scanning main.go see the boot lifecycle
//     end-to-end without scrolling past 200 lines of route table;
//   - gives the router a stable file boundary that future
//     "register handler X" PRs can target without touching the
//     boot path (lower review surface, fewer accidental imports);
//   - keeps `pathAliasMiddleware` next to its sole caller — moving
//     it together with `buildRouter` removes a dangling util in
//     main.go that nothing else used.
//
// No behaviour change. Every handler, every middleware, and every
// nil-safe early-return is preserved verbatim from the original
// main.go block. The function signature is unchanged.

package main

import (
	"log/slog"
	"net/http"
	"strings"
)

func buildRouter(svc *Services, cfg *Config) http.Handler {
	mux := http.NewServeMux()

	// Runtime profiling endpoints (/debug/pprof/*). Off by default;
	// enabled when PPROF_ENABLED=1. See pprof.go for the rationale
	// and access patterns. Logged at startup so an SRE can confirm
	// from the boot logs whether they've got pprof on.
	if registerPprof(mux) {
		slog.Info("pprof endpoints enabled", "prefix", "/debug/pprof/")
	}

	adminHandler := newAdminHandler(svc)
	// S13 — attach the LLM router + reloader onto the admin
	// handler now that both halves of the service graph exist.
	// nil-safe: missing runtime/repo means the LLM provider
	// admin routes simply return 503 / never trigger reloads.
	if adminHandler != nil && svc != nil && svc.LLMRuntime != nil {
		adminHandler.attachLLMRuntime(svc.LLMRuntime.router, svc.LLMRuntime)
	}

	// ---- Health & meta ----
	mux.HandleFunc("GET /api/health", handleHealth(svc))
	mux.HandleFunc("GET /api/version", handleVersion())
	mux.HandleFunc("GET /api/metrics", handleMetrics(svc))
	mux.HandleFunc("POST /api/auth/register", handleRegister(svc, cfg))
	mux.HandleFunc("POST /api/auth/login", handleLogin(svc, cfg))
	mux.HandleFunc("POST /api/auth/wechat-login", handleWechatLogin(svc, cfg))
	mux.HandleFunc("POST /api/auth/logout", handleLogout(cfg))
	mux.HandleFunc("GET /api/auth/session", handleSession(svc, cfg))
	mux.HandleFunc("POST /api/auth/send-verification", handleSendVerification(svc, cfg))
	mux.HandleFunc("POST /api/auth/verify-email", handleVerifyEmail(svc, cfg))
	mux.HandleFunc("POST /api/auth/forgot-password", handleForgotPassword(svc, cfg))
	mux.HandleFunc("POST /api/auth/reset-password", handleResetPassword(svc, cfg))
	mux.HandleFunc("POST /api/auth/change-password", handleChangePassword(svc, cfg))
	// P0-6: 2FA / TOTP — registered conditionally on
	// TOTP_ENCRYPTION_KEY being set (see newTOTPHandler).
	if h := newTOTPHandler(svc, cfg); h != nil {
		h.RegisterRoutes(mux)
	}
	// P0-7: per-action biometric step-up. The endpoint is always
	// registered (no special key required); the verifier on order
	// handlers treats absence as "no proof" rather than "fail".
	if h := newStepUpHandler(cfg); h != nil {
		h.RegisterRoutes(mux)
	}
	mux.HandleFunc("GET /api/account/kyc", handleGetAccountKYC(svc))
	mux.HandleFunc("POST /api/account/kyc", handleSubmitAccountKYC(svc))

	// Sprint 4 / android-core: FCM device-token registry + push
	// fan-out hook for terminal plan transitions.
	deviceTokens := newDeviceTokensService(svc.DB)
	mux.HandleFunc("POST /api/devices/register", deviceTokens.handleRegister)
	mux.HandleFunc("POST /api/devices/unregister", deviceTokens.handleUnregister)
	if svc.WorkflowService != nil {
		svc.WorkflowService.WithPlanLifecycleNotifier(newPlanLifecycleNotifierAdapter(deviceTokens))
	}

	// ---- Real application routes ----
	if svc.SubscriptionHandler != nil {
		svc.SubscriptionHandler.RegisterRoutes(mux)
	}
	if svc.FundHandler != nil {
		svc.FundHandler.RegisterRoutes(mux)
	}
	if adminHandler != nil {
		adminHandler.RegisterRoutes(mux)
	}
	// P0-8 — audit log hash chain verifier (super-admin only).
	// Mounted independently of adminHandler so an admin handler
	// outage doesn't take the verifier offline.
	if verifyHandler := newAuditVerifyHandler(svc); verifyHandler != nil {
		verifyHandler.RegisterRoutes(mux)
	}
	// P0-3 — stop-trigger observability (super-admin only).
	if stopHandler := newStopTriggerStatusHandler(svc); stopHandler != nil {
		stopHandler.RegisterRoutes(mux)
	}
	// W1-2 — DB connection pool status (super-admin only). JSON
	// twin of the fundai_db_* gauges in /api/metrics. The Admin UI
	// can poll this for a live "pool saturation" panel without
	// having to scrape and parse Prometheus text.
	if poolHandler := newDBPoolHandler(svc, cfg); poolHandler != nil {
		poolHandler.RegisterRoutes(mux)
	}
	// W8-2 — memory re-embed queue snapshot (super-admin only).
	// JSON twin of the fundai_memreembed_* counters in /api/metrics
	// (W7-1). Handler always registers — when the queue is nil it
	// reports `enabled=false` instead of 404 so the Admin UI panel
	// can render a "re-embed disabled" state without special-casing
	// the route shape.
	if reembedHandler := newMemReembedHandler(svc); reembedHandler != nil {
		reembedHandler.RegisterRoutes(mux)
	}
	// W11-1 — embedquota.Limiter snapshot (super-admin only).
	// JSON twin of the fundai_embed_quota_* metrics in
	// /api/metrics (W6-2 / W8-1 / W9-1 / W10-1). Same nil-safe
	// shape as the memreembed handler so the Admin UI can render
	// a "limiter disabled" state without 404 special-casing.
	if quotaHandler := newEmbedQuotaHandler(svc); quotaHandler != nil {
		quotaHandler.RegisterRoutes(mux)
	}
	// P0-5 — order Cancel / Replace API.
	// P0-9 — wire the live-trading hard gate. We construct it once
	// and pass into the order-actions handler. The kill switch is
	// LIVE_TRADING_GATE_ENABLED (default true) — see
	// loadLiveTradingGateEnabled.
	gateEnabled := loadLiveTradingGateEnabled()
	gate := newLiveTradingGate(svc, cfg, gateEnabled)
	if !gateEnabled {
		slog.Warn("LIVE_TRADING_GATE_ENABLED=false — live trading hard gate is OFF (dev/test posture only)")
	}
	if orderHandler := newOrderActionsHandlerWithGate(svc, cfg, gate); orderHandler != nil {
		orderHandler.RegisterRoutes(mux)
	}
	// P0-9 — read-only readiness endpoint that the UI hits to
	// render a per-fund "live trading checklist". Registers under
	// /api/funds/{fundId}/live-readiness; nil-safe on missing svc.
	if rh := newLiveReadinessHandler(svc, cfg, gate); rh != nil {
		rh.RegisterRoutes(mux)
	}
	// P1-6 — broker-link self-service for the fund owner. Admin
	// approval routes are added as part of newAdminHandler below.
	if blh := newBrokerLinkHandler(svc); blh != nil {
		blh.RegisterRoutes(mux)
	}
	// P1-1 — fund cash-ledger read endpoint. Powers the
	// "Cash movements" tab on the fund-detail page and the
	// reconciliation pipeline.
	if clh := newCashLedgerHandler(svc); clh != nil {
		clh.RegisterRoutes(mux)
	}
	// P1-2 — funding request self-service (deposit / withdrawal).
	// Admin approve/reject lives in admin_funding.go and is
	// registered as part of newAdminHandler below.
	if fh := newFundingHandler(svc); fh != nil {
		fh.RegisterRoutes(mux)
	}
	// Fund-level settings (base_currency, …; P1-4). Hosted in its
	// own handler so adding more per-fund settings later doesn't
	// keep ballooning a single file.
	if fs := newFundSettingsHandler(svc); fs != nil {
		fs.RegisterRoutes(mux)
	}
	// S7 / P3-1 — per-fund factor-exposure read + trend.
	// Computes the six canonical factor exposures from current
	// holdings on demand; archives a snapshot when ?persist=1.
	if feh := newFactorExposureHandler(svc); feh != nil {
		feh.RegisterRoutes(mux)
	}

	// S7 / P3-2 — per-fund Value-at-Risk + Conditional VaR.
	// Computes historical / parametric / Monte Carlo VaR for the
	// 3 canonical confidence levels from nav_snapshots.daily_return;
	// archives a snapshot when ?persist=1.
	if vh := newVaRHandler(svc); vh != nil {
		vh.RegisterRoutes(mux)
	}

	// S7 / P3-3 — per-fund stress-scenario runner. Applies a
	// named scenario (asset-class / market / instrument / factor
	// shocks) to current holdings and returns the projected
	// P&L plus per-holding contributions.
	if sh := newStressHandler(svc); sh != nil {
		sh.RegisterRoutes(mux)
	}

	// S7 / P3-4 — per-fund Brinson attribution runner +
	// authenticated benchmark catalog. Admin CRUD for benchmark
	// compositions sits on adminHandler.registerBrinsonAdminRoutes.
	if bh := newBrinsonHandler(svc); bh != nil {
		bh.RegisterRoutes(mux)
	}

	// S8.1 — per-fund analyst panel runner (fundamentals /
	// sentiment / news / technical). The AnalystPanelProvider on
	// Services is the dependency-injection seam: in production
	// the wiring layer instantiates one panel per fund with
	// fund-specific LLM credentials; in tests it can pass a
	// fixed stub panel. Nil provider → /run replies 503.
	if ah := newAnalystPanelHandler(svc, svc.AnalystPanelProvider); ah != nil {
		ah.RegisterRoutes(mux, svc.AnalystPanelProvider)
	}

	// S8.2 — Bull / Bear forced debate. Reuses the analyst panel
	// to seed each round (so the same /debates/run call also
	// produces a fresh panel snapshot). DebateProvider on
	// Services is the dependency-injection seam.
	if dh := newDebateHandler(svc); dh != nil {
		dh.RegisterRoutes(mux, svc.AnalystPanelProvider, svc.DebateProvider)
	}

	// S8.4 — per-agent reputation ledger. Read-only fund routes
	// for the dashboard; admin routes (cross-fund view + rebuild
	// trigger) live on *adminHandler.
	if rh := newAgentReputationHandler(svc); rh != nil {
		rh.RegisterRoutes(mux)
	}

	// S14.B — per-fund LLM provider overrides. Owned by the fund's
	// company owner (auth via authorizeFundAccess). Nil-safe: when
	// the override repo is absent the routes stay unregistered and
	// the fund settings UI hides its override section.
	if fh := newFundLLMOverridesHandler(svc); fh != nil {
		fh.RegisterRoutes(mux)
	}
	// S9.2 read-only — fund owners can see their fund's per-step
	// workflow checkpoint timeline so they can self-diagnose the
	// "did today's report run cleanly?" question without going
	// through admin support. Resume / re-fire stays admin-only
	// because re-running a step can spend LLM budget and submit
	// broker instructions; that decision belongs with platform
	// operators (handled by /api/admin/workflow-checkpoints/resume).
	// Nil-safe: when the checkpoint repo is absent the routes stay
	// unregistered and the fund's workflow page degrades to
	// "feature unavailable".
	if wfh := newFundWorkflowCheckpointsHandler(svc); wfh != nil {
		wfh.RegisterRoutes(mux)
	}

	// Read-only LLM provider catalog scoped to a fund's owner. Lets
	// the A/B test creation UI render <select> options for picking
	// an LLM without exposing admin credential surface. Nil-safe.
	if ch := newFundLLMCatalogHandler(svc); ch != nil {
		ch.RegisterRoutes(mux)
	}

	// ---- SPA fallback: serve React static files ----
	spa := spaHandler(cfg.StaticFilesPath)
	mux.Handle("/", spa)

	// Wrap with middleware. Order from innermost (closest to mux) to
	// outermost (closest to client):
	//   pathAlias  → rewrite kebab-case URLs to camelCase
	//   auth       → resolve session cookie → user / role on ctx
	//   rateLimit  → per-IP token bucket (auth / mutate / read classes)
	//   cors       → preflight + Origin allow-list
	//   compress   → Brotli (preferred) or gzip JSON / text (NOT SSE)
	//   logger     → record method/path/status/bytes/duration
	//   recoverer  → catch panics, return 500
	//
	// rateLimit sits AFTER cors and BEFORE auth so:
	// 1) CORS preflight (OPTIONS) responses bypass the bucket
	//    (browsers send these aggressively and they cost nothing);
	// 2) /api/auth/login itself IS rate-limited (auth comes after).
	//
	// compress is between cors and logger so logger.bytesWritten
	// reflects actual on-the-wire size, and so SSE handlers (which
	// set Content-Type=text/event-stream before WriteHeader) can
	// opt out via shouldCompress's Content-Type sniff.
	rateLimitStore := newRateLimiterStore(defaultRateLimitConfig())
	var handler http.Handler = mux
	handler = pathAliasMiddleware(handler)
	handler = authMiddlewareWithKeyring(svc.DB, cfg.effectiveJWTKeyring())(handler)
	handler = rateLimitMiddleware(rateLimitStore)(handler)
	handler = corsMiddleware(cfg.CORSOrigins)(handler)
	handler = compressionMiddleware(handler)
	handler = requestLogger(svc.Metrics, handler)
	handler = recoverer(svc.Metrics, handler)

	return handler
}

// pathAliasMiddleware rewrites kebab-case API path variants to the canonical
// camelCase the handlers are registered under. Lets callers that learned the
// kebab-case spelling from older docs / informal references hit the right
// handler instead of bouncing off F9.3's JSON 404. Keep this list minimal —
// only add aliases for paths where ambiguity has actually caused confusion.
func pathAliasMiddleware(next http.Handler) http.Handler {
	aliases := map[string]string{
		"/api/ab-tests": "/api/abtests",
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for from, to := range aliases {
			if r.URL.Path == from || strings.HasPrefix(r.URL.Path, from+"/") {
				r.URL.Path = to + strings.TrimPrefix(r.URL.Path, from)
				break
			}
		}
		next.ServeHTTP(w, r)
	})
}
