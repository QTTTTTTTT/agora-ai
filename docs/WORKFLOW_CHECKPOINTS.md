# S9.2 — Workflow Checkpoints

## Why

The daily orchestrator already exposes every step result via
`workflow_runs.step_results` (one JSONB blob per run) and the SSE
activity stream. Both are great for the live operator's eye, but
neither lets you drive a **resume action** from an arbitrary step
when something fails halfway through the day:

- `step_results` is one giant blob — there's no stable row to point
  a resume API at.
- The activity stream is in-memory; restart the server and the
  history is gone.
- Today, when the PM step fails at 11:05, the only way to recover is
  to wait for the next nightly scheduler tick, hit "manual trigger"
  for every step downstream of the failure, or restart the whole
  run from `macro_brief`.

S9.2 closes this gap by persisting one row per `(run_id, step)` to a
dedicated `workflow_checkpoints` table and exposing two admin
endpoints over it:

1. A read endpoint that backs the new Admin UI section — operator
   filters by `run_id` or `(fund_id, trading_date)` and sees the full
   timeline plus the error text for any failed row.
2. A resume endpoint that re-fires either the most recent failed /
   paused step (the default) or an explicitly named step. Re-fire
   uses the same orchestrator path as the existing manual-trigger
   workflow, so retries, events, and the next checkpoint write all
   keep working.

## Data model

Migration `075_workflow_checkpoints.sql`:

```sql
CREATE TABLE workflow_checkpoints (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    run_id       UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    fund_id      UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    trading_date DATE NOT NULL,
    step         VARCHAR(64) NOT NULL,
    status       VARCHAR(20) NOT NULL CHECK (status IN
                    ('success','failed','skipped','pending','paused')),
    attempts     INTEGER NOT NULL DEFAULT 1 CHECK (attempts >= 1),
    started_at   TIMESTAMPTZ NOT NULL,
    ended_at     TIMESTAMPTZ NOT NULL,
    duration_ms  BIGINT      NOT NULL DEFAULT 0,
    error_text   TEXT,
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, step)
);
```

The unique constraint on `(run_id, step)` makes the upsert
idempotent — every retry of the same step overwrites the row with
the latest attempt's status / error / duration. The `payload`
column is reserved for the small structured identifiers the resume
path may need later (plan_id, roundtable_id, report counts, …); we
deliberately don't store full LLM responses there.

## Persistence path

`workflow.DailyOrchestrator` grows two new pieces:

- `CheckpointStore` interface — `Save(ctx, CheckpointSnapshot)`.
- `WithCheckpointStore(store)` option constructor.

`runStep` and `recordSkip` both call `persistCheckpoint`, which
forwards the snapshot to the store. Every failure path in the
store call is logged and swallowed — the orchestrator MUST keep
running when the snapshot can't be persisted, because the
in-process state is the source of truth for the current pass. The
next step's upsert will overwrite the row anyway.

Production wiring builds `workflowCheckpointSink` (in
`cmd/server/workflow_checkpoint_sink.go`) which bridges the narrow
`CheckpointStore` contract to `repository.WorkflowCheckpointRepo`.

## Admin endpoints

### `GET /api/admin/workflow-checkpoints`

Query parameters (exactly one of these two combinations):

- `?run_id=<uuid>` — full timeline for the run.
- `?fund_id=<uuid>&trading_date=YYYY-MM-DD` — all checkpoints for
  that (fund, day) pair.

Response:

```json
{
  "checkpoints": [
    {
      "id": "...",
      "run_id": "...",
      "fund_id": "...",
      "trading_date": "2026-06-01",
      "step": "pm_plan",
      "status": "failed",
      "attempts": 2,
      "started_at": "...",
      "ended_at": "...",
      "duration_ms": 2500,
      "error_text": "...",
      "payload": {},
      "created_at": "...",
      "updated_at": "..."
    }
  ]
}
```

### `POST /api/admin/workflow-checkpoints/resume`

Body:

```json
{
  "run_id": "...",
  "step": "pm_plan"  // optional — defaults to latest failed/paused
}
```

The handler resolves the (fund, trading_date) pair from the
checkpoint row itself — operators can't smuggle a different scope
into the request. The trigger goes through
`workflowServiceAdapter.AdminTriggerStep` which mirrors the existing
manual-trigger path (claim → restore → orchestrator.TriggerStep →
persist) but skips the per-user `authorizeFundAccess` call because
the admin handler already enforced `requireAdmin`.

Audit log entry on success:

```json
{
  "action": "workflow_checkpoint.resume",
  "target_type": "workflow_run",
  "target_id": "<run_id>",
  "after": {
    "run_id": "...",
    "fund_id": "...",
    "trading_date": "2026-06-01",
    "step": "pm_plan"
  }
}
```

## Failure modes

- `workflow_checkpoints` table is empty for a run (orchestrator was
  built without `WithCheckpointStore`, or the very first step is
  still running): the list endpoint returns `[]`, the resume
  endpoint returns `409 no_failed_step`.
- DB blip mid-`Save`: the orchestrator logs and moves on. The row
  may be stale (last attempt missed) but the next step's upsert
  fixes the schedule.
- `AdminTriggerStep` returns an error: the response is `500
  resume_failed` with the upstream error text. The fact that the
  resume attempt was made is NOT audit-logged in that case — only
  successful triggers go into the log.

## UI

`AdminWorkflowCheckpointsSection` (in
`web/src/components/AdminWorkflowCheckpointsSection.tsx`):

- Filters: run_id text input (default), or fund_id + date pair.
- One row per checkpoint with colour-coded status badge.
- "Resume from latest failure" header button (the fast path).
- Per-row "Re-fire this step" button on failed / paused rows.
- Feedback banner reports trigger success / failure / "nothing to
  resume" inline.

Mounted on the Admin page right after `AdminAgentReputationSection`.

## Operational knobs

Currently no per-fund knobs — the checkpoint store is on by default
once the DB is wired. A follow-up PR can add:

- A retention policy (e.g. drop checkpoints older than 90 days)
  driven by a periodic loop, since runs older than ~3 months are
  rarely useful for resume.
- A SLA alert on `status='failed'` rows older than 30 minutes
  (these are runs that the operator hasn't gotten to yet).
