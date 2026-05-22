package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/workflow"
)

// fundConfigJSON is a small helper that builds a fundMarketProfile-
// compatible JSON config for use in tests. Centralised so test cases
// stay readable and the wire schema only lives in one place.
func fundConfigJSON(market string, symbols ...string) json.RawMessage {
	payload := map[string]interface{}{
		"market":     market,
		"assetClass": "equity",
		"universe": map[string]interface{}{
			"mode":    "manual",
			"symbols": symbols,
		},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

// TestAppendUniverseWatchActionsCoversAllUniverseSymbols verifies the
// observability fix for the OCS / US-storage bug:
//
//   - OCS fund has 2 universe symbols (688205, 688195) but the
//     deterministic fallback only proposes a buy/reduce on the first
//     symbol; the second was silently dropped.
//   - US storage fund has 4 universe symbols (MU/SNDK/WDC/STX) but
//     trades and plans only ever mentioned MU.
//
// The fix: after the base action(s) are emitted, append non-executing
// "watch" actions for every universe / team-coverage symbol that
// wasn't already touched, so the Decision Center reflects EVERY
// stock the team covers.
func TestAppendUniverseWatchActionsCoversAllUniverseSymbols(t *testing.T) {
	cases := []struct {
		name              string
		fundConfig        json.RawMessage
		teamCandidates    []string
		baseActions       []repository.PlanAction
		wantWatchSymbols  []string
		wantUnchangedBase bool
	}{
		{
			name:           "OCS A-share fund with two universe symbols and one buy",
			fundConfig:     fundConfigJSON("a_share", "688205", "688195"),
			teamCandidates: []string{"688205", "688195"},
			baseActions: []repository.PlanAction{
				{Symbol: "688205", Action: "buy"},
			},
			wantWatchSymbols:  []string{"688195"},
			wantUnchangedBase: true,
		},
		{
			name:           "US storage fund with four universe symbols and one reduce",
			fundConfig:     fundConfigJSON("us_equity", "MU", "SNDK", "WDC", "STX"),
			teamCandidates: []string{"MU"},
			baseActions: []repository.PlanAction{
				{Symbol: "MU", Action: "reduce"},
			},
			wantWatchSymbols:  []string{"SNDK", "WDC", "STX"},
			wantUnchangedBase: true,
		},
		{
			name:           "team candidate outside universe is filtered out as noise (theme acronym)",
			fundConfig:     fundConfigJSON("us_equity", "MU"),
			teamCandidates: []string{"MU", "DRAM", "NAND"},
			baseActions: []repository.PlanAction{
				{Symbol: "MU", Action: "hold"},
			},
			wantWatchSymbols:  nil,
			wantUnchangedBase: true,
		},
		{
			name:              "no fund universe: team candidates show through (no whitelist)",
			fundConfig:        json.RawMessage(`{"market":"us_equity"}`),
			teamCandidates:    []string{"NVDA", "AAPL"},
			baseActions:       []repository.PlanAction{{Symbol: "MU", Action: "buy"}},
			wantWatchSymbols:  []string{"NVDA", "AAPL"},
			wantUnchangedBase: true,
		},
		{
			name:              "no universe configured returns base actions unchanged",
			fundConfig:        json.RawMessage(`{"market":"a_share"}`),
			teamCandidates:    nil,
			baseActions:       []repository.PlanAction{{Symbol: "ANY", Action: "watch"}},
			wantWatchSymbols:  nil,
			wantUnchangedBase: true,
		},
		{
			name:              "duplicate / case-variant candidates dedupe (AAPL filtered: not in universe)",
			fundConfig:        fundConfigJSON("us_equity", "MU", "mu", "  MU  ", "NVDA"),
			teamCandidates:    []string{"NVDA", "nvda", "AAPL"},
			baseActions:       []repository.PlanAction{{Symbol: "MU", Action: "buy"}},
			wantWatchSymbols:  []string{"NVDA"},
			wantUnchangedBase: true,
		},
	}

	agent := &runtimePMAgent{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fund := &repository.Fund{ID: "fund-test", Config: tc.fundConfig}
			base := make([]repository.PlanAction, len(tc.baseActions))
			copy(base, tc.baseActions)
			got := agent.appendUniverseWatchActions(
				context.Background(),
				base,
				fund,
				tc.teamCandidates,
				&workflow.RoundtableResult{Consensus: []string{"test consensus"}},
			)

			if tc.wantUnchangedBase {
				if len(got) < len(tc.baseActions) {
					t.Fatalf("base actions truncated: got=%d base=%d", len(got), len(tc.baseActions))
				}
				for i, baseAct := range tc.baseActions {
					if got[i].Symbol != baseAct.Symbol || got[i].Action != baseAct.Action {
						t.Errorf("base action %d mutated: got %+v, want %+v", i, got[i], baseAct)
					}
				}
			}

			extra := got[len(tc.baseActions):]
			gotWatch := make([]string, 0, len(extra))
			for _, act := range extra {
				if act.Action != "watch" {
					t.Errorf("non-watch action appended: %+v", act)
				}
				if act.Quantity.Valid && act.Quantity.Float64 > 0 {
					t.Errorf("watch action %s must not carry an executable qty: got %v", act.Symbol, act.Quantity.Float64)
				}
				gotWatch = append(gotWatch, act.Symbol)
			}

			if len(gotWatch) != len(tc.wantWatchSymbols) {
				t.Fatalf("watch symbol count: got %v, want %v", gotWatch, tc.wantWatchSymbols)
			}
			for i, want := range tc.wantWatchSymbols {
				if gotWatch[i] != want {
					t.Errorf("watch symbol[%d]: got %q, want %q (full=%v)", i, gotWatch[i], want, gotWatch)
				}
			}
		})
	}
}

// TestAppendUniverseWatchActionsCapsAtSixteen guards against a runaway
// plan size if an operator misconfigures a huge universe. The
// LLM-driven path bypasses this helper, so the cap only applies to
// the deterministic fallback safety-net.
func TestAppendUniverseWatchActionsCapsAtSixteen(t *testing.T) {
	syms := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		// generate distinct synthetic tickers SYM00..SYM29
		s := "SYM"
		if i < 10 {
			s += "0"
		}
		s += string(rune('0'+i/10)) + string(rune('0'+i%10))
		syms = append(syms, s)
	}
	cfg := fundConfigJSON("us_equity", syms...)

	agent := &runtimePMAgent{}
	got := agent.appendUniverseWatchActions(
		context.Background(),
		[]repository.PlanAction{{Symbol: "SYM00", Action: "buy"}},
		&repository.Fund{ID: "fund-cap", Config: cfg},
		nil,
		&workflow.RoundtableResult{},
	)

	watchCount := 0
	for _, act := range got {
		if act.Action == "watch" && strings.HasPrefix(act.Symbol, "SYM") {
			watchCount++
		}
	}
	if watchCount != 16 {
		t.Errorf("watch cap: got %d watch actions, want 16", watchCount)
	}
}
