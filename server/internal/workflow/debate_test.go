package workflow

import (
	"strings"
	"testing"
)

// helper: build a Roundtable with one round of opinions.
func rtWithOpinions(rounds ...[]ResearcherOpinion) *Roundtable {
	rt := &Roundtable{ID: "rt-test", FundID: "fund-test"}
	for i, ops := range rounds {
		rt.Rounds = append(rt.Rounds, RoundtableRound{
			RoundNumber: i + 1,
			Opinions:    ops,
		})
	}
	return rt
}

func op(agent, sym, dir string, conf int) ResearcherOpinion {
	return ResearcherOpinion{
		AgentID:    agent,
		AgentName:  strings.ToTitle(agent),
		Focus:      "stock",
		Symbol:     sym,
		Direction:  dir,
		Confidence: conf,
		Reasoning:  "because",
	}
}

func TestBuildDebateGraph_NilAndEmpty(t *testing.T) {
	if BuildDebateGraph(nil) != nil {
		t.Fatal("expected nil for nil input")
	}
	g := BuildDebateGraph(&Roundtable{})
	if g == nil || len(g.Claims) != 0 || len(g.Edges) != 0 {
		t.Fatalf("expected empty graph, got %+v", g)
	}
}

func TestBuildDebateGraph_WithinRoundEdges(t *testing.T) {
	rt := rtWithOpinions([]ResearcherOpinion{
		op("a", "AAPL", "bullish", 80),
		op("b", "AAPL", "bullish", 60),
		op("c", "AAPL", "bearish", 90),
		op("d", "AAPL", "neutral", 50), // should not produce any edges
	})

	g := BuildDebateGraph(rt)
	if len(g.Claims) != 4 {
		t.Fatalf("expected 4 claims, got %d", len(g.Claims))
	}

	var supports, rebuts int
	for _, e := range g.Edges {
		if e.Kind == EdgeSupports {
			supports++
		} else {
			rebuts++
		}
		// neutral claim never appears.
		if strings.Contains(e.From, ":d:") || strings.Contains(e.To, ":d:") {
			t.Fatalf("neutral claim leaked into edges: %+v", e)
		}
	}
	// supports: a<->b => 2; rebuts: a<->c, b<->c => 4
	if supports != 2 {
		t.Errorf("expected 2 supports edges, got %d", supports)
	}
	if rebuts != 4 {
		t.Errorf("expected 4 rebut edges, got %d", rebuts)
	}
}

func TestBuildDebateGraph_CrossRoundSelfRebuttal(t *testing.T) {
	rt := rtWithOpinions(
		[]ResearcherOpinion{op("a", "AAPL", "bullish", 70)},
		[]ResearcherOpinion{op("a", "AAPL", "bearish", 85)},
	)
	g := BuildDebateGraph(rt)
	if len(g.Claims) != 2 {
		t.Fatalf("expected 2 claims, got %d", len(g.Claims))
	}
	// One self-rebuttal edge from r2 → r1.
	found := false
	for _, e := range g.Edges {
		if e.Kind == EdgeRebuts &&
			strings.HasPrefix(e.From, "r2:") &&
			strings.HasPrefix(e.To, "r1:") {
			found = true
			if e.Strength <= 0 {
				t.Errorf("expected positive strength, got %v", e.Strength)
			}
		}
	}
	if !found {
		t.Errorf("expected self-rebuttal edge r2→r1, got edges=%+v", g.Edges)
	}
}

func TestResolveVerdicts_ClearMajorityWins(t *testing.T) {
	rt := rtWithOpinions([]ResearcherOpinion{
		op("a", "AAPL", "bullish", 80),
		op("b", "AAPL", "bullish", 70),
		op("c", "AAPL", "bearish", 50),
	})
	v := BuildDebateGraph(rt).ResolveVerdicts()
	if len(v) != 1 {
		t.Fatalf("expected 1 verdict, got %d", len(v))
	}
	if v[0].Direction != "bullish" {
		t.Errorf("expected bullish verdict, got %q", v[0].Direction)
	}
	if v[0].Action != "buy" {
		t.Errorf("expected action=buy (mean conf=75), got %q", v[0].Action)
	}
}

func TestResolveVerdicts_HighConfRebutterFlipsThinMajority(t *testing.T) {
	// Three weak bulls (35% each = 1.05 net support) vs one
	// high-conviction bear (95% = 0.95 attack on each bull).
	// Bull supports = 1.05; bull rebuttals = 0.95 * 3 = 2.85; net = -1.80
	// Bear supports = 0.95; bear rebuttals = 0.35 * 3 = 1.05; net = -0.10
	// Bear should win on net even though bulls have the head count.
	rt := rtWithOpinions([]ResearcherOpinion{
		op("a", "AAPL", "bullish", 35),
		op("b", "AAPL", "bullish", 35),
		op("c", "AAPL", "bullish", 35),
		op("z", "AAPL", "bearish", 95),
	})
	v := BuildDebateGraph(rt).ResolveVerdicts()
	if len(v) != 1 || v[0].Direction != "bearish" {
		t.Fatalf("expected bearish to win on net strength, got %+v", v)
	}
}

func TestResolveVerdicts_ContestedFlag(t *testing.T) {
	// Near tie should set Contested.
	rt := rtWithOpinions([]ResearcherOpinion{
		op("a", "AAPL", "bullish", 60),
		op("b", "AAPL", "bearish", 60),
	})
	v := BuildDebateGraph(rt).ResolveVerdicts()
	if len(v) != 1 {
		t.Fatalf("expected 1 verdict, got %d", len(v))
	}
	if !v[0].Contested {
		t.Errorf("expected Contested=true for symmetric debate")
	}
}

func TestToConsensus_PopulatesSupportersDissenters(t *testing.T) {
	rt := rtWithOpinions([]ResearcherOpinion{
		op("a", "AAPL", "bullish", 80),
		op("b", "AAPL", "bullish", 70),
		op("c", "AAPL", "bearish", 50),
	})
	items := BuildDebateGraph(rt).ToConsensus()
	if len(items) != 1 {
		t.Fatalf("expected 1 consensus item, got %d", len(items))
	}
	got := items[0]
	if got.Direction != "bullish" {
		t.Errorf("direction: want bullish, got %q", got.Direction)
	}
	if len(got.Supporters) != 2 || got.Supporters[0] != "a" || got.Supporters[1] != "b" {
		t.Errorf("supporters mismatch: %v", got.Supporters)
	}
	if len(got.Dissenters) != 1 || got.Dissenters[0] != "c" {
		t.Errorf("dissenters mismatch: %v", got.Dissenters)
	}
	if got.Confidence < 70 || got.Confidence > 80 {
		t.Errorf("confidence should be mean of winners (75), got %d", got.Confidence)
	}
	if got.Action != "buy" {
		t.Errorf("expected action=buy, got %q", got.Action)
	}
}

func TestResolveVerdicts_MultipleSymbolsStableOrder(t *testing.T) {
	rt := rtWithOpinions([]ResearcherOpinion{
		op("a", "ZZZ", "bullish", 60),
		op("b", "AAA", "bearish", 70),
		op("c", "MMM", "neutral", 40),
	})
	v := BuildDebateGraph(rt).ResolveVerdicts()
	if len(v) != 3 {
		t.Fatalf("expected 3 verdicts, got %d", len(v))
	}
	// Sorted by symbol asc.
	if v[0].Symbol != "AAA" || v[1].Symbol != "MMM" || v[2].Symbol != "ZZZ" {
		t.Errorf("verdicts not sorted by symbol: %v %v %v",
			v[0].Symbol, v[1].Symbol, v[2].Symbol)
	}
}
