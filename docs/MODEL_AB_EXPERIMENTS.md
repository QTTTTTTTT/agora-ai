# Model A/B Experiments

Sprint 10 introduces **model-level A/B testing**: side-by-side comparison
of different LLMs on the **same** fund / agent / prompt, without forking
the underlying portfolio (which is what `internal/abtest` does for
*strategy* A/B).

## Why this is a separate package

The existing `internal/abtest` engine works by cloning the entire fund
and running both copies independently. That model is correct for
strategy variables ("change the PM persona", "raise the risk cap") but
overkill for a model swap — the question *"does Claude beat GPT on this
PM step?"* can be answered with two side-by-side LLM calls on **the same
prompt** and no portfolio fork.

Model A/B therefore lives in its own package, `internal/modelab`, and
hooks into the `llm.ModelRouter` via a typed function `llm.ModelABHook`.
A nil hook is the no-op default; tests and bare-bones deployments are
unaffected.

## Architecture (Sprint 10.1)

```
business code  →  llm.LLMClient.Chat(ctx, req)
                    ▲
                    │
                    │   inside ModelRouter.ResolveModel:
                    │
                    │     if req.Model != ""    → use it (forensic pin)
                    │     if modelABHook != nil → ask it; if non-nil decision, use arm cfg
                    │     ... (user override / agent default / tier default)
```

The hook is consulted **after** the forensic model pin (so manual replays
keep their model) and **before** per-user / per-agent overrides (so an
operator-initiated experiment overrides a user's preference for its scope).

## Three tables

`076_model_ab_experiments.sql` creates:

| Table                       | Purpose                                                |
| --------------------------- | ------------------------------------------------------ |
| `model_ab_experiments`      | Experiment definition: scope, arms, traffic split, lifecycle. |
| `model_ab_assignments`      | Sticky-arm bindings — one row per `(experiment, run, step, agent)`. |
| `model_ab_shadow_responses` | (S10.2) Raw outputs of every non-primary arm.          |

### `model_ab_experiments` shape

- `scope` ∈ {`global`, `fund`, `agent_role`, `agent_id`} — what the
  experiment matches against.
- `scope_target` — fund_id / role string / agent_id when scope is not
  `global`.
- `step_filter` — optional whitelist of `StepName`s. Empty matches all
  steps.
- `arms` — JSONB array, position 0 is the control. Each element is an
  `ArmConfig`: `{name, provider, model_name, base_url, model_tier,
  temperature, max_tokens}`. API keys are NOT stored here — the router
  fills in `systemAPIKeys` at hook time.
- `traffic_split` — `float[]` summing to ~1.0; index aligns with `arms`.
- `status` ∈ {`draft`, `running`, `paused`, `completed`, `archived`}.
- `max_total_tokens` — optional cost cap; once `tokens_used` crosses
  this, the resolver skips the experiment until the cap is raised.

### Sticky arms

The resolver assigns an arm via a deterministic SHA-256 hash of
`(run_id, step, agent_id)` modulo the cumulative-weight ladder of
`traffic_split`. On the first hit for a tuple it also **persists** the
assignment in `model_ab_assignments` — subsequent calls for the same
tuple read the persisted arm regardless of whether the traffic_split has
been edited in the meantime. This keeps a multi-call workflow (e.g.
`Trader.Propose → Risk.Assess → PM.FinalApprove`) on a single arm.

## What this sprint (10.1) gives us

- Operators can write directly to `model_ab_experiments` and the next
  Chat call across all funds will start routing into the experiment.
- Per-call attribution: every model A/B call produces an
  `model_ab_assignments` row joining `(run_id, step, agent_id)` to
  `(experiment_id, arm_index)`. Existing `usage_records` rows can be
  joined to assignments on that tuple to attribute cost per arm.

## Sprint 10.2 — ShadowDispatcher (delivered)

The router-only path from 10.1 routes ONLY the winning arm; to *compare*
arms you need their outputs side-by-side. The `ShadowDispatcher`
(internal/modelab/dispatcher.go) is an `llm.LLMClient` wrapper that:

1. Asks the `Resolver` whether the call matches an experiment.
2. If no, delegates to the inner client untouched.
3. If yes, fires `len(arms)` calls in parallel: the **primary** arm via
   `Inner.Chat(req)` (so it still goes through the router and steers the
   production response that returns to the caller), and every other arm
   via `ConfigChatClient.ChatWithConfig(req, BuildLLMConfig(arm, hc))`,
   which bypasses the router entirely and pins exactly the arm spec.
4. Non-primary responses are persisted to `model_ab_shadow_responses`
   with `raw_output`, `parsed_output` (when JSON-parsable), latency, and
   token counts.

### Safety properties

- Shadow errors NEVER fail the primary call.
- Primary errors propagate to the caller unchanged (a shadow that
  happens to succeed doesn't mask a primary failure).
- Shadow arms run with a bounded per-call timeout (default 30s) and a
  process-wide semaphore on `MaxConcurrentShadowCalls` (default 8).
- Token budget guard: experiments with `max_total_tokens` set stop
  attracting new traffic once their cumulative output token count
  crosses the cap.
- Shadow arms use a `context.Background()`-derived ctx so the dispatcher
  can finish + persist even if the caller's context is cancelled the
  instant the primary returns.

### Integration

`llmRuntime.LLMClient()` now returns the dispatcher when one is wired
(via `AttachModelABResolver`), so every business-side caller
(`decision.LLMDecisionEngine`, `ThreeStageEngine`, sentiment scorer, …)
sees the wrapped client transparently. Production code does not need to
change.

## Sprint 10.3 — Metrics aggregator + admin endpoints (delivered)

`internal/modelab/report.go` adds a `Reporter` that joins
`model_ab_assignments` (primary counts) with `model_ab_shadow_responses`
(shadow latency / tokens / cost / errors) into a per-arm metric
roll-up — `Report{ Experiment, Window, Arms[] }`. Empty-arm rows still
appear in the report so operators can tell which arms are getting zero
traffic vs which are misconfigured.

REST endpoints (`server/cmd/server/admin_model_ab.go`):

```
GET    /api/admin/model-ab/experiments          list + status filter
GET    /api/admin/model-ab/experiments/{id}     one experiment + arms
GET    /api/admin/model-ab/experiments/{id}/report?from=…&to=…
POST   /api/admin/model-ab/experiments          create (validates arms + sum)
PATCH  /api/admin/model-ab/experiments/{id}/status flip lifecycle
```

All mutating endpoints write an `audit.MutationEvent` row. Status flips
also invalidate the in-process resolver cache so a "pause" takes effect
within the next call (instead of after the 30s cache TTL).

Web UI:
`web/src/components/AdminModelABSection.tsx` is mounted under the Admin
page, between the workflow-checkpoint section and the WS-feed section.
It renders three blocks: experiment list (with status filter), per-arm
report table for the selected experiment, and a create-experiment form
(arms + traffic split JSON, scope picker, optional max-token cap).

The shared API client (`@fundai/api-client`) exports the new types
(`ModelABExperiment`, `ModelABArm`, `ModelABReport`, etc.) and
`web/src/lib/api.ts` exposes them via typed `apiGet` / `apiPost` /
`apiPatch` helpers — same pattern as workflow-checkpoints.

## Sprint 10.4 — Operator ergonomics (delivered)

Once `S10.3` shipped, the next pain point was that any change to a
running experiment required deleting and re-creating it. `S10.4` adds
three repo + admin endpoints that close that gap without weakening
the integrity story:

1. **Edit-draft** — `PATCH /api/admin/model-ab/experiments/{id}`
   replaces the mutable columns of an experiment **only while the
   target row is still in `draft` state**. The first guard is in SQL
   (`UPDATE … WHERE status = 'draft'` returns 0 affected rows for
   non-drafts) and the repo translates the empty result into
   `modelab.ErrNotEditable`; the handler then returns HTTP `409` with
   error code `not_editable`. This protects historical comparability:
   a running experiment's arm definitions never change underneath
   already-recorded assignments and shadow responses.

2. **Clone** — `POST /api/admin/model-ab/experiments/{id}/clone`
   reads the source row and inserts a brand-new `draft` experiment
   carrying the same scope, arms and traffic split. Operators use
   this when they want to tweak a live experiment: clone, edit the
   draft, then start the clone. The original keeps running so the
   metrics window stays intact.

3. **Bulk status** — `POST /api/admin/model-ab/experiments/bulk-status`
   flips many experiments to the same target status in a single
   `UPDATE … WHERE id = ANY($2::uuid[])` statement. Used to archive
   sweeps of one-shot experiments at end-of-sprint. The handler also
   calls `Resolver.Invalidate()` so the cached arm map drops the
   archived rows immediately instead of waiting for the next refresh.

All three endpoints write an `admin_change_log` row via
`audit.MutationEvent` so the operator surface is auditable in the
exact same shape as `create_experiment` and `set_status`.

UI changes (`web/src/components/AdminModelABSection.tsx`):

- Each row now has a `Clone` button (always available) and an
  `Edit draft` button (only on `draft` rows).
- Clicking `Edit draft` switches the bottom form into "edit draft"
  mode — same fields, but the submit button calls `updateModelABExperiment`
  and the `start_immediate` checkbox is hidden because edits never
  auto-start.
- A checkbox column drives a contextual bulk-action bar that appears
  once one or more rows are selected: bulk archive / pause / complete.

## What's still missing (later sprints)

- **S11** — orthogonal: surface LLM-failure-fallback as a first-class
  `decision_source` marker so users can see which calls were actually
  LLM-generated vs rule-based fallback.

## Configuration

There are no environment variables for Sprint 10.1. The hook activates
automatically once at least one `model_ab_experiments` row has
`status='running'`. Empty table = router behaves identically to pre-10
revisions.

## Code locations

- `server/migrations/076_model_ab_experiments.sql`
- `server/internal/modelab/types.go` — domain types & validation
- `server/internal/modelab/picker.go` — deterministic arm picker
- `server/internal/modelab/repo.go` — DB access layer
- `server/internal/modelab/resolver.go` — cached experiment lookup + sticky-arm upsert
- `server/internal/modelab/hook.go` — `BuildLLMConfig` + `Resolver.AsLLMHook`
- `server/internal/modelab/dispatcher.go` — `ShadowDispatcher` + `ConfigChatClient`
- `server/internal/modelab/report.go` — `Reporter` + windowed list helpers
- `server/internal/llm/model_ab_hook.go` — typed hook interface installed on `ModelRouter`
- `server/internal/llm/client.go` — `MultiProviderClient.ChatWithConfig`
- `server/cmd/server/wiring_adapters.go` — `llmRuntime.AttachModelABResolver`
- `server/cmd/server/admin_model_ab.go` — admin REST handlers (list / report / create / set-status / update-draft / clone / bulk-status)
- `server/cmd/server/main.go` — `modelab.NewRepo(db)` + attach call
- `shared/api-client/src/index.ts` — TypeScript types
- `web/src/lib/api.ts` — client wrappers
- `web/src/components/AdminModelABSection.tsx` — Admin UI
