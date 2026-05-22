package workflow

// Debate graph: an explicit, citable representation of a roundtable
// discussion. Where the legacy roundtable code reduces every researcher
// statement to a single "direction vote", the debate graph preserves
// each statement as a Claim node and records the supporting / rebutting
// relationships between claims as edges.
//
// The verdict on each symbol is then derived from "unrebutted argument
// strength" — claims whose rebuttals carry less confidence than the
// supports survive, and the surviving direction with the highest net
// strength wins. This produces a more honest answer than naive vote
// counting when, for example, a single high-confidence researcher with
// a thesis-grade rebuttal flips a 4-vs-2 majority on its head.

import (
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// EdgeKind classifies the relationship between two claims.
type EdgeKind string

const (
	EdgeSupports EdgeKind = "supports"
	EdgeRebuts   EdgeKind = "rebuts"
)

// Claim is a single, citable position taken by one researcher in one round.
type Claim struct {
	ID         string  // stable identifier, e.g. "r1:agent-a:AAPL:bullish"
	Round      int     // 1-based round number where the claim was made
	AgentID    string  // researcher agent id
	AgentName  string  // researcher agent name (display)
	Focus      string  // researcher focus: "stock"|"fundamental"|"macro"|...
	Symbol     string  // subject symbol (may be empty for general claims)
	Direction  string  // "bullish"|"bearish"|"neutral"
	Confidence int     // 0..100, taken from the underlying opinion
	Reasoning  string  // free-form text from the opinion
	Weight     float64 // confidence/100, cached for verdict math
}

// Edge represents a directed relationship between two claims.
//   - Supports: From and To agree (same symbol & direction).
//   - Rebuts:   From contradicts To (same symbol, opposing direction).
//
// The Strength is the supporter/rebutter's confidence in [0,1] and is
// what the verdict resolver weighs against the target.
type Edge struct {
	From     string   // claim ID making the relationship
	To       string   // claim ID being supported / rebutted
	Kind     EdgeKind // supports | rebuts
	Strength float64  // [0,1], typically From.Weight
}

// DebateGraph is the full set of claims and the relationships among them
// derived from one Roundtable.
type DebateGraph struct {
	RoundtableID string
	FundID       string
	Claims       []Claim
	Edges        []Edge
}

// Verdict captures the resolved position on one symbol after the debate.
type Verdict struct {
	Symbol         string
	Direction      string  // winning direction, "" if no claims
	NetStrength    float64 // (supporters - rebutters) for winner, in claim-weight units
	WinnerClaims   []string
	OpposingClaims []string
	Confidence     int    // average confidence of winning claims, 0..100
	Action         string // buy/sell/hold/watch
	Reasoning      string
	// Contested is true when the runner-up's net strength is within
	// 25% of the winner's — verdict still picks a side but downstream
	// callers may want to surface this to the PM.
	Contested bool
}

// ---------------------------------------------------------------------------
// Graph construction
// ---------------------------------------------------------------------------

// BuildDebateGraph derives a DebateGraph from a completed Roundtable.
//
// Edge inference rules (purely structural, no LLM required):
//
//  1. Within a single round, two claims on the same symbol form an edge:
//     - same direction       → Supports
//     - opposing direction   → Rebuts
//     - one side neutral     → no edge (neutral is treated as "no signal")
//
//  2. Across rounds, when the same agent changes direction on the same
//     symbol, the new claim is recorded as Rebutting its own prior claim
//     (an explicit "I have updated my view") with strength = new claim's
//     weight. This is what makes the graph a *debate* rather than a
//     time-series of independent votes.
//
// Edges are deduplicated by (From,To,Kind).
func BuildDebateGraph(rt *Roundtable) *DebateGraph {
	if rt == nil {
		return nil
	}

	g := &DebateGraph{
		RoundtableID: rt.ID,
		FundID:       rt.FundID,
	}

	// 1. Materialise claims with stable IDs.
	for _, round := range rt.Rounds {
		for _, op := range round.Opinions {
			if op.AgentID == "" || op.Symbol == "" || op.Direction == "" {
				continue
			}
			g.Claims = append(g.Claims, Claim{
				ID:         claimID(round.RoundNumber, op),
				Round:      round.RoundNumber,
				AgentID:    op.AgentID,
				AgentName:  op.AgentName,
				Focus:      op.Focus,
				Symbol:     op.Symbol,
				Direction:  op.Direction,
				Confidence: clampConfidence(op.Confidence),
				Reasoning:  op.Reasoning,
				Weight:     float64(clampConfidence(op.Confidence)) / 100.0,
			})
		}
	}

	if len(g.Claims) == 0 {
		return g
	}

	// 2. Index claims for edge inference.
	bySymbolRound := make(map[string][]int) // key: symbol|round → []idx
	byAgentSymbol := make(map[string][]int) // key: agent|symbol  → []idx (chronological)
	for i, c := range g.Claims {
		k1 := c.Symbol + "|" + itoa(c.Round)
		bySymbolRound[k1] = append(bySymbolRound[k1], i)
		k2 := c.AgentID + "|" + c.Symbol
		byAgentSymbol[k2] = append(byAgentSymbol[k2], i)
	}

	// Track unique edges.
	seen := make(map[string]struct{})
	addEdge := func(from, to int, kind EdgeKind) {
		if from == to {
			return
		}
		key := g.Claims[from].ID + "->" + g.Claims[to].ID + ":" + string(kind)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		g.Edges = append(g.Edges, Edge{
			From:     g.Claims[from].ID,
			To:       g.Claims[to].ID,
			Kind:     kind,
			Strength: g.Claims[from].Weight,
		})
	}

	// 3. Within-round support / rebut edges.
	for _, idxs := range bySymbolRound {
		for i := 0; i < len(idxs); i++ {
			for j := i + 1; j < len(idxs); j++ {
				a, b := idxs[i], idxs[j]
				ca, cb := g.Claims[a], g.Claims[b]
				if ca.Direction == "neutral" || cb.Direction == "neutral" {
					continue
				}
				if ca.Direction == cb.Direction {
					addEdge(a, b, EdgeSupports)
					addEdge(b, a, EdgeSupports)
				} else {
					addEdge(a, b, EdgeRebuts)
					addEdge(b, a, EdgeRebuts)
				}
			}
		}
	}

	// 4. Cross-round self-rebuttals (agent changed mind on same symbol).
	for _, idxs := range byAgentSymbol {
		if len(idxs) < 2 {
			continue
		}
		// idxs are appended in claim order, which is also round order
		// because we iterate rt.Rounds in order. Make it explicit:
		sort.SliceStable(idxs, func(a, b int) bool {
			return g.Claims[idxs[a]].Round < g.Claims[idxs[b]].Round
		})
		for k := 1; k < len(idxs); k++ {
			prev, curr := idxs[k-1], idxs[k]
			if g.Claims[prev].Direction == g.Claims[curr].Direction {
				continue
			}
			if g.Claims[prev].Direction == "neutral" || g.Claims[curr].Direction == "neutral" {
				continue
			}
			addEdge(curr, prev, EdgeRebuts)
		}
	}

	return g
}

// ---------------------------------------------------------------------------
// Verdict resolution
// ---------------------------------------------------------------------------

// ResolveVerdicts computes one Verdict per symbol from the graph.
//
// For each symbol, for each direction taken by at least one claim:
//
//	supports  = sum of weights of claims voting that direction
//	rebuttals = sum of edges of kind Rebuts targeting any of those claims
//	            (using the rebuttal edge strength, not the rebutter's
//	            full weight, so that a single rebutter cannot rebut N
//	            different supporters more than once each)
//	net = supports - rebuttals
//
// Winner = direction with the largest net (ties broken by raw supports).
// Contested = runner-up net within 25% of winner's net.
func (g *DebateGraph) ResolveVerdicts() []Verdict {
	if g == nil || len(g.Claims) == 0 {
		return nil
	}

	// Group claims by (symbol, direction).
	type group struct {
		claimIdxs []int
		supports  float64
	}
	bySymDir := make(map[string]map[string]*group) // sym → dir → group
	for i, c := range g.Claims {
		dirs, ok := bySymDir[c.Symbol]
		if !ok {
			dirs = make(map[string]*group)
			bySymDir[c.Symbol] = dirs
		}
		gr, ok := dirs[c.Direction]
		if !ok {
			gr = &group{}
			dirs[c.Direction] = gr
		}
		gr.claimIdxs = append(gr.claimIdxs, i)
		gr.supports += c.Weight
	}

	// Index rebuttal edges by target claim id.
	rebutsByTarget := make(map[string][]Edge)
	for _, e := range g.Edges {
		if e.Kind != EdgeRebuts {
			continue
		}
		rebutsByTarget[e.To] = append(rebutsByTarget[e.To], e)
	}

	verdicts := make([]Verdict, 0, len(bySymDir))
	for sym, dirs := range bySymDir {
		type scored struct {
			direction string
			net       float64
			supports  float64
			claimIdxs []int
		}
		scoredDirs := make([]scored, 0, len(dirs))
		for dir, gr := range dirs {
			rebut := 0.0
			for _, ci := range gr.claimIdxs {
				for _, e := range rebutsByTarget[g.Claims[ci].ID] {
					rebut += e.Strength
				}
			}
			scoredDirs = append(scoredDirs, scored{
				direction: dir,
				net:       gr.supports - rebut,
				supports:  gr.supports,
				claimIdxs: gr.claimIdxs,
			})
		}

		sort.SliceStable(scoredDirs, func(a, b int) bool {
			if scoredDirs[a].net != scoredDirs[b].net {
				return scoredDirs[a].net > scoredDirs[b].net
			}
			return scoredDirs[a].supports > scoredDirs[b].supports
		})

		winner := scoredDirs[0]
		v := Verdict{
			Symbol:      sym,
			Direction:   winner.direction,
			NetStrength: winner.net,
		}

		// Collect winner / opposing claim IDs.
		winSet := make(map[int]struct{}, len(winner.claimIdxs))
		for _, ci := range winner.claimIdxs {
			winSet[ci] = struct{}{}
			v.WinnerClaims = append(v.WinnerClaims, g.Claims[ci].ID)
		}
		for _, sd := range scoredDirs[1:] {
			for _, ci := range sd.claimIdxs {
				v.OpposingClaims = append(v.OpposingClaims, g.Claims[ci].ID)
			}
		}

		// Confidence = mean of winning claims' confidence.
		v.Confidence = meanConfidence(g.Claims, winner.claimIdxs)
		v.Action = directionToAction(winner.direction, v.Confidence)
		v.Reasoning = buildVerdictReasoning(g, winner.direction, winner.claimIdxs)

		// Contested?  Compare to runner-up.
		if len(scoredDirs) > 1 {
			runner := scoredDirs[1]
			ref := winner.net
			if ref < 0 {
				ref = -ref
			}
			if ref > 0 && (winner.net-runner.net)/ref < 0.25 {
				v.Contested = true
			}
			if winner.net <= 0 {
				// No surviving signal at all → mark contested.
				v.Contested = true
			}
		}

		verdicts = append(verdicts, v)
	}

	// Stable output order by symbol.
	sort.SliceStable(verdicts, func(a, b int) bool {
		return verdicts[a].Symbol < verdicts[b].Symbol
	})
	return verdicts
}

// ToConsensus converts verdicts into the legacy ConsensusItem shape so
// callers that already speak ConsensusItem can adopt the debate graph
// without other code changes.
func (g *DebateGraph) ToConsensus() []ConsensusItem {
	verdicts := g.ResolveVerdicts()
	items := make([]ConsensusItem, 0, len(verdicts))

	// Build a quick lookup: claimID → AgentID.
	agentOf := make(map[string]string, len(g.Claims))
	for _, c := range g.Claims {
		agentOf[c.ID] = c.AgentID
	}

	for _, v := range verdicts {
		supporters := uniqueAgents(v.WinnerClaims, agentOf)
		dissenters := uniqueAgents(v.OpposingClaims, agentOf)
		items = append(items, ConsensusItem{
			Symbol:     v.Symbol,
			Direction:  v.Direction,
			Confidence: v.Confidence,
			Supporters: supporters,
			Dissenters: dissenters,
			Action:     v.Action,
			Reasoning:  v.Reasoning,
		})
	}
	return items
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func claimID(round int, op ResearcherOpinion) string {
	return fmt.Sprintf("r%d:%s:%s:%s", round, op.AgentID, op.Symbol, op.Direction)
}

func clampConfidence(c int) int {
	if c < 0 {
		return 0
	}
	if c > 100 {
		return 100
	}
	return c
}

func meanConfidence(claims []Claim, idxs []int) int {
	if len(idxs) == 0 {
		return 0
	}
	sum := 0
	for _, i := range idxs {
		sum += claims[i].Confidence
	}
	return sum / len(idxs)
}

func uniqueAgents(claimIDs []string, agentOf map[string]string) []string {
	seen := make(map[string]struct{}, len(claimIDs))
	out := make([]string, 0, len(claimIDs))
	for _, id := range claimIDs {
		a := agentOf[id]
		if a == "" {
			continue
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func buildVerdictReasoning(g *DebateGraph, direction string, winnerIdxs []int) string {
	if len(winnerIdxs) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Debate verdict %s. Lead arguments: ", direction)
	first := true
	for _, i := range winnerIdxs {
		c := g.Claims[i]
		if !first {
			b.WriteString("; ")
		}
		first = false
		fmt.Fprintf(&b, "%s (r%d, %s, conf=%d) — %s",
			c.AgentName, c.Round, c.Focus, c.Confidence, truncate(c.Reasoning, 80))
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func itoa(i int) string {
	// small helper to avoid importing strconv just for this.
	return fmt.Sprintf("%d", i)
}
