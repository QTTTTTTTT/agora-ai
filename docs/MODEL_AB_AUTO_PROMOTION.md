# Model A/B auto-promotion — Sprint 13

> Nightly scanner that turns a "B arm beat the primary for 7 days
> straight" signal into an admin-actionable promotion draft.

## Why a draft, not an auto-flip

We deliberately stop short of writing the recommended model into
production traffic. The cost of a wrong auto-promotion (every
fund on the platform suddenly routing PM calls through a
silently-regressing model) is much higher than the cost of one
admin clicking "apply" the next morning. The S13 pipeline:

1. Scanner detects "B is consistently winning".
2. A row is written to `model_ab_promotion_drafts` with status
   `pending`.
3. The admin board surfaces the draft alongside the criteria that
   triggered it and the report snapshot.
4. An admin clicks **Apply** (closes the experiment + audits the
   decision) or **Reject** (records the rejection reason for
   compliance).

The "apply" step does NOT auto-rewrite per-fund model
configurations — that is left to operators via the existing
`llm.UserOverride` and fund settings tooling.

## Criteria

Defaults (see `internal/modelab/promotion.go::Criteria`):

| Field                       | Default | Meaning                                                                                                                |
| --------------------------- | ------- | ---------------------------------------------------------------------------------------------------------------------- |
| `MinStreakDays`             | 7       | Required consecutive trailing days the arm has to beat the primary.                                                    |
| `MinAgreementPct`           | 0.75    | Daily fraction of shadow outputs that must agree with the primary on the `stance` field.                               |
| `MinSampleSize`             | 20      | Minimum shadow_count per day. Days below this are NEUTRAL (skipped) — quiet weekends don't reset the streak.           |
| `MaxErrorRate`              | 0.05    | error_count / shadow_count per day.                                                                                    |
| `MaxCostRegressionPct`      | 0.2     | Shadow may be at most +20% the primary's cost. Negative values (e.g. -0.1) require cost _improvement_.                 |
| `PrimaryArmIndex`           | 0       | Which arm the comparison treats as the production arm. Override in pathological setups.                                |

Defaults are filled by `Criteria.FilledDefaults` before any
evaluation — operators can override by editing the criteria JSON
on the draft repo or by passing custom options into
`newPromotionScanLoop`.

## Streak algorithm

`trailingStreak` walks backwards from today and stops at the first
day that breaks one of the qualifying conditions:

- `agreement < MinAgreementPct` → break.
- `error_rate > MaxErrorRate` → break.
- cost regression `> MaxCostRegressionPct` → break.

Days with `shadow_count < MinSampleSize` are NEUTRAL — they don't
extend the streak but they don't break it either.

## Scanner loop

`server/cmd/server/promotion_scan_loop.go` runs once per 24h
(default) ± 5% jitter. Each wave:

1. Lists all `model_ab_experiments` with `status = running`.
2. For each, asks the `modelab.Scanner` for a Recommendation.
3. For positive recommendations, calls
   `DraftRepo.UpsertPending`, which:

   - Marks any prior `pending` draft for the same experiment as
     `superseded` (so the partial unique index doesn't trip), and
   - Inserts a fresh row.

Operators can trigger an on-demand scan via
`POST /api/admin/model-ab/promotion-drafts/scan` (admin-only,
audit-logged).

## Schema

`server/migrations/079_model_ab_promotion_drafts.sql`:

- `id`, `experiment_id` (FK with `ON DELETE CASCADE`).
- Recommendation pointers: `recommended_arm_index`,
  `recommended_arm_label`, `primary_arm_index`,
  `primary_arm_label`.
- `streak_days`, `evaluated_at`, `window_from`, `window_to`.
- `criteria_payload`, `report_snapshot` (JSONB) — both kept on the
  row so the admin board can render "why did this fire?" without
  re-running the scanner.
- Lifecycle: `pending` → `applied` / `rejected` / `superseded`.
- Audit fields: `applied_by`, `applied_at`, `rejection_reason`.

Partial unique index on `(experiment_id) WHERE status = 'pending'`
guarantees at most one pending draft per experiment.

## API

| Method | Path                                                                  | Notes                                                                         |
| ------ | --------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| GET    | `/api/admin/model-ab/promotion-drafts`                                | List (?status=pending\|applied\|...). Report snapshot omitted.                |
| GET    | `/api/admin/model-ab/promotion-drafts/{id}`                           | Detail. Includes the full report_snapshot for the "why did this fire?" panel. |
| POST   | `/api/admin/model-ab/promotion-drafts/scan`                           | Synchronous on-demand scan. Audit-logged.                                     |
| PATCH  | `/api/admin/model-ab/promotion-drafts/{id}/apply`                     | Closes the source experiment, records the decision.                           |
| PATCH  | `/api/admin/model-ab/promotion-drafts/{id}/reject`                    | Records the rejection reason.                                                 |

All routes require admin auth (`requireAdmin`). Every mutating
endpoint emits an `audit.MutationEvent` row.

## UI

`web/src/components/AdminModelABPromotionSection.tsx` lives on
the admin page next to the existing model A/B board. Features:

- Status filter (pending / applied / rejected / superseded / all).
- "Scan now" button — triggers the synchronous scan endpoint.
- Per-draft table with a deep-link to the experiment, the
  recommendation summary, and three actions: Details, Apply,
  Reject.
- The Apply modal explicitly warns the operator that this action
  does NOT auto-rewrite any fund's model configuration — the
  follow-up rollout is manual. This is intentional: avoiding
  silent cross-fund production changes is the whole point of the
  draft model.

## Future work

Tracked as follow-ups, not in this sprint:

- Auto-create a phase-2 experiment when Apply is confirmed, with
  the winner as the new control.
- Per-scope criteria overrides (e.g. tighter agreement bar for
  scope=global than scope=fund).
- Daily summary email to the on-call distribution list.
