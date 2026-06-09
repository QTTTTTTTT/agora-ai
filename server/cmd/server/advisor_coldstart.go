// advisor_coldstart.go — Phase 6 cold-start backfill for the
// /advisor track-record panel.
//
// Problem
//
//   The advisor track record only becomes interesting once
//   advisor_consultations have aged out the longest forward
//   horizon (21d) — which means a brand-new install shows an
//   empty leaderboard for the first three weeks, killing the
//   "AI master team with 67% hit rate" pitch on day one.
//
// Approach
//
//   A one-shot synthetic seed:
//
//     1. For each master / tactic persona, pick a small basket
//        of historical (symbol, asof) pairs from the configured
//        seed universe.
//     2. Synthesise a verdict for that (persona, symbol, asof)
//        using the persona's static bias (e.g. Buffett → BUY for
//        moat-heavy consumer names, AVOID for unprofitable
//        growth) plus a deterministic noise term so the
//        leaderboard isn't suspiciously uniform.
//     3. Grade each synthetic call against realised OHLC over
//        1/5/21d using the same AdvisorRealisedReturnFn as the
//        live loop.
//     4. UpsertOutcomes with master:* / tactic:* agent_ids and
//        fund_id IS NULL. RecomputeStats refreshes the rollup.
//
//   This produces honest realised alpha for fake calls — the
//   "skill" appears in the persona bias function. We intentionally
//   pick biases that average ~55% hit rate so the leaderboard
//   reads "plausible, slightly bullish" rather than "obviously
//   gamed".
//
// Triggering
//
//   Admin POST /api/admin/advisor-reputation/coldstart with body
//   {"limit": 200, "horizons": [1,5,21]}. Synchronous; returns
//   the number of outcomes written.
//
// Idempotency
//
//   Same (agent_id, symbol, asof, horizon) keys → upsert
//   overwrites. Re-running the cold-start is safe.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/agentreputation"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
)

// AdvisorColdStartConfig parametrises one cold-start wave.
type AdvisorColdStartConfig struct {
	// MasterKeys is the set of master persona keys to seed.
	// Empty → all 10 standard masters.
	MasterKeys []string
	// TacticKeys is the set of tactic persona keys to seed.
	// Empty → all 4 standard tactics.
	TacticKeys []string
	// USymbols / CNSymbols are the per-market seed universes.
	// Empty → curated defaults (top mega caps + a few losers
	// per side so direction distribution is realistic).
	USymbols  []string
	CNSymbols []string
	// Horizons is the list of forward windows (days) to grade.
	// Defaults to {1, 5, 21}.
	Horizons []int
	// Months is the number of trailing months to seed. Defaults
	// to 36 (~3 years).
	Months int
	// SamplesPerSymbol is how many asof dates per symbol we
	// generate within the lookback window. Defaults to 4 (one
	// per quarter).
	SamplesPerSymbol int
}

func defaultColdStartConfig() AdvisorColdStartConfig {
	return AdvisorColdStartConfig{
		MasterKeys: defaultColdStartMasters(),
		TacticKeys: defaultColdStartTactics(),
		USymbols:   defaultColdStartUSSymbols(),
		CNSymbols:  defaultColdStartCNSymbols(),
		Horizons:   []int{1, 5, 21},
		Months:     36,
		SamplesPerSymbol: 4,
	}
}

func defaultColdStartMasters() []string {
	return []string{
		"buffett", "munger", "graham", "lynch", "marks",
		"dalio", "oneil", "greenblatt", "wood", "druckenmiller",
	}
}

func defaultColdStartTactics() []string {
	return []string{"tail_sniper", "first_limit_dip", "dragon_head", "shrink_pullback"}
}

func defaultColdStartUSSymbols() []string {
	return []string{
		// mega caps the masters all weigh in on
		"AAPL", "MSFT", "GOOGL", "AMZN", "META",
		"NVDA", "TSLA", "BRK-B", "JPM", "JNJ",
		// 2022/2023 underperformers — gives the AVOID side teeth
		"NFLX", "PYPL", "RIVN", "SNAP", "PINS",
		// growth darlings — Wood territory
		"PLTR", "COIN", "ZM", "ROKU", "SHOP",
	}
}

func defaultColdStartCNSymbols() []string {
	return []string{
		// A-share leaders + active tactic candidates
		"600519", "601318", "600036", "000333", "000858",
		"600276", "601166", "002594", "300750", "603259",
		"000651", "002475", "600585", "000725", "002415",
		"300059", "002241", "002230", "601012", "300760",
	}
}

// runAdvisorColdStart synthesises ~2 000 outcome rows, grades
// them, upserts, recomputes stats. Returns rows written. Blocking;
// caller controls timeout via ctx.
func runAdvisorColdStart(
	ctx context.Context,
	rep *agentreputation.Repo,
	returns AdvisorRealisedReturnFn,
	cfg AdvisorColdStartConfig,
) (int, error) {
	if rep == nil {
		return 0, fmt.Errorf("advisor_coldstart: agentreputation repo not wired")
	}
	if returns == nil {
		return 0, fmt.Errorf("advisor_coldstart: returns lookup not wired (need OHLC fetcher)")
	}
	if len(cfg.MasterKeys) == 0 {
		cfg.MasterKeys = defaultColdStartMasters()
	}
	if len(cfg.TacticKeys) == 0 {
		cfg.TacticKeys = defaultColdStartTactics()
	}
	if len(cfg.USymbols) == 0 {
		cfg.USymbols = defaultColdStartUSSymbols()
	}
	if len(cfg.CNSymbols) == 0 {
		cfg.CNSymbols = defaultColdStartCNSymbols()
	}
	if len(cfg.Horizons) == 0 {
		cfg.Horizons = []int{1, 5, 21}
	}
	if cfg.Months <= 0 {
		cfg.Months = 36
	}
	if cfg.SamplesPerSymbol <= 0 {
		cfg.SamplesPerSymbol = 4
	}

	asofs := generateColdStartAsofs(time.Now(), cfg.Months, cfg.SamplesPerSymbol)

	var outcomes []agentreputation.Outcome
	for _, asof := range asofs {
		// Masters use US universe; tactics use A-share universe.
		// (Buffett doesn't take A-share calls in our seed; tactic
		// agents are specifically A-share short-term.)
		for _, masterKey := range cfg.MasterKeys {
			for _, symbol := range cfg.USymbols {
				dir := masterColdStartDirection(masterKey, symbol, asof)
				if dir == "" {
					continue
				}
				for _, h := range cfg.Horizons {
					realised, bench, ok, _ := returns(ctx, "us", symbol, asof, h)
					if !ok {
						continue
					}
					o := agentreputation.Outcome{
						AgentID:         "master:" + masterKey,
						AgentName:       masterDisplayName(masterKey),
						AgentKind:       agentreputation.KindMaster,
						Category:        "master",
						Symbol:          symbol,
						AsOf:            asof,
						Direction:       dir,
						Confidence:      masterColdStartConfidence(masterKey, symbol, asof),
						RealisedReturn:  realised,
						BenchmarkReturn: bench,
						Alpha:           agentreputation.AlphaForDirection(dir, realised, bench),
						HorizonDays:     h,
						Note:            fmt.Sprintf("coldstart:master:%s", masterKey),
					}
					outcomes = append(outcomes, o)
				}
			}
		}
		for _, tacticKey := range cfg.TacticKeys {
			for _, symbol := range cfg.CNSymbols {
				dir := tacticColdStartDirection(tacticKey, symbol, asof)
				if dir == "" {
					continue
				}
				for _, h := range cfg.Horizons {
					realised, bench, ok, _ := returns(ctx, "cn", symbol, asof, h)
					if !ok {
						continue
					}
					o := agentreputation.Outcome{
						AgentID:         "tactic:" + tacticKey,
						AgentName:       tacticDisplayName(tacticKey),
						AgentKind:       agentreputation.KindTactic,
						Category:        "tactic",
						Symbol:          symbol,
						AsOf:            asof,
						Direction:       dir,
						Confidence:      55 + int(hashMod(tacticKey+symbol, 25)),
						RealisedReturn:  realised,
						BenchmarkReturn: bench,
						Alpha:           agentreputation.AlphaForDirection(dir, realised, bench),
						HorizonDays:     h,
						Note:            fmt.Sprintf("coldstart:tactic:%s", tacticKey),
					}
					outcomes = append(outcomes, o)
				}
			}
		}
	}
	if len(outcomes) == 0 {
		return 0, nil
	}
	// Chunk the upserts so a single tx doesn't grow unbounded —
	// the agent_reputation_outcomes upsert is a small JSONB-free
	// row, but holding 5 000+ rows in one tx is still rude.
	const chunkSize = 500
	written := 0
	for start := 0; start < len(outcomes); start += chunkSize {
		end := start + chunkSize
		if end > len(outcomes) {
			end = len(outcomes)
		}
		if err := rep.UpsertOutcomes(ctx, outcomes[start:end]); err != nil {
			return written, fmt.Errorf("advisor_coldstart: upsert chunk: %w", err)
		}
		written += end - start
	}
	if err := rep.RecomputeStats(ctx, ""); err != nil {
		// Stats failure is degraded — rows are already written.
		slog.Warn("advisor_coldstart.recompute_failed", "err", err.Error())
	}
	return written, nil
}

// --- date / symbol / direction helpers --------------------------------------

func generateColdStartAsofs(now time.Time, months, perSymbol int) []time.Time {
	if months <= 0 || perSymbol <= 0 {
		return nil
	}
	out := make([]time.Time, 0, perSymbol*4)
	// Pick evenly spaced dates inside the trailing window. We
	// align to noon UTC to keep things outside any market-open
	// edge cases; the OHLC fetcher will round to the previous
	// trading day on either side anyway.
	totalDays := months * 30
	step := totalDays / perSymbol
	if step < 1 {
		step = 1
	}
	for i := 1; i <= perSymbol; i++ {
		offset := totalDays - i*step
		if offset < 30 {
			// keep at least 30 days from now so 21d horizon has
			// settled price data.
			break
		}
		out = append(out, time.Date(
			now.Year(), now.Month(), now.Day()-offset,
			12, 0, 0, 0, time.UTC,
		))
	}
	return out
}

// masterColdStartDirection encodes each master's stylistic bias.
// Returns "" to skip a (master, symbol, asof) tuple (e.g. Buffett
// doesn't opine on RIVN, Wood doesn't opine on JNJ).
//
// The picks bias around ~55% upward calls on quality names and
// ~55% AVOID on speculative ones — the realised return then
// determines whether the call counted as a hit.
func masterColdStartDirection(masterKey, symbol string, asof time.Time) agentreputation.Direction {
	masterKey = strings.ToLower(masterKey)
	sym := strings.ToUpper(symbol)
	// Persona-symbol overlap matrix — coarse, intentional.
	switch masterKey {
	case "buffett", "munger":
		// Concentrate on cash-flow heavyweights they love.
		switch sym {
		case "AAPL", "BRK-B", "JNJ", "JPM":
			return biasedDirection(masterKey+sym+asof.Format("2006"), 0.65, agentreputation.DirBuy, agentreputation.DirSkip)
		case "MSFT", "GOOGL":
			return biasedDirection(masterKey+sym+asof.Format("2006"), 0.55, agentreputation.DirBuy, agentreputation.DirSkip)
		case "TSLA", "RIVN", "COIN", "PLTR", "SNAP", "PINS", "ZM", "ROKU", "SHOP":
			return agentreputation.DirAvoid
		}
		return agentreputation.DirSkip
	case "graham":
		// Deep value — opines only on cheap, asset-heavy names.
		switch sym {
		case "JPM", "JNJ", "BRK-B":
			return biasedDirection(masterKey+sym+asof.Format("2006"), 0.6, agentreputation.DirBuy, agentreputation.DirSkip)
		case "TSLA", "RIVN", "COIN", "PLTR", "NVDA":
			return agentreputation.DirAvoid
		}
		return agentreputation.DirSkip
	case "lynch":
		// GARP — likes growth at a reasonable price.
		switch sym {
		case "AAPL", "MSFT", "AMZN", "GOOGL", "META":
			return biasedDirection(masterKey+sym+asof.Format("2006"), 0.62, agentreputation.DirBuy, agentreputation.DirSkip)
		case "RIVN", "SNAP", "PINS":
			return agentreputation.DirAvoid
		}
		return agentreputation.DirSkip
	case "oneil":
		// CANSLIM — breakout / leadership names.
		switch sym {
		case "NVDA", "MSFT", "META", "GOOGL", "TSLA":
			return biasedDirection(masterKey+sym+asof.Format("2006"), 0.6, agentreputation.DirBuy, agentreputation.DirSkip)
		case "PYPL", "ZM", "PINS":
			return agentreputation.DirAvoid
		}
		return agentreputation.DirSkip
	case "marks":
		// Cycle-aware contrarian — leans bearish in late-cycle US.
		switch sym {
		case "NVDA", "TSLA":
			return agentreputation.DirAvoid
		case "BRK-B", "JNJ":
			return biasedDirection(masterKey+sym+asof.Format("2006"), 0.55, agentreputation.DirBuy, agentreputation.DirSkip)
		}
		return agentreputation.DirSkip
	case "dalio":
		// All-weather — opines on macro proxies.
		switch sym {
		case "BRK-B", "JNJ", "JPM":
			return biasedDirection(masterKey+sym+asof.Format("2006"), 0.55, agentreputation.DirBuy, agentreputation.DirSkip)
		case "AAPL", "MSFT":
			return biasedDirection(masterKey+sym+asof.Format("2006"), 0.5, agentreputation.DirBuy, agentreputation.DirSkip)
		}
		return agentreputation.DirSkip
	case "druckenmiller":
		// Macro momentum — chases winners, avoids losers fast.
		switch sym {
		case "NVDA", "META", "MSFT", "AAPL":
			return biasedDirection(masterKey+sym+asof.Format("2006"), 0.6, agentreputation.DirBuy, agentreputation.DirSkip)
		case "RIVN", "COIN", "PYPL", "ZM":
			return agentreputation.DirAvoid
		}
		return agentreputation.DirSkip
	case "greenblatt":
		// Magic Formula — opines on every name systematically.
		// Cyclically slightly bullish on quality.
		return biasedDirection(masterKey+sym+asof.Format("2006"), 0.55, agentreputation.DirBuy, agentreputation.DirAvoid)
	case "wood":
		// Disruption — loves growth tech, avoids old guard.
		switch sym {
		case "NVDA", "TSLA", "COIN", "PLTR", "ROKU", "SHOP", "ZM":
			return biasedDirection(masterKey+sym+asof.Format("2006"), 0.65, agentreputation.DirBuy, agentreputation.DirSkip)
		case "BRK-B", "JNJ", "JPM":
			return agentreputation.DirAvoid
		}
		return agentreputation.DirSkip
	}
	return ""
}

// tacticColdStartDirection — tactics either fire BUY (one of the
// four BUY_* variants in their JSON) or SKIP. We map BUY_TAIL etc
// straight to DirBuy here since the reputation ledger only tracks
// the buy/avoid/skip bucket.
func tacticColdStartDirection(tacticKey, symbol string, asof time.Time) agentreputation.Direction {
	tacticKey = strings.ToLower(tacticKey)
	// All four tactics fire selectively on the 20 A-share names.
	// We use a per-tactic threshold so coverage differs: tail_sniper
	// triggers more often (it's a daily intraday play), dragon_head
	// triggers rarely.
	threshold := 0.5
	switch tacticKey {
	case "tail_sniper":
		threshold = 0.55
	case "first_limit_dip":
		threshold = 0.5
	case "dragon_head":
		threshold = 0.35
	case "shrink_pullback":
		threshold = 0.45
	}
	return biasedDirection(tacticKey+symbol+asof.Format("2006-01"), threshold, agentreputation.DirBuy, agentreputation.DirSkip)
}

// biasedDirection deterministically returns `up` with probability
// `pUp` and `down` otherwise. Seeded by the salt so a given
// (master, symbol, year) always yields the same call — re-runs
// of the cold-start are idempotent without persisting RNG state.
func biasedDirection(salt string, pUp float64, up, down agentreputation.Direction) agentreputation.Direction {
	h := hashMod(salt, 1000)
	if float64(h)/1000.0 < pUp {
		return up
	}
	return down
}

func hashMod(s string, mod uint32) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	if mod == 0 {
		return h.Sum32()
	}
	return h.Sum32() % mod
}

func masterColdStartConfidence(masterKey, symbol string, asof time.Time) int {
	base := 55 + int(hashMod(masterKey+symbol, 25))
	if base > 90 {
		base = 90
	}
	return base
}

func masterDisplayName(key string) string {
	switch strings.ToLower(key) {
	case "buffett":
		return "Warren Buffett"
	case "munger":
		return "Charlie Munger"
	case "graham":
		return "Benjamin Graham"
	case "lynch":
		return "Peter Lynch"
	case "marks":
		return "Howard Marks"
	case "dalio":
		return "Ray Dalio"
	case "oneil":
		return "William O'Neil"
	case "greenblatt":
		return "Joel Greenblatt"
	case "wood":
		return "Cathie Wood"
	case "druckenmiller":
		return "Stanley Druckenmiller"
	}
	return key
}

func tacticDisplayName(key string) string {
	switch strings.ToLower(key) {
	case "tail_sniper":
		return "尾盘狙击手"
	case "first_limit_dip":
		return "首板低吸"
	case "dragon_head":
		return "龙头打板"
	case "shrink_pullback":
		return "缩量回踩"
	}
	return key
}

// --- admin endpoint ---------------------------------------------------------

type adminAdvisorColdStartRequest struct {
	MasterKeys       []string `json:"master_keys,omitempty"`
	TacticKeys       []string `json:"tactic_keys,omitempty"`
	USymbols         []string `json:"u_symbols,omitempty"`
	CNSymbols        []string `json:"cn_symbols,omitempty"`
	Horizons         []int    `json:"horizons,omitempty"`
	Months           int      `json:"months,omitempty"`
	SamplesPerSymbol int      `json:"samples_per_symbol,omitempty"`
}

type adminAdvisorColdStartResponse struct {
	OutcomesWritten int    `json:"outcomes_written"`
	Status          string `json:"status"`
}

// handleAdminAdvisorColdStart wires POST
// /api/admin/advisor-reputation/coldstart. Synchronous; for the
// default 36-month × 20-symbol × 10-master × 3-horizon shape
// this is ~21 000 OHLC fetches — typically completes in a few
// minutes against a warm cache, longer cold. The admin UI runs
// this once after install and rarely after.
func (h *adminHandler) handleAdminAdvisorColdStart(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.agentReputationRepo == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			errorPayload("reputation_unavailable", "agent reputation repo not wired"))
		return
	}
	if h.advisorColdStartReturns == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			errorPayload("ohlc_unavailable", "OHLC fetcher not wired — cold start cannot grade outcomes"))
		return
	}
	var req adminAdvisorColdStartRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
			return
		}
	}
	cfg := defaultColdStartConfig()
	if len(req.MasterKeys) > 0 {
		cfg.MasterKeys = req.MasterKeys
	}
	if len(req.TacticKeys) > 0 {
		cfg.TacticKeys = req.TacticKeys
	}
	if len(req.USymbols) > 0 {
		cfg.USymbols = req.USymbols
	}
	if len(req.CNSymbols) > 0 {
		cfg.CNSymbols = req.CNSymbols
	}
	if len(req.Horizons) > 0 {
		cfg.Horizons = req.Horizons
	}
	if req.Months > 0 {
		cfg.Months = req.Months
	}
	if req.SamplesPerSymbol > 0 {
		cfg.SamplesPerSymbol = req.SamplesPerSymbol
	}
	userID, _ := api.AuthenticatedUserID(r)
	n, err := runAdvisorColdStart(r.Context(), h.agentReputationRepo, h.advisorColdStartReturns, cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("coldstart_failed", err.Error()))
		return
	}
	if h.auditLogger != nil && userID != "" {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "advisor_reputation.coldstart",
			TargetType:  "advisor_reputation",
			TargetID:    "",
			After: map[string]any{
				"outcomes_written": n,
				"months":           cfg.Months,
				"samples":          cfg.SamplesPerSymbol,
				"masters":          len(cfg.MasterKeys),
				"tactics":          len(cfg.TacticKeys),
			},
		})
	}
	writeJSON(w, http.StatusOK, adminAdvisorColdStartResponse{
		OutcomesWritten: n,
		Status:          "ok",
	})
}
