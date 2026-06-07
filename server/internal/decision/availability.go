package decision

import (
	"sort"
	"strings"
)

// UnavailableSymbol is one symbol that flowed through the wiring
// layer (operator put it in Universe / Positions) but for which
// every per-symbol signal builder returned no data. Surfaced in
// the PM prompt with an explicit "do not estimate / do not
// fabricate" directive so the LLM does not invent prices or
// indicator values from stale training memory.
//
// Reason is a short machine-readable tag the LLM can quote in
// reasoning so the operator can grep audit logs by failure mode:
//
//   - "no_signal_blocks": every per-symbol fetch came back empty.
//     This is the canonical hallucination-risk case (no quote, no
//     fundamentals, no news, no nothing).
//   - "no_provider": the symbol's market has no registered upstream
//     provider at all (e.g. a futures symbol on a fund wired only
//     for equities). Reserved for future use; currently the wiring
//     layer can only detect the "no_signal_blocks" case from the
//     assembled DecisionInput.
//   - "circuit_open": the upstream provider's circuit breaker was
//     open the entire time this decision ran. Reserved for future
//     use — providerHealthTracker has the data but does not yet
//     surface it back to the decision input.
//
// The shape is intentionally JSON-tagged (lowerCamelCase) so the
// wire format is stable for prompt consumers and admin endpoints.
type UnavailableSymbol struct {
	Symbol string `json:"symbol"`
	Reason string `json:"reason"`
}

// Reason tags. Kept as untyped string constants so the package
// boundary stays simple — these flow into LLM prompts and audit
// logs verbatim, not into typed APIs.
const (
	UnavailableReasonNoSignalBlocks = "no_signal_blocks"
	UnavailableReasonNoProvider     = "no_provider"
	UnavailableReasonCircuitOpen    = "circuit_open"
)

// ComputeUnavailableSymbols inspects an already-assembled
// DecisionInput and returns the list of symbols that appear in
// Universe or Positions but have NO per-symbol signal block.
// Pure function: no I/O, no allocation beyond the result slice
// and an internal "seen" set. Safe to call inline in the wiring
// layer right before Decide().
//
// "Has data" means at least one of the per-symbol signal builders
// produced a row for the symbol:
//
//   - QuantSnapshots / UniverseRanking
//   - QualityScores / ValueScores / LowBetaScores
//   - Cooldowns / NewsCatalysts
//   - PEAD.Signals / EarningsCalendar.PerSymbol
//   - IntradaySnapshots
//
// We deliberately do NOT count membership in Universe or Positions
// itself as "has data" — being in the universe is exactly what
// makes a coverage gap dangerous (the operator referenced it; the
// LLM will be tempted to opine on it; the model has no anchor).
//
// Returned slice is sorted by Symbol so the prompt JSON and any
// audit logs are deterministic across calls.
func ComputeUnavailableSymbols(in DecisionInput) []UnavailableSymbol {
	seen := make(map[string]struct{}, len(in.Universe)+len(in.Positions))

	addSym := func(sym string) {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" {
			return
		}
		seen[sym] = struct{}{}
	}

	for _, x := range in.QuantSnapshots {
		addSym(x.Symbol)
	}
	for _, x := range in.UniverseRanking {
		addSym(x.Symbol)
	}
	for _, x := range in.QualityScores {
		addSym(x.Symbol)
	}
	for _, x := range in.ValueScores {
		addSym(x.Symbol)
	}
	for _, x := range in.LowBetaScores {
		addSym(x.Symbol)
	}
	for _, x := range in.Cooldowns {
		addSym(x.Symbol)
	}
	for _, x := range in.NewsCatalysts {
		addSym(x.Symbol)
	}
	if in.PEAD != nil {
		for _, sig := range in.PEAD.Signals {
			addSym(sig.Symbol)
		}
	}
	if in.EarningsCalendar != nil {
		for sym := range in.EarningsCalendar.PerSymbol {
			addSym(sym)
		}
	}
	for _, x := range in.IntradaySnapshots {
		addSym(x.Symbol)
	}

	// Build the candidate set (Universe ∪ Positions) deduplicated
	// in insertion order so the diagnostic surfaces operator-typed
	// symbols first.
	candidates := make([]string, 0, len(in.Universe)+len(in.Positions))
	candidateSet := make(map[string]struct{}, cap(candidates))
	addCandidate := func(sym string) {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			return
		}
		if _, ok := candidateSet[key]; ok {
			return
		}
		candidateSet[key] = struct{}{}
		candidates = append(candidates, key)
	}
	for _, sym := range in.Universe {
		addCandidate(sym)
	}
	for _, p := range in.Positions {
		addCandidate(p.Symbol)
	}

	out := make([]UnavailableSymbol, 0)
	for _, sym := range candidates {
		if _, ok := seen[sym]; ok {
			continue
		}
		out = append(out, UnavailableSymbol{
			Symbol: sym,
			Reason: UnavailableReasonNoSignalBlocks,
		})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}
