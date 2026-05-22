package debate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
)

// LLMRoundtable is the production orchestrator. It runs each
// Researcher in parallel per round, feeds previous-round peer views
// back in, and stops early when all agents' views are
// cosine-similar enough between consecutive rounds.
//
// Researchers is the ordered set of role implementations. The order
// only matters for stability of the OverallStance composition
// (bull first, bear second, quant third reads naturally). Roles
// may repeat — the orchestrator does not enforce uniqueness — but
// in practice the caller wires one of each.
type LLMRoundtable struct {
	Researchers []Researcher
}

// DefaultMaxRounds is the round cap used when DebateInput.MaxRounds
// is non-positive. Two rounds is the sweet spot in practice: round
// 0 establishes positions, round 1 lets each agent rebut, and
// further rounds typically just rehash without new arguments.
const DefaultMaxRounds = 2

// DefaultConvergenceThreshold mirrors the design doc's stated 0.9
// cosine-similarity floor. Tunable per call via
// DebateInput.ConvergenceThreshold.
const DefaultConvergenceThreshold = 0.9

// Run drives the multi-round debate. Returns an error only if every
// researcher failed on every round; otherwise emits a
// best-effort RoundtableOutput so the workflow keeps moving.
func (r *LLMRoundtable) Run(ctx context.Context, input DebateInput) (*RoundtableOutput, error) {
	if r == nil || len(r.Researchers) == 0 {
		return nil, errors.New("debate roundtable: no researchers configured")
	}
	maxRounds := input.MaxRounds
	if maxRounds <= 0 {
		maxRounds = DefaultMaxRounds
	}
	threshold := input.ConvergenceThreshold
	if threshold == 0 {
		threshold = DefaultConvergenceThreshold
	}

	// viewsByRole tracks the latest accepted view per role across
	// rounds. We feed this back to peers as PeerViews and use it
	// for convergence scoring.
	viewsByRole := make(map[AgentRole]*AgentView, len(r.Researchers))
	var allRounds [][]AgentView

	converged := false
	convergedAt := 0
	successesSeen := 0

	for round := 0; round < maxRounds; round++ {
		peerSnapshot := snapshotViews(viewsByRole)
		roundViews := r.runRound(ctx, input, round, peerSnapshot)
		if len(roundViews) == 0 {
			slog.Warn("debate round produced zero views; treating as no-op", "round", round)
			continue
		}
		// Convergence is scored against the previous round's views.
		// On round 0 there is nothing to compare against, so we
		// always run at least round 1 unless maxRounds == 1.
		if round > 0 && allViewsConverged(viewsByRole, roundViews, threshold) {
			updateViews(viewsByRole, roundViews)
			allRounds = append(allRounds, roundViews)
			successesSeen += len(roundViews)
			converged = true
			convergedAt = round + 1
			break
		}
		updateViews(viewsByRole, roundViews)
		allRounds = append(allRounds, roundViews)
		successesSeen += len(roundViews)
	}

	if successesSeen == 0 {
		return nil, fmt.Errorf("debate roundtable: every researcher failed across %d rounds", maxRounds)
	}

	out := r.consolidate(input, viewsByRole, allRounds)
	out.Converged = converged
	if converged {
		out.ConvergedRounds = convergedAt
	} else {
		out.ConvergedRounds = len(allRounds)
	}
	out.Rounds = len(allRounds)
	return out, nil
}

// runRound fans out to every Researcher in parallel and collects
// successful results. Each researcher only receives peer views from
// the *other* roles — feeding back an agent's own prior view would
// bias it toward repetition. Per-researcher errors are logged and
// dropped; the orchestrator treats an empty round as an indication
// to fall out of the loop without an error (the caller will see
// the degraded RoundtableOutput).
func (r *LLMRoundtable) runRound(ctx context.Context, input DebateInput, round int, peers []AgentView) []AgentView {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		out  []AgentView
		errs []error
	)
	for _, agent := range r.Researchers {
		agent := agent
		wg.Add(1)
		go func() {
			defer wg.Done()
			view, err := agent.Debate(ctx, input, round, filterOwnRole(peers, agent.Role()))
			if err != nil {
				slog.Warn("debate researcher error", "role", agent.Role(), "round", round, "err", err)
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			if view == nil {
				return
			}
			view.Role = agent.Role()
			view.Round = round
			mu.Lock()
			out = append(out, *view)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// filterOwnRole returns a new slice that omits the entry whose Role
// matches self. Used so each researcher only ever sees its peers'
// prior views.
func filterOwnRole(peers []AgentView, self AgentRole) []AgentView {
	out := make([]AgentView, 0, len(peers))
	for _, p := range peers {
		if p.Role == self {
			continue
		}
		out = append(out, p)
	}
	return out
}

// snapshotViews returns a stable slice copy so the next round's
// peer-view input is consistent across all goroutines.
func snapshotViews(m map[AgentRole]*AgentView) []AgentView {
	out := make([]AgentView, 0, len(m))
	for _, v := range m {
		if v != nil {
			out = append(out, *v)
		}
	}
	return out
}

// updateViews merges a freshly completed round into the cross-round
// view map. The latest view per role wins (a successful round
// overrides a stale view from an earlier round).
func updateViews(m map[AgentRole]*AgentView, fresh []AgentView) {
	for i := range fresh {
		copy := fresh[i]
		m[copy.Role] = &copy
	}
}

// allViewsConverged is the convergence rule: every role we have
// data for must be cosine-similar (>= threshold) between its
// previous-round and current-round serialized form. Roles missing
// from either round force a non-converged verdict because we don't
// want a flaky agent to silently halt the debate.
func allViewsConverged(prev map[AgentRole]*AgentView, current []AgentView, threshold float64) bool {
	if len(current) == 0 {
		return false
	}
	currentByRole := make(map[AgentRole]AgentView, len(current))
	for _, v := range current {
		currentByRole[v.Role] = v
	}
	if len(currentByRole) < len(prev) {
		return false
	}
	for role, prevView := range prev {
		curView, ok := currentByRole[role]
		if !ok {
			return false
		}
		score := viewSimilarity(*prevView, curView)
		if score < threshold {
			return false
		}
	}
	return true
}

// viewSimilarity scores how close two AgentView snapshots are. We
// serialize each verdict to its KeyPoints + direction signature and
// compute average cosine similarity over the shared symbol set.
// Symbols present in one view but not the other contribute 0 to the
// numerator (treated as "the view changed materially").
func viewSimilarity(a, b AgentView) float64 {
	bySymbol := func(view AgentView) map[string]string {
		m := make(map[string]string, len(view.Verdicts))
		for _, v := range view.Verdicts {
			m[strings.ToUpper(strings.TrimSpace(v.Symbol))] = joinKeyPoints(v)
		}
		return m
	}
	aMap := bySymbol(a)
	bMap := bySymbol(b)
	union := make(map[string]struct{}, len(aMap)+len(bMap))
	for sym := range aMap {
		union[sym] = struct{}{}
	}
	for sym := range bMap {
		union[sym] = struct{}{}
	}
	if len(union) == 0 {
		return 0
	}
	var total float64
	for sym := range union {
		total += cosineSimilarity(aMap[sym], bMap[sym])
	}
	return total / float64(len(union))
}

// cosineSimilarity is a tiny TF-bag-of-words cosine similarity over
// whitespace-separated tokens. Good enough for "did the agent's view
// shift round-over-round?" without pulling in an NLP dependency.
// Empty strings (one side abstained) return 0.
func cosineSimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	tfA := termFreq(tokenize(a))
	tfB := termFreq(tokenize(b))
	if len(tfA) == 0 || len(tfB) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for term, freqA := range tfA {
		dot += float64(freqA) * float64(tfB[term])
		normA += float64(freqA) * float64(freqA)
	}
	for _, freqB := range tfB {
		normB += float64(freqB) * float64(freqB)
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

func tokenize(s string) []string {
	s = strings.ToLower(s)
	fields := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', ',', '.', ';', ':', '!', '?', '|', '/', '\\', '(', ')', '[', ']', '{', '}', '"', '\'':
			return true
		}
		return false
	})
	return fields
}

func termFreq(tokens []string) map[string]int {
	m := make(map[string]int, len(tokens))
	for _, t := range tokens {
		if t == "" {
			continue
		}
		m[t]++
	}
	return m
}

// consolidate walks the final per-role views and produces the
// aggregate RoundtableOutput the wiring layer will hand to the
// PMAgent. Per-symbol verdicts are voted via simple majority with
// quant as the tiebreaker — the quant lens is the most data-
// grounded so it earns the deciding vote.
func (r *LLMRoundtable) consolidate(input DebateInput, viewsByRole map[AgentRole]*AgentView, rounds [][]AgentView) *RoundtableOutput {
	bull := viewsByRole[RoleBull]
	bear := viewsByRole[RoleBear]
	quant := viewsByRole[RoleQuant]

	out := &RoundtableOutput{
		BullCase:  roleStance(bull),
		BearCase:  roleStance(bear),
		QuantCase: roleStance(quant),
	}

	verdictBySymbol := make(map[string]*SymbolDebate, len(input.Universe))
	for _, sym := range input.Universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		verdictBySymbol[key] = &SymbolDebate{Symbol: sym}
	}
	for role, view := range viewsByRole {
		if view == nil {
			continue
		}
		for _, v := range view.Verdicts {
			key := strings.ToUpper(strings.TrimSpace(v.Symbol))
			if key == "" {
				continue
			}
			sd, ok := verdictBySymbol[key]
			if !ok {
				sd = &SymbolDebate{Symbol: v.Symbol}
				verdictBySymbol[key] = sd
			}
			joined := strings.Join(v.KeyPoints, "; ")
			switch role {
			case RoleBull:
				sd.BullCase = strings.TrimSpace(joined)
			case RoleBear:
				sd.BearCase = strings.TrimSpace(joined)
			case RoleQuant:
				sd.QuantCase = strings.TrimSpace(joined)
			}
		}
	}

	// Resolve verdict + dissent per symbol.
	for key, sd := range verdictBySymbol {
		bullDir := directionFor(bull, key)
		bearDir := directionFor(bear, key)
		quantDir := directionFor(quant, key)
		sd.Verdict, sd.DissentVotes = resolveVerdict(bullDir, bearDir, quantDir)
	}

	out.Symbols = make([]SymbolDebate, 0, len(verdictBySymbol))
	// Preserve universe order for stable output.
	seen := make(map[string]struct{}, len(input.Universe))
	for _, sym := range input.Universe {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if sd, ok := verdictBySymbol[key]; ok {
			out.Symbols = append(out.Symbols, *sd)
		}
	}
	// Append any symbols agents introduced beyond the configured universe.
	for key, sd := range verdictBySymbol {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out.Symbols = append(out.Symbols, *sd)
	}

	out.OverallStance = composeOverallStance(out)
	return out
}

func roleStance(view *AgentView) string {
	if view == nil {
		return ""
	}
	return strings.TrimSpace(view.Stance)
}

func directionFor(view *AgentView, symbolKey string) string {
	if view == nil {
		return ""
	}
	for _, v := range view.Verdicts {
		if strings.ToUpper(strings.TrimSpace(v.Symbol)) == symbolKey {
			return v.Direction
		}
	}
	return ""
}

// resolveVerdict applies the majority-with-quant-tiebreak rule.
// Returns (verdict, dissentVotes).
func resolveVerdict(bull, bear, quant string) (string, int) {
	tally := map[string]int{}
	if bull != "" {
		tally[bull]++
	}
	if bear != "" {
		tally[bear]++
	}
	if quant != "" {
		tally[quant]++
	}
	if len(tally) == 0 {
		return "neutral", 0
	}
	// Find max.
	var best string
	bestCount := 0
	for dir, count := range tally {
		if count > bestCount || (count == bestCount && dir == quant) {
			best = dir
			bestCount = count
		}
	}
	dissent := 0
	for dir, count := range tally {
		if dir != best {
			dissent += count
		}
	}
	return best, dissent
}

func composeOverallStance(out *RoundtableOutput) string {
	parts := make([]string, 0, 3)
	if s := strings.TrimSpace(out.BullCase); s != "" {
		parts = append(parts, "bull: "+s)
	}
	if s := strings.TrimSpace(out.BearCase); s != "" {
		parts = append(parts, "bear: "+s)
	}
	if s := strings.TrimSpace(out.QuantCase); s != "" {
		parts = append(parts, "quant: "+s)
	}
	return strings.Join(parts, " || ")
}
