package strategy

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/fundai/server/internal/regime"
)

// ---------------------------------------------------------------------------
// Service: per-fund strategy-sleeve coordinator
// ---------------------------------------------------------------------------

// Service is the wiring-side façade. The PMAgent constructs one
// per (fund, decision slot), feeds it a Bundle per candidate
// instrument, and consumes the resulting SleeveAction slice.
//
// State: sleeves are immutable post-construction; no caching
// happens inside Service (the OHLC + regime caches that matter
// live one layer up, inside their dedicated services). Service
// is safe for concurrent use only because its sleeves' Evaluate
// methods are pure; we don't take any internal locks.
//
// mutedSleeveRegimes is the Phase 3A-5 self-learning loop: the
// attribution service writes "loser" lessons into the memory
// store; the wiring layer reads them back as a set of
// (sleeve, regime) keys and calls WithMutedSleeveRegimes on
// the freshly-constructed Service. Evaluate then silently drops
// proposals from cells the platform has previously observed
// losing money in, without retraining or LLM cost.
type Service struct {
	policy             Policy
	sleeves            []Sleeve
	mutedSleeveRegimes map[string]struct{}
}

// NewService builds a Service from a fund's effective policy.
// Returns nil (along with an error sentinel) when the policy is
// disabled — callers should short-circuit on that signal so the
// PMAgent doesn't waste OHLC fetches.
//
// The policy is NOT re-normalised here; the caller is expected
// to have run EffectivePolicy already. We do the cheap
// EnabledSleeves filter and pick the implementations that match.
func NewService(policy Policy) *Service {
	if !policy.Enabled || len(policy.EnabledSleeves) == 0 {
		return nil
	}
	sleeves := make([]Sleeve, 0, len(policy.EnabledSleeves))
	for _, name := range policy.EnabledSleeves {
		switch name {
		case "trend":
			params := defaultTrend()
			if policy.Trend != nil {
				params = *policy.Trend
			}
			sleeves = append(sleeves, NewTrendSleeve(params))
		case "mean_reversion", "mean-reversion":
			params := defaultMeanReversion()
			if policy.MeanReversion != nil {
				params = *policy.MeanReversion
			}
			sleeves = append(sleeves, NewMeanReversionSleeve(params))
		case "dual_ma", "dual-ma", "dualma":
			params := defaultDualMA()
			if policy.DualMA != nil {
				params = *policy.DualMA
			}
			sleeves = append(sleeves, NewDualMASleeve(params))
		case "xs_momentum", "xs-momentum", "xsmomentum", "cross_sectional_momentum":
			params := defaultXSMomentum()
			if policy.XSMomentum != nil {
				params = *policy.XSMomentum
			}
			sleeves = append(sleeves, NewCrossSectionalMomentumSleeve(params))
		default:
			slog.Warn("strategy: unknown sleeve name in policy", "sleeve", name)
		}
	}
	if len(sleeves) == 0 {
		return nil
	}
	return &Service{policy: policy, sleeves: sleeves}
}

// ErrNoSleeves is returned by Evaluate when the Service was
// constructed without any matching sleeves (defensive: NewService
// already returns nil in that case, but tests sometimes
// construct a Service{} directly).
var ErrNoSleeves = errors.New("strategy: no sleeves configured")

// BatchSleeve is the optional interface a sleeve implements when
// its signal is only meaningful across the full universe of
// bundles (cross-sectional momentum, factor tilts, anything
// that needs a ranking). EvaluateBatch returns a slice the same
// length as `bundles`, with proposals[i] == nil for any bundle
// the sleeve has no opinion on.
//
// The Service detects this interface at Evaluate time and routes
// through EvaluateBatch instead of looping over Sleeve.Evaluate.
// Per-bundle sleeves (the majority) just implement Sleeve and
// the Service falls back to the legacy per-bundle path.
type BatchSleeve interface {
	Sleeve
	EvaluateBatch(bundles []Bundle) []*Proposal
}

// WithMutedSleeveRegimes installs the attribution feedback set:
// a list of (sleeve, regime) cells that the lesson generator has
// previously flagged as losers. Evaluate drops any proposal whose
// (sleeve, regime) matches a muted key.
//
// Each key is normalised lower-case "sleeve|regime"; empty inputs
// are tolerated (they simply produce no matches). Calling with
// nil clears the mute list. Idempotent.
//
// Returns the receiver so the wiring layer can chain it next to
// NewService().
func (s *Service) WithMutedSleeveRegimes(muted []SleeveRegimeMute) *Service {
	if s == nil {
		return nil
	}
	if len(muted) == 0 {
		s.mutedSleeveRegimes = nil
		return s
	}
	out := make(map[string]struct{}, len(muted))
	for _, m := range muted {
		k := mutedKey(m.Sleeve, m.Regime)
		if k == "" {
			continue
		}
		out[k] = struct{}{}
	}
	s.mutedSleeveRegimes = out
	return s
}

// SleeveRegimeMute is one row of the attribution feedback set.
// Carried as a struct (rather than a flat string) so the
// wiring layer doesn't have to construct keys by hand and tests
// can build the input declaratively.
type SleeveRegimeMute struct {
	Sleeve string
	Regime string
}

// MutedSleeveRegimes exposes the active mute set for diagnostics.
// Returns a copy so callers can't mutate internal state.
func (s *Service) MutedSleeveRegimes() []SleeveRegimeMute {
	if s == nil || len(s.mutedSleeveRegimes) == 0 {
		return nil
	}
	out := make([]SleeveRegimeMute, 0, len(s.mutedSleeveRegimes))
	for k := range s.mutedSleeveRegimes {
		// Split "sleeve|regime"; defensive against malformed
		// keys (shouldn't happen because we built them in
		// mutedKey).
		i := -1
		for idx, ch := range k {
			if ch == '|' {
				i = idx
				break
			}
		}
		if i < 0 {
			continue
		}
		out = append(out, SleeveRegimeMute{Sleeve: k[:i], Regime: k[i+1:]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sleeve != out[j].Sleeve {
			return out[i].Sleeve < out[j].Sleeve
		}
		return out[i].Regime < out[j].Regime
	})
	return out
}

// Evaluate runs every enabled sleeve against every bundle and
// returns the resulting SleeveAction slice, sorted by
// (instrument_key, sleeve) so the wiring layer's merge logic
// sees a stable order.
//
// One Bundle can produce up to len(sleeves) SleeveActions —
// e.g. both trend and mean_reversion could (in theory) fire on
// the same name in different regimes, but the regime gates make
// this rare. We still emit one row per sleeve so the attribution
// agent can see the parallel votes.
//
// Per-sleeve errors (panics) are NOT swallowed — Sleeve.Evaluate
// is expected to be pure. We do filter dropped proposals (nil
// returns, sub-MinConfidence, gated-by-regime) before producing
// output.
func (s *Service) Evaluate(ctx context.Context, bundles []Bundle) ([]SleeveAction, error) {
	if s == nil || len(s.sleeves) == 0 {
		return nil, ErrNoSleeves
	}
	if len(bundles) == 0 {
		return nil, nil
	}
	out := make([]SleeveAction, 0, len(bundles))

	// Pre-partition sleeves into batch / per-bundle so we don't
	// re-detect on every bundle. The batch sleeves see the full
	// slice once; the per-bundle sleeves keep the legacy loop.
	batchSleeves := make([]BatchSleeve, 0)
	perBundleSleeves := make([]Sleeve, 0, len(s.sleeves))
	for _, sleeve := range s.sleeves {
		if bs, ok := sleeve.(BatchSleeve); ok {
			batchSleeves = append(batchSleeves, bs)
		} else {
			perBundleSleeves = append(perBundleSleeves, sleeve)
		}
	}

	// Batch path. Each batch sleeve returns a 1:1-aligned
	// slice of *Proposal; we walk that slice, applying the same
	// regime gate + mute filter + MinConfidence floor the
	// per-bundle path uses. Done first so context cancellation
	// gets observed BEFORE we kick off the (potentially
	// O(N·K log K) sort-heavy) batch.
	for _, sleeve := range batchSleeves {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		proposals := sleeve.EvaluateBatch(bundles)
		for i, p := range proposals {
			if p == nil {
				continue
			}
			b := bundles[i]
			if b.Symbol == "" {
				continue
			}
			if !AllowsRegime(sleeve.PreferredRegimes(), b.Regime) {
				continue
			}
			if s.isMuted(sleeve.Name(), b.Regime) {
				continue
			}
			if !isValidProposal(*p, s.policy.MinConfidence) {
				continue
			}
			out = append(out, SleeveAction{
				Sleeve:        sleeve.Name(),
				Symbol:        b.Symbol,
				InstrumentKey: b.InstrumentKey,
				Market:        b.Market,
				AssetClass:    b.AssetClass,
				Regime:        b.Regime,
				Proposal:      *p,
			})
		}
	}

	// Per-bundle path: legacy loop. Each sleeve sees one bundle
	// at a time and returns at most one proposal.
	for _, b := range bundles {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if b.Symbol == "" {
			continue
		}
		for _, sleeve := range perBundleSleeves {
			// Regime gate: cheap pre-filter, also enforced
			// inside Evaluate as defence in depth.
			if !AllowsRegime(sleeve.PreferredRegimes(), b.Regime) {
				continue
			}
			// Phase 3A-5 lesson gate: attribution has flagged
			// this (sleeve, regime) cell as a money-loser. Skip
			// before paying for the Evaluate() call.
			if s.isMuted(sleeve.Name(), b.Regime) {
				continue
			}
			p := sleeve.Evaluate(b)
			if p == nil {
				continue
			}
			if !isValidProposal(*p, s.policy.MinConfidence) {
				continue
			}
			out = append(out, SleeveAction{
				Sleeve:        sleeve.Name(),
				Symbol:        b.Symbol,
				InstrumentKey: b.InstrumentKey,
				Market:        b.Market,
				AssetClass:    b.AssetClass,
				Regime:        b.Regime,
				Proposal:      *p,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].InstrumentKey != out[j].InstrumentKey {
			return out[i].InstrumentKey < out[j].InstrumentKey
		}
		return out[i].Sleeve < out[j].Sleeve
	})
	return out, nil
}

// EnabledSleeves returns the canonical names of every sleeve
// the Service will actually run. Useful for the dashboard /
// debug surface.
func (s *Service) EnabledSleeves() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.sleeves))
	for _, sl := range s.sleeves {
		out = append(out, sl.Name())
	}
	return out
}

// Policy returns the policy the Service was configured with.
// Exposed so the wiring layer can read MinConfidence / other
// knobs without re-decoding fund.config.
func (s *Service) Policy() Policy {
	if s == nil {
		return Policy{}
	}
	return s.policy
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// isValidProposal screens out malformed / weak proposals. The
// confidence floor catches sleeve implementations that return a
// degenerate "I had to return SOMETHING" verdict; the action
// allowlist defends against a future sleeve mistakenly emitting
// "watch" or some other non-standard verb.
func isValidProposal(p Proposal, minConfidence float64) bool {
	switch p.Action {
	case ActionBuy, ActionSell, ActionHold:
		// ok
	default:
		return false
	}
	if p.Confidence <= 0 {
		return false
	}
	if minConfidence > 0 && p.Confidence < minConfidence {
		return false
	}
	return true
}

// isMuted reports whether a (sleeve, regime) pair has been
// muted by attribution feedback. Pure: zero allocations on the
// hot Evaluate path because the map and keys are pre-built.
func (s *Service) isMuted(sleeveName string, regimeValue regime.Regime) bool {
	if s == nil || len(s.mutedSleeveRegimes) == 0 {
		return false
	}
	_, ok := s.mutedSleeveRegimes[mutedKey(sleeveName, string(regimeValue))]
	return ok
}

// mutedKey is the canonical key the mute map uses. We
// lower-case both sides so callers don't have to remember
// whether the attribution lesson writer emitted "Trend" or
// "trend"; the strategy.Service is the single normalisation point.
func mutedKey(sleeve, regimeName string) string {
	sleeve = lowerTrim(sleeve)
	regimeName = lowerTrim(regimeName)
	if sleeve == "" || regimeName == "" {
		return ""
	}
	return sleeve + "|" + regimeName
}

func lowerTrim(s string) string {
	// Tight loop hand-rolled rather than strings.ToLower +
	// strings.TrimSpace because mutedKey is on the hot path.
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	if start == 0 && end == len(s) {
		// No trim needed; just lower-case.
		b := make([]byte, end)
		for i := 0; i < end; i++ {
			c := s[i]
			if c >= 'A' && c <= 'Z' {
				c += 32
			}
			b[i] = c
		}
		return string(b)
	}
	b := make([]byte, end-start)
	for i := start; i < end; i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		b[i-start] = c
	}
	return string(b)
}

// AllPreferredRegimes is a convenience aggregator the wiring
// layer can use to decide which regimes are worth classifying.
// If no enabled sleeve cares about regime=chop, we don't need to
// run the regime classifier for instruments whose latest bar
// indicates chop. Currently the wiring layer always classifies
// (regime is needed elsewhere), but exposing this here keeps
// the seam open for a future cost optimisation.
func (s *Service) AllPreferredRegimes() []regime.Regime {
	if s == nil {
		return nil
	}
	seen := make(map[regime.Regime]struct{})
	for _, sl := range s.sleeves {
		for _, r := range sl.PreferredRegimes() {
			seen[r] = struct{}{}
		}
	}
	out := make([]regime.Regime, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
