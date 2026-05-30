package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

// ShadowAgents implements api.ABShadowAgentService for the Card D
// "compare A vs B shadow agents" surface. It re-uses the analyzer
// data the existing AB pipeline already writes — no new tables.
//
// Auth contract matches the rest of the AB endpoints: callers must
// be members of BOTH the control fund and the treatment fund of the
// test. Test not found → ErrNotFound; user lacks access to either
// fund → ErrForbidden.
//
// The returned envelope always carries 2 variants (A,B). Agents
// missing from the snapshot's team get default name/role placeholders;
// agents missing entirely from the agents table fall back to their
// raw UUID. We deliberately don't fail the whole request when one
// agent row can't be hydrated — the comparison view is still useful
// without it.
func (s *abTestServiceAdapter) ShadowAgents(ctx context.Context, userID, testID string) (api.ABTestShadowAgentResponse, error) {
	if _, err := s.authorizeABTest(ctx, userID, testID); err != nil {
		return api.ABTestShadowAgentResponse{}, err
	}
	resp := api.ABTestShadowAgentResponse{TestID: testID}

	variants, err := s.loadABShadowVariants(ctx, testID)
	if err != nil {
		return api.ABTestShadowAgentResponse{}, err
	}
	// Build [A, B] in deterministic order so the UI can
	// rely on Variants[0] = A and Variants[1] = B.
	for _, key := range []string{"A", "B"} {
		v, ok := variants[key]
		if !ok {
			resp.Variants = append(resp.Variants, api.ABTestShadowAgentVariant{
				VariantKey: key,
				Agents:     []api.ABTestShadowAgent{},
			})
			continue
		}
		variantOut := api.ABTestShadowAgentVariant{
			VariantKey:     v.Key,
			VariantName:    v.Name,
			StrategyConfig: v.StrategyConfig,
			Agents:         []api.ABTestShadowAgent{},
		}

		events, err := s.loadABShadowAgentEvents(ctx, testID, v.ID)
		if err != nil {
			return api.ABTestShadowAgentResponse{}, err
		}
		memoryByAgent, err := s.loadABShadowAgentMemories(ctx, testID, v.ID)
		if err != nil {
			return api.ABTestShadowAgentResponse{}, err
		}
		for _, agentID := range sortedAgentIDs(events) {
			agentEvents := events[agentID]
			agentOut := api.ABTestShadowAgent{
				AgentID:    agentID,
				EventCount: len(agentEvents),
				Memories:   memoryByAgent[agentID],
			}
			s.populateABAgentMetadata(ctx, agentID, &agentOut, v.TeamSnapshot)
			s.foldABAgentEvents(ctx, agentID, agentEvents, &agentOut)
			variantOut.Agents = append(variantOut.Agents, agentOut)
		}
		resp.Variants = append(resp.Variants, variantOut)
	}
	return resp, nil
}

// abShadowAgentEvent is one row from ab_test_agent_learning_events
// kept as raw maps so the diff stage can compare them against
// the agent's CURRENT evolution_config without re-decoding.
type abShadowAgentEvent struct {
	TradingDate            time.Time
	Summary                string
	Lessons                []string
	Adjustments            []string
	SpecializationLearning map[string]any
	ProposedEvolution      map[string]any
}

func (s *abTestServiceAdapter) loadABShadowAgentEvents(ctx context.Context, testID, variantID string) (map[string][]abShadowAgentEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT agent_id::text, trading_date, COALESCE(summary, ''),
		       COALESCE(lessons, '[]'::jsonb),
		       COALESCE(adjustments, '[]'::jsonb),
		       COALESCE(specialization_learning, '{}'::jsonb),
		       COALESCE(proposed_evolution_config, '{}'::jsonb)
		FROM ab_test_agent_learning_events
		WHERE test_id = $1 AND variant_id = $2
		ORDER BY agent_id, trading_date DESC`, testID, variantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]abShadowAgentEvent{}
	for rows.Next() {
		var (
			agentID                                                          string
			tradingDate                                                      time.Time
			summary                                                          string
			lessonsJSON, adjustmentsJSON, specializationJSON, proposedJSON   json.RawMessage
		)
		if err := rows.Scan(&agentID, &tradingDate, &summary, &lessonsJSON, &adjustmentsJSON, &specializationJSON, &proposedJSON); err != nil {
			return nil, err
		}
		out[agentID] = append(out[agentID], abShadowAgentEvent{
			TradingDate:            tradingDate,
			Summary:                strings.TrimSpace(summary),
			Lessons:                stringSliceFromJSON(lessonsJSON),
			Adjustments:            stringSliceFromJSON(adjustmentsJSON),
			SpecializationLearning: mapFromJSON(specializationJSON),
			ProposedEvolution:      mapFromJSON(proposedJSON),
		})
	}
	return out, rows.Err()
}

func (s *abTestServiceAdapter) loadABShadowAgentMemories(ctx context.Context, testID, variantID string) (map[string][]api.ABTestShadowMemory, error) {
	// We cap to 20 memories per agent so the response stays
	// bounded for long-running tests with chatty memory writers.
	// trading_date NULL rows sort last so dated entries surface
	// first in the timeline.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
		  COALESCE(agent_id::text, ''),
		  memory_key,
		  COALESCE(layer, 'shadow'),
		  trading_date,
		  COALESCE(content, '{}'::jsonb)
		FROM ab_test_variant_memory
		WHERE test_id = $1 AND variant_id = $2
		ORDER BY agent_id, trading_date DESC NULLS LAST, updated_at DESC`,
		testID, variantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]api.ABTestShadowMemory{}
	for rows.Next() {
		var (
			agentID                  string
			memoryKey, layer         string
			tradingDate              sql.NullTime
			contentJSON              json.RawMessage
		)
		if err := rows.Scan(&agentID, &memoryKey, &layer, &tradingDate, &contentJSON); err != nil {
			return nil, err
		}
		if agentID == "" {
			continue
		}
		if len(out[agentID]) >= 20 {
			continue
		}
		entry := api.ABTestShadowMemory{
			MemoryKey: memoryKey,
			Layer:     layer,
			Content:   mapFromJSON(contentJSON),
		}
		if tradingDate.Valid {
			entry.TradingDate = tradingDate.Time.Format("2006-01-02")
		}
		out[agentID] = append(out[agentID], entry)
	}
	return out, rows.Err()
}

// sortedAgentIDs returns the keys of m in stable order so the
// response is deterministic for clients diffing across requests.
func sortedAgentIDs(m map[string][]abShadowAgentEvent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// populateABAgentMetadata fills name/role from the live agents
// table when available, falling back to the variant's team
// snapshot, and finally to a "(unknown agent)" placeholder. We
// never bubble repository errors here; failure to look up a
// single agent shouldn't tank the whole comparison.
func (s *abTestServiceAdapter) populateABAgentMetadata(ctx context.Context, agentID string, out *api.ABTestShadowAgent, snapshot abTeamSnapshot) {
	out.AgentName = agentID
	for _, member := range snapshot.Members {
		if member.AgentID == agentID {
			if name := strings.TrimSpace(member.AgentName); name != "" {
				out.AgentName = name
			}
			out.Role = member.Role
			break
		}
	}
	agents := repository.NewAgentRepo(s.db)
	agent, err := agents.GetByID(ctx, agentID)
	if err == nil && agent != nil {
		if strings.TrimSpace(agent.Name) != "" {
			out.AgentName = agent.Name
		}
		if strings.TrimSpace(agent.Role) != "" {
			out.Role = agent.Role
		}
	}
}

// foldABAgentEvents aggregates the per-day events into the
// terminal wire shape: deduped lesson list, deduped adjustments,
// summaries (most recent 5), specialization learning entries
// (most recent 5), and the daily timeline view.
//
// It also computes the evolution-config diff vs. the agent's
// CURRENT live evolution_config. We deliberately use the
// most-recent event's proposed_evolution_config because the
// shadow run rewrites it day by day — the latest one is the
// shadow's "final" recommendation.
func (s *abTestServiceAdapter) foldABAgentEvents(ctx context.Context, agentID string, events []abShadowAgentEvent, out *api.ABTestShadowAgent) {
	dedup := func(in []string) []string { return uniqueNonEmpty(in) }
	timeline := make([]api.ABTestShadowAgentDay, 0, len(events))
	allLessons := []string{}
	allAdjustments := []string{}
	summaries := []string{}
	specialization := []map[string]any{}
	var proposed map[string]any

	for i, ev := range events {
		dayLessons := dedup(ev.Lessons)
		dayAdjustments := dedup(ev.Adjustments)
		timeline = append(timeline, api.ABTestShadowAgentDay{
			TradingDate: ev.TradingDate.Format("2006-01-02"),
			Summary:     ev.Summary,
			Lessons:     dayLessons,
			Adjustments: dayAdjustments,
		})
		allLessons = append(allLessons, dayLessons...)
		allAdjustments = append(allAdjustments, dayAdjustments...)
		if ev.Summary != "" {
			summaries = append(summaries, ev.Summary)
		}
		if len(ev.SpecializationLearning) > 0 {
			specialization = append(specialization, ev.SpecializationLearning)
		}
		// Most recent event drives the proposed config diff;
		// loadABShadowAgentEvents orders by trading_date DESC
		// so events[0] is the latest.
		if i == 0 && len(ev.ProposedEvolution) > 0 {
			proposed = ev.ProposedEvolution
		}
		if ev.TradingDate.IsZero() {
			continue
		}
		if out.LatestTradingDate == "" || ev.TradingDate.Format("2006-01-02") > out.LatestTradingDate {
			out.LatestTradingDate = ev.TradingDate.Format("2006-01-02")
		}
	}

	out.Lessons = limitStrings(dedup(allLessons), 12)
	out.Adjustments = limitStrings(dedup(allAdjustments), 12)
	out.Summaries = limitStrings(summaries, 5)
	if len(specialization) > 0 {
		out.SpecializationLearning = specialization[:min(len(specialization), 5)]
	}
	out.Timeline = timeline

	if proposed != nil {
		out.ProposedEvolutionDiff = s.computeABEvolutionDiff(ctx, agentID, proposed)
	}
}

// computeABEvolutionDiff returns the additive view of "what
// would change if we promoted this shadow run", relative to
// the agent's CURRENT live evolution_config. nil when there's
// no diff worth surfacing or when the live config isn't
// loadable (the caller treats nil as "no diff" in the UI).
func (s *abTestServiceAdapter) computeABEvolutionDiff(ctx context.Context, agentID string, proposed map[string]any) *api.ABEvolutionConfigDiff {
	if len(proposed) == 0 {
		return nil
	}
	agents := repository.NewAgentRepo(s.db)
	agent, err := agents.GetByID(ctx, agentID)
	if err != nil {
		// Agent missing — treat the entire proposed payload
		// as additive so the UI still renders something.
		if errors.Is(err, repository.ErrNotFound) {
			return &api.ABEvolutionConfigDiff{Added: proposed}
		}
		return nil
	}
	current := mapFromJSON(agent.EvolutionConfig)
	diff := &api.ABEvolutionConfigDiff{
		Added:   map[string]any{},
		Changed: map[string][2]any{},
	}
	for k, v := range proposed {
		cur, ok := current[k]
		if !ok {
			diff.Added[k] = v
			continue
		}
		if !jsonEqual(cur, v) {
			diff.Changed[k] = [2]any{cur, v}
		}
	}
	for k := range current {
		if _, ok := proposed[k]; !ok {
			diff.Removed = append(diff.Removed, k)
		}
	}
	if len(diff.Added) == 0 && len(diff.Changed) == 0 && len(diff.Removed) == 0 {
		return nil
	}
	if len(diff.Added) == 0 {
		diff.Added = nil
	}
	if len(diff.Changed) == 0 {
		diff.Changed = nil
	}
	return diff
}

// jsonEqual compares two arbitrary JSON-decoded values for
// structural equality. We round-trip through json.Marshal so
// floats / nested slices / maps compare correctly without
// having to write a recursive walker.
func jsonEqual(a, b any) bool {
	aBytes, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bBytes, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(aBytes) == string(bBytes)
}

// ---------------------------------------------------------------------------
// Operational attribution
// ---------------------------------------------------------------------------

// OperationalAttribution implements api.ABOperationalAttributionService
// for the Card D "per-symbol PnL gap A vs B" table. The aggregation
// is one SQL query — we pivot in Go after.
//
// The query intentionally reads from ab_test_variant_trades only,
// which covers both the deterministic "[auto-shadow]" rows and any
// future real shadow runs. Real fund trades are NOT mixed in; the
// AB report should only show what the variants generated.
func (s *abTestServiceAdapter) OperationalAttribution(ctx context.Context, userID, testID string) (api.ABTestOperationalAttribution, error) {
	if _, err := s.authorizeABTest(ctx, userID, testID); err != nil {
		return api.ABTestOperationalAttribution{}, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT
		  v.variant_key,
		  t.symbol,
		  COUNT(*),
		  COALESCE(SUM(ABS(t.notional)), 0),
		  COALESCE(SUM(t.realized_pnl), 0),
		  COALESCE(SUM(CASE WHEN t.realized_pnl > 0 THEN 1 ELSE 0 END), 0)
		FROM ab_test_variants v
		JOIN ab_test_variant_trades t ON t.variant_id = v.id
		WHERE v.test_id = $1
		GROUP BY v.variant_key, t.symbol`, testID)
	if err != nil {
		return api.ABTestOperationalAttribution{}, err
	}
	defer rows.Close()

	type bucket struct {
		Symbol      string
		TradeCount  int
		Turnover    float64
		RealizedPnL float64
		WinTrades   int
	}
	bySymbol := map[string]map[string]bucket{} // symbol -> variantKey -> bucket
	totals := map[string]bucket{
		"A": {},
		"B": {},
	}

	for rows.Next() {
		var (
			variantKey, symbol            string
			tradeCount, winTrades         int
			turnover, realizedPnL         float64
		)
		if err := rows.Scan(&variantKey, &symbol, &tradeCount, &turnover, &realizedPnL, &winTrades); err != nil {
			return api.ABTestOperationalAttribution{}, err
		}
		variantKey = strings.ToUpper(variantKey)
		if variantKey != "A" && variantKey != "B" {
			continue
		}
		entry := bucket{
			Symbol:      symbol,
			TradeCount:  tradeCount,
			Turnover:    turnover,
			RealizedPnL: realizedPnL,
			WinTrades:   winTrades,
		}
		if bySymbol[symbol] == nil {
			bySymbol[symbol] = map[string]bucket{}
		}
		bySymbol[symbol][variantKey] = entry
		t := totals[variantKey]
		t.TradeCount += tradeCount
		t.Turnover += turnover
		t.RealizedPnL += realizedPnL
		t.WinTrades += winTrades
		totals[variantKey] = t
	}
	if err := rows.Err(); err != nil {
		return api.ABTestOperationalAttribution{}, err
	}

	resp := api.ABTestOperationalAttribution{
		TestID:   testID,
		TotalA:   totalsToDTO(totals["A"]),
		TotalB:   totalsToDTO(totals["B"]),
		BySymbol: []api.ABAttributionSymbolRow{},
	}

	for symbol, sides := range bySymbol {
		a := sides["A"]
		b := sides["B"]
		row := api.ABAttributionSymbolRow{
			Symbol:       symbol,
			TradeCountA:  a.TradeCount,
			TradeCountB:  b.TradeCount,
			RealizedPnLA: a.RealizedPnL,
			RealizedPnLB: b.RealizedPnL,
			TurnoverA:    a.Turnover,
			TurnoverB:    b.Turnover,
			PnLGap:       b.RealizedPnL - a.RealizedPnL,
		}
		denom := maxFloat(a.Turnover, b.Turnover)
		if denom > 0 {
			row.GapPctOfNotional = (row.PnLGap / denom) * 100
		}
		switch {
		case row.PnLGap > 0.0001:
			row.Winner = "B"
		case row.PnLGap < -0.0001:
			row.Winner = "A"
		default:
			row.Winner = "tie"
		}
		resp.BySymbol = append(resp.BySymbol, row)
	}
	// Largest absolute gap first; ties sorted by symbol for
	// deterministic ordering across requests.
	sort.SliceStable(resp.BySymbol, func(i, j int) bool {
		ai := absFloat(resp.BySymbol[i].PnLGap)
		aj := absFloat(resp.BySymbol[j].PnLGap)
		if ai != aj {
			return ai > aj
		}
		return resp.BySymbol[i].Symbol < resp.BySymbol[j].Symbol
	})
	if len(resp.BySymbol) > 50 {
		resp.BySymbol = resp.BySymbol[:50]
	}
	return resp, nil
}

func totalsToDTO(b struct {
	Symbol      string
	TradeCount  int
	Turnover    float64
	RealizedPnL float64
	WinTrades   int
}) api.ABAttributionTotals {
	out := api.ABAttributionTotals{
		TradeCount:  b.TradeCount,
		Turnover:    b.Turnover,
		RealizedPnL: b.RealizedPnL,
	}
	if b.TradeCount > 0 {
		out.AvgPnL = b.RealizedPnL / float64(b.TradeCount)
		out.WinTradeRate = float64(b.WinTrades) / float64(b.TradeCount)
	}
	return out
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// ensure the unused-import linter doesn't drop fmt accidentally
// (fmt is used elsewhere in the package, but having a single
// guard keeps the file robust against future edits).
var _ = fmt.Sprintf
