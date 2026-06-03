# Sprint 11 — LLM Failure Disclosure

## Why

Before S11 a PM plan produced by the deterministic fallback heuristic
looked indistinguishable from one produced by a real LLM run:

- Same `investment_plans` row shape.
- `Confidence` was either the LLM's own score (LLM path) or a hardcoded
  `0.55` (fallback path), but no caller knew which.
- The chat-style "Reasoning" field could include strings like
  "fallback strategy applied" only by accident — there was no formal
  contract.

This created three real problems:

1. **User trust.** When a fund subscribed to the AI service got a
   plan that was actually a rule-based fallback, the user had no
   indication of it. After repeated incidents the surface signal
   "this plan came from Claude" became unreliable.
2. **Operator visibility.** Once an LLM provider degraded
   (rate-limited, key revoked, context too long, etc.) the only
   visible symptom was "fallback confidence everywhere" — there
   was no way to attribute the degradation to a category or a
   provider.
3. **Compliance.** Regulated investment surfaces commonly require
   that "automated decisions" be labelled with the system that made
   them. A rule-engine fallback masquerading as an AI decision is
   a defensible risk.

## What S11 ships

S11 introduces a tiered disclosure model:

- **Every user** sees a chip on the plan card indicating one of six
  provenance tags: `llm_pm`, `llm_three_stage`, `fallback_no_llm`,
  `fallback_after_llm_error`, `fallback_empty_plan`, `legacy`.
  Fallback chips render a short, non-technical category
  (`rate_limited`, `service_unavailable`, …) and a suggested next
  step.
- **Admins** see the full `errorclass.Detail` payload — including
  the raw provider summary — on a dedicated LLM-health dashboard.
- **SREs** consume one new Prometheus series,
  `fundai_pm_decision_total{source,category,provider}`, that powers
  the "fallback rate exceeded 5%" alert.

The user-facing surface NEVER includes the raw provider error
string; that information lives strictly on the admin board.

## Schema (S11.1)

`migrations/077_decision_source_disclosure.sql`:

```sql
ALTER TABLE investment_plans
  ADD COLUMN decision_source TEXT NOT NULL DEFAULT 'legacy',
  ADD COLUMN fallback_reason JSONB;

CREATE INDEX idx_investment_plans_fallback
  ON investment_plans (created_at DESC, decision_source)
  WHERE decision_source LIKE 'fallback_%';
```

`decision_source` is intentionally a `TEXT` column rather than a SQL
`ENUM` so a future engine (e.g. an ensemble) can extend the set
without an `ALTER TYPE` migration. The Go errorclass package owns the
authoritative enum.

### Error classifier

`internal/decision/errorclass` is a small library that maps the
heterogeneous error surface produced by the LLM stack into one of:

| Category                  | Meaning                                                              |
| ------------------------- | -------------------------------------------------------------------- |
| `rate_limited`            | Provider returned 429 or platform rate-limiter blocked the call.     |
| `service_unavailable`     | Provider returned 5xx or circuit breaker is open.                    |
| `auth_failed`             | Missing or invalid API key (401 / 403).                              |
| `context_length_exceeded` | Prompt exceeded the model's context window.                          |
| `invalid_request`         | Generic 4xx that isn't rate-limit or auth.                           |
| `schema_validation_failed`| LLM output failed JSON parse or response_schema validation.          |
| `network_timeout`         | Request timed out or upstream connection error.                      |
| `budget_exceeded`         | Call budget guard tripped pre-flight.                                |
| `empty_response`          | Provider returned 200 with empty content.                            |
| `cancelled`               | Call cancelled by the caller (workflow timeout / shutdown).          |
| `unknown`                 | Did not match any known shape (warning logged + counter incremented).|

The categorisation is deterministic and exhaustive: every non-nil
error returned by `decision.DecisionEngine.Decide()` resolves to
exactly one Category. Unknown shapes fall into `CategoryUnknown` AND
are surfaced via the metrics counter so operators can extend the enum
when drift appears.

## Wiring (S11.2)

`runtimePMAgent.buildPlanActions` records the decision source in a
per-fund sync.Map (`lastDecisionSourceByFund`) right after deciding
which path (LLM / fallback) it took. After
`PlanRepo.CreateWithActions` returns, `runtimePMAgent.GeneratePlan`
load-and-deletes the record and calls
`PlanRepo.SetDecisionSource(planID, source, reasonJSON)` to persist
both columns.

Same load-and-delete contract as the G1 attribution writer; see the
inline comment on `lastTraceByFund` for the design rationale.

Soft-fail: a DB error during `SetDecisionSource` logs a warning and
leaves the row at the SQL default `'legacy'`. Never breaks plan
creation.

## API surface (S11.3)

### User-facing — `api.Plan`

```go
type Plan struct {
    // ... existing fields ...
    DecisionSource string              `json:"decisionSource,omitempty"`
    FallbackReason *PlanFallbackReason `json:"fallbackReason,omitempty"`
}

type PlanFallbackReason struct {
    Category string `json:"category"`
    Provider string `json:"provider,omitempty"`
    At       string `json:"at,omitempty"`
}
```

Note `PlanFallbackReason` is the **redacted** projection of
`errorclass.Detail`. We intentionally omit `Summary` (the raw
provider message) and `Model` (vendor-specific identifiers that may
be under contract). The API converter helper
`attachDecisionSource` is the single point that performs this
redaction and is opt-in: endpoints that want the chip call it after
`convertPlan()`, endpoints that don't (bulk list views) skip it.

### Admin-facing

```
GET /api/admin/llm-health/summary[?window_hours=24]
GET /api/admin/llm-health/recent-fallbacks[?window_hours=24&limit=50]
```

The admin endpoint is the **one and only** surface where the raw
provider `Summary` is rendered. Authorisation: `requireAdmin`.

### UI — DecisionSourceChip

`web/src/components/DecisionSourceChip.tsx` renders the chip with a
hover popover. The category → user-facing message dictionary is
embedded in the component so the chip works offline and never has to
read an unbounded category from the wire.

## Monitoring (S11.4)

### Prometheus series

```
fundai_pm_decision_total{source,category,provider}
```

- `source`: one of the six `decision_source` enum values.
- `category`: errorclass.Category, set only for fallback rows.
- `provider`: `openai` / `claude` / `gemini` / "" depending on
  whether the failure happened before a provider was selected.

Cardinality: 6 sources × 11 categories × ~5 providers ≈ 330 series.

### Suggested alert rules

```
# alert when fallback share > 5% sustained 30 minutes
- alert: PMDecisionFallbackRateHigh
  expr: |
    sum(rate(fundai_pm_decision_total{source=~"fallback_.*"}[5m])) /
    sum(rate(fundai_pm_decision_total[5m]))
    > 0.05
  for: 30m
  labels: { severity: warning }
  annotations:
    summary: "Fallback rate > 5% for {{ $labels.fund }}"

# alert on unknown classifier category — extend the enum
- alert: PMDecisionUnknownCategory
  expr: |
    sum(rate(fundai_pm_decision_total{category="unknown"}[1h]))
    > 0
  for: 6h
  labels: { severity: info }
  annotations:
    summary: "errorclass.Category drift; new failure shape — investigate and extend the classifier."
```

### Operator playbook

1. Open the admin LLM-health dashboard.
2. Inspect the **Fallback share** card. If it's red (> 5%):
   - Look at the **By failure category** table — the top row is
     usually the root cause.
   - For `rate_limited` / `service_unavailable` / `network_timeout`:
     transient, wait 5–10m and re-check.
   - For `auth_failed`: check the platform-level credentials or the
     specific fund's user_provider_key entry.
   - For `context_length_exceeded`: a fund's universe grew past the
     model's context window — switch tier or shrink the universe.
   - For `schema_validation_failed`: investigate the prompt or the
     model's compliance with structured output; not user actionable.
3. Cross-reference the **Recent fallbacks** table — clicking a plan
   ID jumps to the decision-trace page for that specific plan
   (planned in a follow-up).

## Non-goals

- We do NOT auto-retry failed LLM calls at the decision-engine
  level beyond the existing retry policy in `llm.MultiProviderClient`.
  Fallback firing means we've already exhausted the retry budget.
- We do NOT expose the raw `Summary` or `Model` to non-admin users
  under any code path. This is enforced by `attachDecisionSource`
  performing the projection.
- We do NOT change the existing `Confidence` semantics. Fallback
  plans still get `0.55` — the chip surfaces the provenance
  information that `Confidence` alone could not.

## Files

```
server/
  migrations/077_decision_source_disclosure.sql
  migrations/077_decision_source_disclosure.down.sql
  internal/decision/errorclass/errorclass.go
  internal/decision/errorclass/errorclass_test.go
  internal/repository/fund_repo.go              (+ SetDecisionSource, GetDecisionSource)
  internal/repository/plan_decision_source_test.go
  internal/repository/llm_health_repo.go
  internal/api/fund_handler.go                  (+ DecisionSource, FallbackReason on api.Plan)
  cmd/server/wiring_adapters.go                 (+ runtimePMAgent.recordDecisionSource, consumeDecisionSource, attachDecisionSource)
  cmd/server/decision_source_wiring_test.go
  cmd/server/admin_llm_health.go
  cmd/server/admin_llm_health_test.go
  cmd/server/main.go                            (+ ObservePMDecisionSource, decisionSourceTotal Prometheus series)

shared/api-client/src/index.ts                  (+ LLMHealthSummary, LLMHealthRecentFallback)
web/src/lib/api.ts                              (+ fetchLLMHealthSummary, fetchLLMHealthRecentFallbacks,
                                                   DecisionSource / PlanFallbackReason types)
web/src/components/DecisionSourceChip.tsx       (NEW)
web/src/components/AdminLLMHealthSection.tsx    (NEW)
web/src/pages/DecisionCenter.tsx                (+ chip rendering)
web/src/pages/Admin.tsx                         (+ LLM-health section)
web/src/pages/decisionCenter/types.ts           (+ decisionSource / fallbackReason on ApiPlan)
```
