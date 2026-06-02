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

## What's still missing (later sprints)

- **S10.3** — aggregator + REST API + React report page that shows
  decision agreement, latency, cost, and downstream α per arm.
- **S10.4** — admin CRUD UI on top of the REST API so operators don't
  have to write SQL.
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
- `server/internal/llm/model_ab_hook.go` — typed hook interface installed on `ModelRouter`
- `server/internal/llm/client.go` — `MultiProviderClient.ChatWithConfig`
- `server/cmd/server/wiring_adapters.go` — `llmRuntime.AttachModelABResolver`
- `server/cmd/server/main.go` — `modelab.NewRepo(db)` + attach call
