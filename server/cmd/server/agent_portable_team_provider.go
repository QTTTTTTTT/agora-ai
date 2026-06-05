// agent_portable_team_provider.go — AP10 wiring of the
// agent-portable lessons cross-fund branch into the production
// PM context builder. See docs/AGENT_PORTABLE_LEARNING.md.
//
// The pure rendering / SQL primitives all landed in AP1-AP9
// (alphalesson package). This file is the ONLY production
// call site that lights up the cross-fund branch by handing
// alphalesson.BuildContext a real TeamProvider closure.
//
// Why a separate file rather than inline in wiring_adapters.go:
//
//   * wiring_adapters.go is already 23k+ lines; adding more
//     adapter helpers there makes it harder to find anything.
//   * The provider has its own focused unit test surface
//     (agent_portable_team_provider_test.go) — keeping the
//     producer next to its tests is the convention the rest of
//     the cmd/server package uses for non-trivial helpers.
//   * Future product work (regime selection, per-fund
//     team-source overrides) will land here without tangling
//     into the mega-adapter file.

package main

import (
	"context"
	"strings"

	"github.com/fundai/server/internal/repository"
)

// agentPortableTeamProvider returns a closure suitable for
// alphalesson.ContextOptions.TeamProvider. The closure resolves,
// per BuildContext invocation:
//
//   - The UUIDs of agents currently on the fund's team. Source:
//     fund_team_members rows for the fundID with status='active'.
//     A non-active member (departed, on leave) doesn't bring
//     their agent's portable lessons in — that's the right
//     semantic for "agent IP follows the agent" because if the
//     agent isn't live on this team, this team shouldn't be
//     reading their notebook today.
//
//   - The per-fund opt-out flag. Source:
//     fund.config.allow_agent_portable_imports parsed via
//     agentPortableImportsOptOut. Default (key absent) = imports
//     allowed; explicit false = imports blocked; malformed
//     config = fail-safe blocked.
//
//   - currentRegime: returned empty for now. The regime gate
//     (AP5) is a no-op when the writer side isn't stamping
//     regimes (which is the current state — no caller of
//     WriteAlphaLessons sets RegimeStamp). Wiring the regime
//     classifier requires a product decision on what counts as
//     a fund's "primary" regime when it holds many instruments;
//     deferred to a follow-up. See AP10 commit message and
//     docs/AGENT_PORTABLE_LEARNING.md "Future work" section.
//
// Nil-safety: when either repo is nil, the closure returns
// (nil, "", false) and ListLessons collapses to the legacy
// fund-only path. This preserves the pre-AP10 behaviour for
// smoke / unit-test deployments that don't construct a full
// repo set.
//
// The closure swallows DB errors silently (returning empty
// team) because BuildContext is on the hot path of every PM
// prompt — a transient DB blip should degrade to "no inherited
// lessons this tick" rather than aborting the prompt build.
// The errors are logged at debug level by alphalesson itself
// when ListLessons fails further down.
func agentPortableTeamProvider(
	fundRepo *repository.FundRepo,
	teamRepo *repository.TeamRepo,
) func(ctx context.Context, fundID string) (teamAgentIDs []string, currentRegime string, optedOut bool) {
	return func(ctx context.Context, fundID string) (teamAgentIDs []string, currentRegime string, optedOut bool) {
		// Defensive: a nil teamRepo means the legacy unit
		// test wiring is in play. Return empty so the
		// cross-fund branch stays dormant; ListLessons treats
		// that the same as "no team" and falls back to
		// fund-scoped retrieval.
		if teamRepo == nil {
			return nil, "", false
		}
		members, err := teamRepo.ListByFund(ctx, fundID)
		if err != nil {
			// DB blip — degrade to fund-only retrieval
			// rather than failing the whole prompt build.
			// The downstream slog.Debug in
			// buildAgentTrackRecord will emit the actual
			// error if the lesson list ever fails too.
			return nil, "", false
		}
		// We could pre-allocate len(members) but most teams
		// are small (~5-10) and the inactive filter typically
		// keeps ~all of them; the make-on-first-append cost is
		// negligible vs. the readability win.
		var team []string
		for _, m := range members {
			if !strings.EqualFold(strings.TrimSpace(m.Status), "active") {
				continue
			}
			if strings.TrimSpace(m.AgentID) == "" {
				continue
			}
			team = append(team, m.AgentID)
		}

		// Per-fund opt-out: default-on, explicit-false-to-disable.
		// fundRepo can also be nil in some fixture wirings;
		// when so we skip the lookup and assume default
		// (imports allowed). The team list above is the more
		// load-bearing of the two — without it the cross-fund
		// branch can't return anything regardless of flag.
		opted := false
		if fundRepo != nil {
			fund, err := fundRepo.GetByID(ctx, fundID)
			if err == nil && fund != nil {
				opted = agentPortableImportsOptOut(fund.Config)
			}
			// Lookup error: stay in default-on mode. This
			// matches the conservative / cooperative
			// posture of degrading to fund-only retrieval
			// rather than a noisier failure mode.
		}

		// currentRegime: empty until product decides
		// fund-level regime semantics. See file header.
		return team, "", opted
	}
}
