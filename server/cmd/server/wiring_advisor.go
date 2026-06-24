// wiring_advisor.go — assemble the /advisor service graph at boot.
//
// Wires:
//
//   * advisor.Repo               — reads the four advisor_* tables.
//   * advisor.Service            — request-time orchestrator.
//   * MasterPanelBuilder         — closure that materialises a panel
//                                  for a requested set of master keys
//                                  using the shared LLM client.
//   * FundamentalsLoader         — adapts the existing
//                                  fundamental.Fetcher into the shape
//                                  the advisor service expects.
//
// Tactic panel builder is intentionally left unwired in Phase 1 —
// the service handles `cn_short` presets by returning
// advisor.ErrUnsupportedPreset which the handler maps to 501.
// Phase 4 lands the tactic builder + lights up the cn_short preset.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/advisor"
	"github.com/fundai/server/internal/agent"
	"github.com/fundai/server/internal/cnmarketstructure"
	"github.com/fundai/server/internal/compliance"
	"github.com/fundai/server/internal/dailypicks"
	"github.com/fundai/server/internal/fundamental"
	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/repository"
)

// advisorPicksAdapter satisfies advisor.PicksRepoIface by
// forwarding to a concrete *dailypicks.Repo. Lives in the wiring
// layer because that's the seam where the field-by-field shape
// mapping (advisor.PicksUpsertInput → dailypicks.SaveInput) is
// owned — keeping it here means the advisor package doesn't take a
// build-time import on dailypicks (and vice-versa), which avoids
// an import cycle once the loop reaches for advisor.Service.
type advisorPicksAdapter struct {
	repo *dailypicks.Repo
}

// UpsertPick projects the advisor-side input shape onto the
// dailypicks-side input shape. Field-for-field with one
// trivial type cast (no nil handling because both sides are
// concrete value types — the only failure mode is the underlying
// SQL UPSERT, which we propagate verbatim).
func (a advisorPicksAdapter) UpsertPick(ctx context.Context, in advisor.PicksUpsertInput) (int64, error) {
	if a.repo == nil {
		return 0, errors.New("advisor_picks_adapter: nil repo")
	}
	return a.repo.UpsertPick(ctx, dailypicks.SaveInput{
		Symbol:           in.Symbol,
		SymbolName:       in.SymbolName,
		Market:           in.Market,
		PresetKey:        in.PresetKey,
		PickDate:         in.PickDate,
		ResultJSON:       in.ResultJSON,
		AggregateVerdict: in.AggregateVerdict,
		AggregateScore:   in.AggregateScore,
		Consensus:        in.Consensus,
		LLMCostUSD:       in.LLMCostUSD,
		ErrorReason:      in.ErrorReason,
	})
}

// advisorHistoryLookback is the default number of years of historical
// financial data the advisor pulls per consultation. Buffett's
// "10-year ROE average" demands ~10y; lighter personas (Lynch, GARP)
// are fine with 5y. We always request 10y and let downstream
// personas project a window down as needed — the upstream call is
// the same regardless and the cache hits are equally good.
const advisorHistoryLookback = 10

// buildAdvisorService constructs the request-time service the
// HTTP handler depends on. Returns nil when the DB isn't wired
// so the route stays unregistered in degraded boots (e.g. tests
// running without a Postgres connection).
func buildAdvisorService(svc *Services) *advisor.Service {
	if svc == nil || svc.DB == nil {
		return nil
	}
	repo := advisor.NewRepo(svc.DB)
	if repo == nil {
		return nil
	}

	personas, err := agent.LoadMasterPersonas()
	if err != nil {
		// We log + degrade rather than panic: persona files are
		// shipped with the binary, so a load failure means the
		// build is broken. The service still exists so list /
		// detail endpoints serve historical rows; only Consult
		// returns ErrNotReady.
		slog.Error("advisor: failed to load master personas", "err", err)
		return advisor.NewService(repo)
	}

	masterBuilder := newMasterPanelBuilder(svc, personas)
	loader := newAdvisorFundamentalsLoader(svc, personas)

	options := []advisor.ServiceOption{
		advisor.WithMasterPanelBuilder(masterBuilder),
		advisor.WithFundamentalsLoader(loader),
		advisor.WithComplianceMode(svc.ComplianceMode),
		advisor.WithPhraseViolationSink(advisorViolationSink(svc.ComplianceRepo, svc.Metrics)),
	}

	// Technical loader — best-effort. Without an OHLC fetcher
	// configured (LOCAL_DEV_DISABLE_OHLC=1 or all upstreams
	// unwired) the masters simply see no "technical snapshot"
	// section in their prompt, and the prompt rule 9 covers the
	// missing-block case gracefully. Wired here rather than in
	// main.go because the advisor service is the only consumer
	// — keeping construction local to the service that uses it
	// avoids polluting Services with a field used by exactly one
	// caller.
	if techLoader := newAdvisorTechnicalLoader(); techLoader != nil {
		options = append(options, advisor.WithTechnicalLoader(techLoader))
	}

	// Publisher mode (/daily-picks surface). When the
	// daily_picks tables exist (migration 106 applied), wire the
	// shared-cache repo so advisor.Service.PublishConsult is
	// callable by the daily picks loop. Without this wiring
	// PublishConsult returns ErrNotReady and the loop logs +
	// no-ops.
	if svc.DailyPicksRepo != nil {
		options = append(options,
			advisor.WithPicksRepo(advisorPicksAdapter{repo: svc.DailyPicksRepo}),
		)
	}

	// Tactic side — only wired when the cn_tactics persona files
	// load cleanly. The data loader is best-effort: when the
	// cnmarketstructure provider isn't configured the loader
	// becomes a passthrough and tactic agents SKIP with
	// data_unavailable on every check.
	if tacticPersonas, err := agent.LoadTacticPersonas(); err == nil && len(tacticPersonas) > 0 {
		options = append(options,
			advisor.WithTacticPanelBuilder(newTacticPanelBuilder(svc, tacticPersonas)),
		)
		if loader := newAdvisorTacticDataLoader(svc); loader != nil {
			options = append(options, advisor.WithTacticDataLoader(loader))
		}
	} else if err != nil {
		slog.Error("advisor: failed to load tactic personas", "err", err)
	}

	return advisor.NewService(repo, options...)
}

// advisorViolationSink wires the advisor.Service's phrase
// scanner audit hook into compliance_phrase_violations. Returns
// nil when the repo is unwired so the service degrades to a
// no-op sink (rewrites still happen, just no audit trail).
//
// We deliberately fire-and-forget on the DB write: a failed
// audit insert MUST NOT fail the consultation. The user already
// got the redacted output; losing the audit row is a soft
// failure we log and move on from.
func advisorViolationSink(repo *repository.ComplianceRepo, metrics *serverMetrics) advisor.PhraseViolationSink {
	if repo == nil {
		return nil
	}
	return func(ctx context.Context, userID, surface, sourceEntity, sourceID, redacted string, violations []compliance.Violation) {
		if len(violations) == 0 {
			return
		}
		// B2 — per-pattern counter. Fired BEFORE the DB write so
		// even a failing audit-row insert still updates the
		// metric (the metric is the alerting signal; the audit
		// row is the forensic trail). Layer is the surface key
		// passed by the scanner — usually "advisor", but the
		// daily-picks loop reuses the same sink with surface
		// "daily_picks" so the metric naturally segments.
		layer := strings.TrimSpace(surface)
		if layer == "" {
			layer = "advisor"
		}
		for _, v := range violations {
			metrics.RecordComplianceFilterBlock(v.Rule, layer)
		}
		rows := make([]repository.PhraseViolationRow, 0, len(violations))
		for _, v := range violations {
			row := repository.PhraseViolationRow{
				Surface:        surface,
				Rule:           v.Rule,
				OriginalPhrase: v.Phrase,
				Replacement:    v.Replacement,
			}
			if userID != "" {
				row.UserID.Valid = true
				row.UserID.String = userID
			}
			if redacted != "" {
				row.FullRedacted.Valid = true
				row.FullRedacted.String = redacted
			}
			if sourceEntity != "" {
				row.SourceEntity.Valid = true
				row.SourceEntity.String = sourceEntity
			}
			if sourceID != "" {
				row.SourceID.Valid = true
				row.SourceID.String = sourceID
			}
			rows = append(rows, row)
		}
		// Detach from the request context — the consultation may
		// have returned by the time the insert lands. 3s ceiling
		// keeps a slow DB from holding the goroutine forever.
		writeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := repo.InsertPhraseViolations(writeCtx, rows); err != nil {
			slog.Warn("advisor: failed to audit phrase violation",
				"err", err, "count", len(rows), "surface", surface, "source_entity", sourceEntity)
		}
	}
}

// newTacticPanelBuilder returns a closure that materialises a fresh
// TacticPanel for the requested keys. Identical caching reasoning
// as newMasterPanelBuilder: cheap to construct, persona keys vary
// per request, deterministic clock injection makes test fixtures
// reproducible.
//
// Phase B-3: ctx + userID are accepted but only userID is forwarded
// into the LLMAdapter (the LLM client itself is constructed per
// panel — re-binding it per call would defeat the per-fund budget
// tracking). The ctx parameter exists so future per-request
// authorisation checks (e.g. "did the user disable BYOK in
// settings between Check and Consume?") have a hook point.
func newTacticPanelBuilder(svc *Services, personas map[string]agent.TacticPersona) advisor.TacticPanelBuilder {
	return func(_ context.Context, userID string, tacticKeys []string) (*agent.TacticPanel, error) {
		if len(tacticKeys) == 0 {
			return nil, errors.New("advisor: no tactic keys requested")
		}
		llmClient := userScopedAdvisorLLM(svc, userID, "advisor_tactic")

		var agents []*agent.TacticAgent
		var missing []string
		for _, key := range tacticKeys {
			persona, ok := personas[key]
			if !ok {
				missing = append(missing, key)
				continue
			}
			a, err := agent.NewTacticAgent(persona, llmClient)
			if err != nil {
				return nil, fmt.Errorf("advisor: build tactic %q: %w", key, err)
			}
			agents = append(agents, a)
		}
		if len(agents) == 0 {
			return nil, fmt.Errorf("advisor: no known tactic personas in %v (missing=%v)", tacticKeys, missing)
		}
		if len(missing) > 0 {
			slog.Warn("advisor: skipped unknown tactic keys", "missing", missing)
		}
		return agent.NewTacticPanel(agents), nil
	}
}

// newAdvisorTacticDataLoader returns the structural-data fetcher
// the tactic panel uses. Returns nil when the CN market structure
// provider isn't wired — tactic agents then SKIP everything with
// data_unavailable.
func newAdvisorTacticDataLoader(svc *Services) advisor.TacticDataLoader {
	provider := svc.CNMarketStructureProvider
	if provider == nil {
		return nil
	}
	return func(ctx context.Context, in agent.TacticInput) (agent.TacticInput, error) {
		// Intraday snapshot — gate every must-have + red-line.
		if snap, err := provider.FetchIntraday(ctx, in.Symbol); err == nil {
			in.Intraday = snap
			// ST tag from the snapshot doubles as a hard-risk
			// failure so every tactic short-circuits SKIP.
			if snap != nil && snap.IsST {
				in.HardRiskFailures = append(in.HardRiskFailures, "ST/退市风险警示")
			}
		}
		// Market regime — gate against fried-board rates etc.
		if regime, err := provider.FetchMarketRegime(ctx); err == nil {
			in.Regime = regime
		}
		// Sector strength — used by some tactics' must-haves
		// ("属于当日涨幅榜前 3 板块").
		if sectors, err := provider.FetchSectorStrength(ctx, 20); err == nil {
			in.Sectors = sectors
		}
		return in, nil
	}
}

// newMasterPanelBuilder returns a closure that materialises a fresh
// MasterPanel for the requested keys on every call. We don't cache
// panels because:
//
//  1. They're cheap to construct (no I/O at construction time).
//  2. Persona keys can change across requests for the same user
//     (preset switching), so a cache keyed on user_id would still
//     miss most of the time.
//  3. Clock injection per-request is useful for deterministic
//     tests that need to control GeneratedAt.
//
// Phase B-3: ctx + userID are accepted so the LLMAdapter can stamp
// ChatRequest.UserID and the llm.UserOverrideHook can swap in the
// user's BYOK key for this consultation. An empty userID degrades
// gracefully (the hook returns nil and the chain falls through to
// fund / platform defaults).
func newMasterPanelBuilder(svc *Services, personas map[string]agent.MasterPersona) advisor.MasterPanelBuilder {
	return func(_ context.Context, userID string, masterKeys []string) (*agent.MasterPanel, error) {
		if len(masterKeys) == 0 {
			return nil, errors.New("advisor: no master keys requested")
		}
		// Reuse the same LLM adapter the analyst panel uses so
		// cost telemetry and provider routing are unified. The
		// step label "advisor_master" lets the per-step health
		// dashboard distinguish advisor traffic from fund traffic.
		llmClient := userScopedAdvisorLLM(svc, userID, "advisor_master")

		var agents []*agent.MasterAgent
		var missing []string
		for _, key := range masterKeys {
			persona, ok := personas[key]
			if !ok {
				missing = append(missing, key)
				continue
			}
			a, err := agent.NewMasterAgent(persona, llmClient)
			if err != nil {
				return nil, fmt.Errorf("advisor: build master %q: %w", key, err)
			}
			agents = append(agents, a)
		}
		if len(agents) == 0 {
			return nil, fmt.Errorf("advisor: no known master personas in %v (missing=%v)", masterKeys, missing)
		}
		if len(missing) > 0 {
			slog.Warn("advisor: skipped unknown master keys", "missing", missing)
		}
		return agent.NewMasterPanel(agents), nil
	}
}

// userScopedAdvisorLLM builds the LLMAdapter the advisor panels
// use for the given user. Mirrors agentLLMForFund but stamps
// ChatRequest.UserID via WithLLMAdapterUser instead of FundID, so
// the llm.UserOverrideHook can route the call through the user's
// own BYOK key when one is registered. When LLMRuntime isn't
// wired (system has no API keys configured) the adapter is nil
// and the master/tactic agents fall back to their deterministic
// rule paths — same degraded behaviour fund-mode already has.
//
// fundID is intentionally empty for advisor traffic. The budget
// gate (llm_budgets) and usage ledger (usage_entries) both store
// fund_id as a UUID REFERENCES funds(id), so a string sentinel
// like "advisor" is rejected by Postgres with 22P02 and shorts
// out the LLM call. Per-step observability uses the stepName
// label ("advisor_master" / "advisor_tactic"), which usage_entries
// already buckets by in its TEXT step_name column.
func userScopedAdvisorLLM(svc *Services, userID, stepName string) agent.LLMClient {
	if svc == nil || svc.LLMRuntime == nil || svc.LLMRuntime.client == nil {
		return nil
	}
	opts := []agent.LLMAdapterOption{
		agent.WithLLMAdapterStep(stepName),
	}
	if stepName == "advisor_master" {
		// Master reports include thesis, reasons, risks and persona-specific
		// fields, but DeepSeek's prompt-only JSON mode can spend hidden
		// reasoning tokens before emitting visible JSON. 2048 caused many
		// empty/truncated replies in the daily_picks batch; 4096 preserves
		// compact prompts while giving enough room to close the JSON object.
		opts = append(opts,
			agent.WithLLMAdapterMaxTokens(4096),
			agent.WithLLMAdapterTemperature(0.2),
		)
	}
	if userID != "" {
		opts = append(opts, agent.WithLLMAdapterUser(userID))
	}
	return agent.NewLLMAdapter(svc.LLMRuntime.client, "", opts...)
}

// newAdvisorFundamentalsLoader adapts the wired fundamental.Fetcher
// into the simpler shape advisor.Service expects. Returns nil-safe
// closure when no fetcher is wired (master prompts then quote
// "data_unavailable" for every quant criterion, which is honest).
//
// When the historical fetcher is wired (svc.advisorFundamentalHistoryFetcher),
// the loader best-effort enriches the block with up to advisorHistoryLookback
// years of YearlyMetrics. History failures are swallowed silently so the
// snapshot still reaches the master prompt.
//
// The `personas` argument is reserved for future per-persona
// fundamental shaping (e.g. Lynch wanting Q-over-Q earnings, Marks
// wanting credit spreads). Today it's threaded so we can adopt that
// behavior without changing the function signature.
func newAdvisorFundamentalsLoader(svc *Services, _ map[string]agent.MasterPersona) advisor.FundamentalsLoader {
	fetcher := svc.advisorFundamentalFetcher
	if fetcher == nil {
		return nil
	}
	historyFetcher := svc.advisorFundamentalHistoryFetcher
	return func(ctx context.Context, symbol, market, _ string) (*agent.FundamentalsBlock, error) {
		req := fundamental.FetchRequest{Symbol: symbol, Market: market}
		metrics, err := fetcher.Fetch(ctx, req)
		if err != nil {
			if errors.Is(err, fundamental.ErrNoData) || errors.Is(err, fundamental.ErrNoProvider) {
				return nil, nil
			}
			return nil, err
		}
		if metrics == nil {
			return nil, nil
		}
		// Best-effort historical enrichment. Failures are
		// silent — the masters fall back to data_unavailable.
		if historyFetcher != nil {
			fundamental.EnrichWithHistory(ctx, metrics, historyFetcher, req, advisorHistoryLookback)
		}
		// Render metrics into the same canonical map the
		// FundamentalsAnalyst consumes. We deliberately keep
		// the keys consistent so a UI debugging tool can
		// inspect both surfaces without translating.
		m := map[string]float64{}
		if metrics.PE > 0 {
			m["pe"] = metrics.PE
		}
		if metrics.ForwardPE > 0 {
			m["forward_pe"] = metrics.ForwardPE
		}
		if metrics.PB > 0 {
			m["pb"] = metrics.PB
		}
		if metrics.DividendYield > 0 {
			m["dividend_yield"] = metrics.DividendYield
		}
		if metrics.ProfitMargin != 0 {
			m["profit_margin"] = metrics.ProfitMargin
		}
		if metrics.OperatingMargin != 0 {
			m["operating_margin"] = metrics.OperatingMargin
		}
		if metrics.ReturnOnEquity != 0 {
			m["roe"] = metrics.ReturnOnEquity
		}
		if metrics.RevenueGrowth != 0 {
			m["revenue_growth_yoy"] = metrics.RevenueGrowth
		}
		if metrics.EarningsGrowth != 0 {
			m["earnings_growth_yoy"] = metrics.EarningsGrowth
		}
		// _latest carries the YoY from the most recent interim
		// (e.g. 2026-Q1) when fresher than the annual. Surfaces
		// timely turnaround / acceleration signal to growth /
		// momentum personas (Wood, Lynch fast-grower, breakout
		// tactics). Skipped when sidecar didn't ship it.
		if metrics.RevenueGrowthLatest != 0 {
			m["revenue_growth_yoy_latest"] = metrics.RevenueGrowthLatest
		}
		if metrics.EarningsGrowthLatest != 0 {
			m["earnings_growth_yoy_latest"] = metrics.EarningsGrowthLatest
		}
		if metrics.DebtToEquity != 0 {
			m["debt_to_equity"] = metrics.DebtToEquity
		}
		if metrics.MarketCap > 0 {
			m["market_cap"] = metrics.MarketCap
		}
		if metrics.Beta != 0 {
			m["beta"] = metrics.Beta
		}
		// Citation-grade metadata from 业绩快报: absolute revenue /
		// net income (so the LLM can show its work and a reviewer
		// can re-derive the growth %), QoQ deltas (momentum reversal
		// detector — pure YoY can't see Q4→Q1 cooling), and the
		// latest gross margin (price-war signal that profit margin
		// alone may lag). All gated on non-zero so the prompt stays
		// clean when the sidecar didn't ship them.
		if metrics.LatestRevenue > 0 {
			m["latest_revenue"] = metrics.LatestRevenue
		}
		if metrics.LatestNetIncome != 0 {
			m["latest_net_income"] = metrics.LatestNetIncome
		}
		if metrics.LatestRevenueQoQ != 0 {
			m["latest_revenue_qoq"] = metrics.LatestRevenueQoQ
		}
		if metrics.LatestNetIncomeQoQ != 0 {
			m["latest_net_income_qoq"] = metrics.LatestNetIncomeQoQ
		}
		if metrics.GrossMarginLatest != 0 {
			m["gross_margin_latest"] = metrics.GrossMarginLatest
		}
		block := &agent.FundamentalsBlock{
			Name:               metrics.Name,
			AnnualPeriod:       metrics.AnnualPeriod,
			LatestPeriod:       metrics.LatestPeriod,
			ListingDate:        metrics.ListingDate,
			ListingYears:       metrics.ListingYears,
			LatestAnnounceDate: metrics.LatestAnnounceDate,
			LatestSource:       metrics.LatestSource,
			Metrics:            m,
		}
		if len(metrics.History) > 0 {
			block.History = make([]agent.YearlyMetricsLite, 0, len(metrics.History))
			for _, y := range metrics.History {
				block.History = append(block.History, agent.YearlyMetricsLite{
					Year:              y.Year,
					ReturnOnEquity:    y.ReturnOnEquity,
					ReturnOnCapital:   y.ReturnOnCapital,
					GrossMargin:       y.GrossMargin,
					OperatingMargin:   y.OperatingMargin,
					ProfitMargin:      y.ProfitMargin,
					FreeCashFlow:      y.FreeCashFlow,
					EPS:               y.EPS,
					BookValuePerShare: y.BookValuePerShare,
					DividendPerShare:  y.DividendPerShare,
					CurrentRatio:      y.CurrentRatio,
					DebtToEquity:      y.DebtToEquity,
					RevenueGrowthYoY:  y.RevenueGrowthYoY,
					EarningsGrowthYoY: y.EarningsGrowthYoY,
				})
			}
			// Backfill the most recent year's missing fields from
			// the single-period snapshot. Yahoo's quoteSummary
			// historical modules are increasingly rate-limited
			// (HTTP 429), which means the per-year rows often only
			// carry ProfitMargin and the rest fall to 0. Without
			// this backfill the master prompt sees `roe=0% gross=0%
			// op=0% fcf=0 …` even when fundamentals.* has the live
			// values, which makes the deterministic rule_based_prior
			// pre-check come back UNKNOWN on every criterion and the
			// LLM correctly defaults every verdict to HOLD.
			//
			// We only patch the LATEST year (index 0 since History is
			// sorted most-recent-first) and only when the field is
			// the literal zero — preserving any actual historical
			// values the fetcher did manage to retrieve.
			if len(block.History) > 0 {
				h := &block.History[0]
				if h.ReturnOnEquity == 0 && metrics.ReturnOnEquity != 0 {
					h.ReturnOnEquity = metrics.ReturnOnEquity
				}
				if h.GrossMargin == 0 && metrics.GrossMarginLatest != 0 {
					h.GrossMargin = metrics.GrossMarginLatest
				}
				if h.OperatingMargin == 0 && metrics.OperatingMargin != 0 {
					h.OperatingMargin = metrics.OperatingMargin
				}
				if h.ProfitMargin == 0 && metrics.ProfitMargin != 0 {
					h.ProfitMargin = metrics.ProfitMargin
				}
				if h.RevenueGrowthYoY == 0 && metrics.RevenueGrowth != 0 {
					h.RevenueGrowthYoY = metrics.RevenueGrowth
				}
				if h.EarningsGrowthYoY == 0 && metrics.EarningsGrowth != 0 {
					h.EarningsGrowthYoY = metrics.EarningsGrowth
				}
				if h.DebtToEquity == 0 && metrics.DebtToEquity != 0 {
					h.DebtToEquity = metrics.DebtToEquity
				}
			}
		} else if metrics.ReturnOnEquity != 0 || metrics.ProfitMargin != 0 ||
			metrics.OperatingMargin != 0 || metrics.GrossMarginLatest != 0 ||
			metrics.RevenueGrowth != 0 || metrics.EarningsGrowth != 0 ||
			metrics.DebtToEquity != 0 {
			// History fetcher was unwired or failed entirely (very
			// common when Yahoo quoteSummary is throttled). Synthesise
			// a single-row history from the live snapshot so the
			// rule_based_prior layer has at least one usable data
			// point per criterion instead of returning UNKNOWN
			// across the board.
			block.History = []agent.YearlyMetricsLite{{
				Year:              time.Now().UTC().Year(),
				ReturnOnEquity:    metrics.ReturnOnEquity,
				GrossMargin:       metrics.GrossMarginLatest,
				OperatingMargin:   metrics.OperatingMargin,
				ProfitMargin:      metrics.ProfitMargin,
				DebtToEquity:      metrics.DebtToEquity,
				RevenueGrowthYoY:  metrics.RevenueGrowth,
				EarningsGrowthYoY: metrics.EarningsGrowth,
			}}
		}
		return block, nil
	}
}

// buildAdvisorFundamentalHistoryFetcherFromEnv assembles the multi-
// year financial fetcher the advisor service uses to satisfy
// Buffett-grade criteria like ROE_10yr_avg. Sister of
// buildFundamentalFetcherFromEnv; nil-degradable.
//
// Env knobs:
//
//	FUNDAMENTAL_HISTORY_DISABLED=1     Force-disable entirely.
//	YAHOO_FUNDAMENTAL_HISTORY_DISABLED=1 Skip Yahoo (US/HK history).
//	YAHOO_FUNDAMENTAL_HISTORY_BASE_URL=...  Override Yahoo base URL.
//	AKSHARE_FUNDAMENTAL_HISTORY_URL=...     Akshare-MCP base URL.
//	                                        Defaults to AKSHARE_FUNDAMENTAL_URL
//	                                        when unset so a single deployment
//	                                        knob covers both surfaces.
//	FUNDAMENTAL_HISTORY_CACHE_TTL=...  Go duration; default 24h.
func buildAdvisorFundamentalHistoryFetcherFromEnv() fundamental.HistoricalFetcher {
	if envBool("FUNDAMENTAL_HISTORY_DISABLED") {
		return nil
	}
	reg := fundamental.NewHistoricalRegistry()
	wired := 0
	if !envBool("YAHOO_FUNDAMENTAL_HISTORY_DISABLED") {
		reg.Register(&fundamental.YahooHistoryProvider{
			BaseURL: strings.TrimSpace(os.Getenv("YAHOO_FUNDAMENTAL_HISTORY_BASE_URL")),
		})
		wired++
	}
	akURL := strings.TrimSpace(os.Getenv("AKSHARE_FUNDAMENTAL_HISTORY_URL"))
	if akURL == "" {
		akURL = strings.TrimSpace(os.Getenv("AKSHARE_FUNDAMENTAL_URL"))
	}
	if akURL != "" {
		reg.Register(&fundamental.AkshareHistoryProvider{BaseURL: akURL})
		wired++
	}
	if wired == 0 {
		return nil
	}
	ttl := 24 * time.Hour
	if raw := strings.TrimSpace(os.Getenv("FUNDAMENTAL_HISTORY_CACHE_TTL")); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			ttl = parsed
		}
	}
	return fundamental.NewHistoricalCache(reg, ttl)
}

// historyDurationDefault is exported only so tests can lift the
// hardcoded default without depending on time.
const historyDurationDefault = 24 * time.Hour

var (
	_ = strconv.Atoi // keep strconv import live for future env parsing
	_ = historyDurationDefault
)

// newAdvisorTechnicalLoader builds the closure the advisor service
// uses to attach a price-action / momentum / volatility snapshot to
// every master prompt (and, in publisher mode, to every
// daily_picks.result_json blob).
//
// Construction shape:
//  1. one OHLC fetcher per process (cache-wrapped via
//     buildOHLCFetcherFromEnv), so 50 symbols × 4 presets in one
//     cron tick fan out into ~50 unique Yahoo chart requests, not
//     200 (the cache TTL bucket subsumes the cron pass);
//  2. lookback fixed at 260 daily bars (~1y) so SMA200 has full
//     seeding margin and the 52-week-high return is meaningful;
//  3. per-call timeout fixed at 6s — Yahoo p99 is well under 2s
//     and the advisor's per-symbol timeout in the daily-picks
//     loop is already 90s, so a stuck Yahoo call must not chew
//     through that envelope before fundamentals get a chance.
//
// Returns nil when no OHLC fetcher is wired (local-dev /
// air-gapped tests). The advisor service then degrades to the
// no-technical-block path, which the master prompt rule 9
// handles gracefully ("if the technical snapshot is absent, do
// not invent values").
func newAdvisorTechnicalLoader() advisor.TechnicalLoader {
	fetcher := buildOHLCFetcherFromEnv()
	if fetcher == nil {
		return nil
	}
	return func(ctx context.Context, symbol, market, _ string) (*agent.MasterTechnicalBlock, error) {
		// 6s ceiling — see construction-shape comment above.
		callCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		bars, err := fetcher.Fetch(callCtx, ohlc.FetchRequest{
			Symbol:    symbol,
			Market:    market,
			Interval:  ohlc.IntervalDay,
			LookbackN: 260,
		})
		if err != nil || len(bars) == 0 {
			// Soft failure — no error to the caller. The
			// advisor service's runPanels already treats
			// "loader returned nil" as "no block to inject".
			if err != nil && !errors.Is(err, ohlc.ErrNoData) && !errors.Is(err, ohlc.ErrNoProvider) {
				slog.Debug("advisor: ohlc fetch failed (degrading to no-technical-block)",
					"symbol", symbol, "market", market, "err", err)
			}
			return nil, nil
		}
		snap := indicator.Compute(bars)
		return technicalSnapshotToBlock(snap, bars), nil
	}
}

// technicalSnapshotToBlock projects indicator.Snapshot onto the
// agent.MasterTechnicalBlock wire shape the master prompt and the
// frontend consume. Three responsibilities:
//
//  1. Field-by-field copy (rsi14, macd, kdj, S/R, etc.).
//  2. Multi-window returns derived from the bar slice itself
//     (the snapshot only carries the latest values; computing
//     5d / 20d / 52w returns from the OHLC tail is one slice
//     lookup each).
//  3. RSI zone string normalisation: indicator.Snapshot uses
//     "overbought"/"oversold"/""; the prompt section uses
//     "overbought"/"oversold"/"neutral" so the masters never
//     see an empty-string zone label that could be misread.
func technicalSnapshotToBlock(s indicator.Snapshot, bars []ohlc.Bar) *agent.MasterTechnicalBlock {
	if s.LastClose == 0 || len(bars) == 0 {
		return nil
	}
	t := &agent.MasterTechnicalBlock{
		BarsUsed:       s.BarsUsed,
		LastClose:      s.LastClose,
		SMA20:          s.SMA20,
		SMA50:          s.SMA50,
		SMA200:         s.SMA200,
		RSI14:          s.RSI14,
		MACDLine:       s.MACDLine,
		MACDSignal:     s.MACDSig,
		MACDHist:       s.MACDHist,
		MACDCross:      s.MACDCross,
		ATR14PctOfPx:   s.ATR14PctOfPx,
		KDJK:           s.KDJK,
		KDJD:           s.KDJD,
		KDJJ:           s.KDJJ,
		Volume:         s.LastVolume,
		RelativeVolume: s.RelativeVolume,
		Support:        s.SupportLevel,
		Resistance:     s.ResistanceLevel,
		SRWindow:       s.SRWindow,
		BreakoutState:  s.BreakoutState,
		Tags:           s.Tags,
	}
	// AsOf — last bar's UTC time, RFC3339. Bars are oldest-first.
	t.AsOf = bars[len(bars)-1].Time.UTC().Format(time.RFC3339)
	// MA alignment classification — the indicator package doesn't
	// surface this directly (it computes the tags) so we recompute
	// here for the wire shape's structured field. The prompt
	// renderer uses both: structured value for "ma_alignment=..."
	// line, and the prebuilt tag string for the bullet list.
	switch {
	case s.SMA20 > 0 && s.SMA50 > 0 && s.SMA200 > 0:
		switch {
		case s.SMA20 > s.SMA50 && s.SMA50 > s.SMA200:
			t.MAAlignment = "bullish"
		case s.SMA20 < s.SMA50 && s.SMA50 < s.SMA200:
			t.MAAlignment = "bearish"
		default:
			t.MAAlignment = "mixed"
		}
	}
	// RSI zone: prefer the snapshot's tag; default to "neutral"
	// when it's empty so the structured field always has a
	// readable value (the LLM treats "" as "missing", which is
	// not what we want for in-band 30–70 readings).
	switch s.RSI14Tag {
	case "overbought":
		t.RSI14Zone = "overbought"
	case "oversold":
		t.RSI14Zone = "oversold"
	default:
		if s.RSI14 > 0 {
			t.RSI14Zone = "neutral"
		}
	}
	// Multi-window returns from the bar tail. We compute these
	// here (not in indicator.Snapshot) because Snapshot stays
	// single-window; multi-window returns are a publisher-side
	// presentation concern.
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	last := len(closes) - 1
	if last >= 1 && closes[last-1] != 0 {
		t.PctChange1D = closes[last]/closes[last-1] - 1
	}
	if last >= 5 && closes[last-5] != 0 {
		t.PctChange5D = closes[last]/closes[last-5] - 1
	}
	if last >= 20 && closes[last-20] != 0 {
		t.PctChange20D = closes[last]/closes[last-20] - 1
	}
	// 52-week high return: scan the last 252 bars (≈1y of trading
	// days), expressed as (close / 52w_high - 1). Negative when
	// price is below the 52w high; ~0 at a fresh high. Useful for
	// momentum personas (O'Neil's "buy near 52w high"); same
	// number Stock Rover / Finviz expose under "52W % off high".
	hi := closes[last]
	start := last - 252
	if start < 0 {
		start = 0
	}
	for i := start; i <= last; i++ {
		if bars[i].High > hi {
			hi = bars[i].High
		}
	}
	if hi > 0 {
		t.PctChange52WHi = closes[last]/hi - 1
	}
	return t
}

// buildCNMarketStructureProvider assembles the A-share market
// structure chain. Returns (registry, provider) where the registry
// is the underlying chain (used by the admin probe handler for
// health stats) and the provider is the cache-wrapped surface
// agents and handlers actually call.
//
// Env knobs:
//
//	CN_MARKETSTRUCTURE_DISABLED=1   Force-disable entirely.
//	AKSHARE_CNSTRUCT_URL=...        Akshare-MCP base URL. Defaults
//	                                to AKSHARE_OHLC_URL when unset
//	                                (most deployments reuse one MCP).
//	CN_INTRADAY_TTL=...             Default 60s.
//	CN_LONGHUBANG_TTL=...           Default 10m.
//	CN_MARKET_REGIME_TTL=...        Default 60s.
//	CN_SECTOR_STRENGTH_TTL=...      Default 5m.
//
// Returns (nil, nil) when no provider is wired. Both return values
// are non-nil when at least one provider was registered.
func buildCNMarketStructureProvider() (*cnmarketstructure.Registry, cnmarketstructure.Provider) {
	if envBool("CN_MARKETSTRUCTURE_DISABLED") {
		return nil, nil
	}
	reg := cnmarketstructure.NewRegistry()
	wired := 0
	akURL := strings.TrimSpace(os.Getenv("AKSHARE_CNSTRUCT_URL"))
	if akURL == "" {
		akURL = strings.TrimSpace(os.Getenv("AKSHARE_OHLC_URL"))
	}
	if akURL != "" {
		reg.Register(&cnmarketstructure.AkshareProvider{BaseURL: akURL})
		wired++
	}
	if wired == 0 {
		return nil, nil
	}
	opts := cnmarketstructure.CacheOptions{
		IntradayTTL:       parseDurationEnv("CN_INTRADAY_TTL", 60*time.Second),
		DragonTigerTTL:    parseDurationEnv("CN_LONGHUBANG_TTL", 10*time.Minute),
		MarketRegimeTTL:   parseDurationEnv("CN_MARKET_REGIME_TTL", 60*time.Second),
		SectorStrengthTTL: parseDurationEnv("CN_SECTOR_STRENGTH_TTL", 5*time.Minute),
	}
	return reg, cnmarketstructure.NewCache(reg, opts)
}
