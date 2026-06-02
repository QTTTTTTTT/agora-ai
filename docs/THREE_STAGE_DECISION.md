# Sprint 9.4 — Three-Stage Decision Pipeline

This document covers the split of `PM.Decide` into
`Trader.Propose → Risk.Assess → PM.FinalApprove`.

## 1. Why

Until 9.4, the workflow's portfolio decision was a single LLM call
that asked one model to produce the entire plan (stance, action list,
sizes, reasoning) in one shot. That worked at low complexity but
collapsed three distinct fund-desk roles into one prompt:

| Role | Job | Bias when conflated |
| --- | --- | --- |
| Trader | Pitch concrete trades with size + urgency. | LLM emits vague prose instead of trades. |
| Risk officer | Push back on sizing / concentration / vol. | LLM rationalises its own proposal. |
| Portfolio manager | Final authoriser, balances both. | LLM has no second-pair-of-eyes to disagree with. |

Splitting them into three stages — each its own LLM call with its
own system prompt — mirrors how a real institutional desk takes a
trade from idea to authorisation, and produces a more disciplined
plan because each stage can only do its narrow job.

## 2. Pipeline

```text
                +-----------------------+
   DecisionInput|                       |
   ────────────►|  Stage 1: Trader.     |
                |     Propose           |
                |  (TierStandard LLM)   |
                +----------+------------+
                           | TraderProposal {
                           |   stance, actions[{symbol,side,qty,urg,conf}],
                           |   reasoning
                           | }
                           v
                +-----------------------+
                |  Stage 2: Risk.       |
                |     Assess            |
                |  (TierStandard LLM)   |
                +----------+------------+
                           | RiskAssessment {
                           |   verdict, concerns[{sym,sev,reason}],
                           |   mitigations[], commentary
                           | }
                           v
                +-----------------------+
                |  Stage 3: PM.         |
                |     FinalApprove      |
                |  (TierCritical LLM,   |
                |   inner LLMDecision-  |
                |   Engine)             |
                +----------+------------+
                           v
                       DecisionOutput
                       (final action list)
```

The three stages are sequential because:
- Stage 2 needs the proposal to critique.
- Stage 3 needs both the proposal AND the critique to weigh them.

Total worst-case latency is ~3 × `StageTimeout`. The stage timeout
defaults to 60s.

## 3. Engineering posture

- `ThreeStageEngine` satisfies `decision.DecisionEngine` — the same
  interface the legacy `LLMDecisionEngine` implements. Downstream
  callers (workflow runtime, fallback gates, audit logging) need
  zero changes.
- Stage 3 delegates to an INNER `DecisionEngine` (always the
  existing `LLMDecisionEngine`). The PM-final prompt is the
  unchanged production prompt — only two new fields are added to
  `DecisionInput`:
  - `TraderProposal` — markdown-rendered proposal.
  - `RiskAssessment` — markdown-rendered concerns + mitigations.
- The system prompt rules (see `internal/decision/prompt.go`)
  describe how the LLM should weigh the proposal and assessment as
  soft priors:
  - `verdict=veto` on an action → default-drop unless PM can
    articulate why risk officer is wrong.
  - `severity=block` on a concern → per-action veto.
  - `severity=warn` → apply mitigation if possible.
  - `severity=info` → take the action, log the concern.
- The wrapper degrades to single-stage behaviour when the LLM
  client is nil, so feature-detection tests never silently
  invent calls.

## 4. Failure handling

| Failure | Behaviour |
| --- | --- |
| Trader stage errors | Returns wrapped error; PM stage is NOT called. Caller falls back to deterministic engine. |
| Risk stage errors | Returns wrapped error; PM stage is NOT called. Caller falls back to deterministic engine. |
| PM stage errors | Returns wrapped error; same fallback path the legacy engine had. |
| Trader timeout | `ErrStageTimedOut` returned; caller can `errors.Is` and choose fallback policy. |
| Risk timeout | Same `ErrStageTimedOut` path. |
| LLM client nil | Wrapper delegates straight to the inner engine — degrades to single-stage. |
| Inner engine nil | Wrapper returns "inner engine not configured" — a wiring bug. |

## 5. Configuration

Opt-in via env. Defaults are CHOSEN so that the cost surface is
proposal+risk on `standard` + PM on `critical` ≈ 2x the legacy
single-call cost; operators can raise / lower tiers to taste.

| Env var | Default | Notes |
| --- | --- | --- |
| `PM_THREE_STAGE_DECISION` | unset (off) | Set to anything non-falsy to enable the wrapper. |
| `PM_THREE_STAGE_PROPOSAL_TIER` | `standard` | `simple` / `standard` / `critical`. |
| `PM_THREE_STAGE_ASSESSMENT_TIER` | `standard` | `simple` / `standard` / `critical`. |

## 6. Code locations

- `server/internal/decision/three_stage_engine.go` — `ThreeStageEngine`,
  stage prompts, `TraderProposal`, `RiskAssessment` types and parsers,
  rendering helpers.
- `server/internal/decision/three_stage_engine_test.go` — happy path
  + sentinel error + timeout + parser normalisation tests.
- `server/internal/decision/engine.go` — `DecisionInput` extended with
  `TraderProposal` and `RiskAssessment` string fields.
- `server/internal/decision/prompt.go` — payload struct + system-prompt
  rules updated to render and reference the two new fields.
- `server/cmd/server/wiring_adapters.go` — `buildLLMDecisionEngine`
  wraps with `ThreeStageEngine` when `PM_THREE_STAGE_DECISION` is
  set; `llmTierFromEnv` helper resolves the per-stage tier env vars.
- `server/cmd/server/three_stage_wiring_test.go` — env-flag-driven
  selection tests for `buildLLMDecisionEngine`.

## 7. Acceptance criteria

- The wrapper preserves the existing `DecisionEngine` interface
  (verified by go-build / type-check).
- With the flag off, `buildLLMDecisionEngine` returns the legacy
  `*LLMDecisionEngine` and behaviour is byte-identical to pre-9.4
  (verified by `TestBuildLLMDecisionEngine_FlagOff_ReturnsSingleStage`).
- With the flag on, the wrapper is installed and threading of
  proposal + assessment into the PM input works end-to-end
  (verified by `TestThreeStageEngine_HappyPath_FlowsProposalAndAssessmentToPM`).
- Trader / risk stage failures abort before the expensive PM stage
  fires (verified by the `*_NoPMCall` tests).
- A timeout on any stage surfaces `ErrStageTimedOut` so the caller
  can pick a fallback policy (verified by
  `TestThreeStageEngine_StageTimeout_ReturnsSentinel`).
