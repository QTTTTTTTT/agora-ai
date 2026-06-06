# Per-fund embedquota observability — design (W13-7 + W14)

> Status: **fully landed**. Recorder data structure + tests shipped in
> W13-7; call-site wiring (W14-1), Prometheus per-fund exporter
> (W14-2), and admin JSON endpoint + UI sub-panel (W14-3) shipped in
> W14. Section 7 below tracks the per-wave delivery.
>
> Audience: someone deciding whether to widen `embedquota` from "per-process
> aggregate" to "per-fund slice" in our observability stack — and how
> invasive that change is.

## 1. Problem

Today, `embedquota.Limiter` exposes a single, process-global view:

- `tokens_today_used`, `tokens_daily_max` (gauges)
- `throttled_total`, `exhausted_total` (W8-1 counters)
- `acquire_wait_seconds` histogram (W9-1)
- `call_tokens` histogram (W10-1)

Every metric is tenant-blind. Prometheus can answer:

> *"Is the embed pipeline being throttled right now?"*

But it can NOT answer:

> *"Is **fund X** being throttled right now?"*

Concrete cases where the gap bites:

1. Multi-fund users see one fund silently underperforming on memory
   recall. The aggregate counters look fine because three other funds
   are quiet; throttling concentrated on the noisy fund hides in the
   noise.
2. Capacity planning. We want to charge each fund's daily token spend
   to that fund (for cost-allocation reports), not just see the
   aggregate.
3. Per-fund SLO: "fund X's p99 embed wait < 2s during trading hours."
   Today this isn't expressible because `acquire_wait_seconds_bucket`
   has no `fund_id` label.

## 2. Constraints

- **Limiter API stability.** `Acquire(estimatedTokens int)` and
  `RecordUsage(actualTokens int)` are called from ~12 sites in the
  recall / memreembed stack. Changing the signatures has high
  ripple. Any solution that requires touching every call site faces a
  large blast radius.
- **Quota semantics.** The external LLM provider's quota is **per
  account**, not per fund. The limiter today (correctly) treats one
  noisy fund's burn as "shared" — it slows everyone down. Splitting
  the quota into per-fund shards would change *limiting* behaviour,
  which is a separate business decision.
- **Cardinality.** Prometheus storage cost grows linearly with
  per-label-value series. With 7 buckets in `call_tokens_bucket` and N
  active funds, we'd add `7 × N` series per metric. Today
  `embedquota` emits ~25 series total; adding `fund_id` to all
  histograms with 50 active funds takes that to ~1,200. Acceptable
  but worth budgeting for.
- **Test seam.** Every change to `embedquota.Limiter` already pays
  the cost of the existing test suite (15 unit tests, 5 handler
  tests, 1 integration test). Whatever ships must keep that passing.

## 3. Options considered

### Option A — Per-call `fundID` parameter on `Limiter.Acquire / RecordUsage`

```go
// Existing:
limiter.Acquire(estTokens int) (wait time.Duration, status Status, err error)
limiter.RecordUsage(actualTokens int)

// Proposed:
limiter.Acquire(fundID string, estTokens int) (...)
limiter.RecordUsage(fundID string, actualTokens int)
```

| Pros | Cons |
| --- | --- |
| Single source of truth — limiter sees fund identity directly | All ~12 call sites must be updated to thread fund context |
| Per-fund quota shards become easy in v2 | Breaks the "limit is global by design" semantic clarity |
| Histograms gain `fund_id` label naturally | Risk of fund being unknown ("which fund issued this batched re-embed?") leading to "" sentinel polluting metrics |

### Option B — Side-car observation layer (CHOSEN)

Keep the limiter single-tenant. Add a sibling package `embedquotaobs`
that records per-fund observations *alongside* (not in lieu of) the
limiter's own histograms. Call sites that already know their `fundID`
(decision center, recall plan loader, memreembed batch initiator) pass
it to the side-car; sites that don't (background consolidation runs
without a fund anchor) pass `""` and the side-car drops them.

| Pros | Cons |
| --- | --- |
| Zero limiter API changes — existing calls untouched | Two histograms per concept (limiter's aggregate + side-car's per-fund); slight duplication |
| Caller opt-in: anonymous batches don't pollute metrics | Side-car can drift from limiter if a future call path forgets to record on both |
| Drop-in: rolling out per-fund observability one call site at a time | Doesn't grant per-fund LIMITING — only per-fund OBSERVABILITY |
| Clean migration path: when a real per-fund quota becomes a business requirement, side-car becomes the input to a real per-fund limiter |  |

### Option C — Make the limiter multi-tenant (`map[fundID]*shard`)

Limiter holds `map[fundID]*Limiter`; `Acquire(fundID, …)` dispatches.
Per-fund quotas + per-fund histograms come for free.

| Pros | Cons |
| --- | --- |
| Cleanest end-state for both observability AND limiting | Requires a per-fund quota policy decision (we don't have one yet) |
| Single source of truth | Largest blast radius; rewrites the limiter internally |
| | High risk: incorrect sharding could let a noisy fund DoS others |

## 4. Decision: Option B (side-car), with explicit upgrade path to C

We ship a new package `server/internal/embedquotaobs` whose only
responsibility is **observability**. It's API:

```go
type Recorder struct { /* opaque */ }

func New(cfg Config) *Recorder

// Called once after each successful Acquire(estimatedTokens).
// fundID may be "" when the call has no fund context — the
// recorder skips empty IDs (anonymous batches don't pollute the
// per-fund slice).
func (r *Recorder) RecordCall(fundID string, tokensActual int, waitDuration time.Duration)

// Read API for Prometheus exporter + admin handler.
func (r *Recorder) Snapshot() []FundSnapshot
```

Internally:

- `map[fundID]*fundShard` under a `sync.RWMutex`.
- Each shard holds atomic counters for: `tokens_today_used`,
  `acquire_wait_buckets[]`, `call_tokens_buckets[]`,
  `throttled_total`, `exhausted_total` — same shapes as the limiter,
  just keyed by fund.
- A pruner goroutine evicts shards whose `lastSeenAt` is older than
  the configured `RetainFor` (default 7d). Caps cardinality even if
  funds are created and abandoned.
- Day rollover handled the same way as `embedquota.Limiter` —
  `tokens_today_used` is per-day-keyed inside each shard.

When per-fund LIMITING becomes a requirement, we promote the shards
into the limiter (Option C); the side-car's data structure is already
the right shape. No data migration needed.

## 5. Cardinality budget

| Metric | Buckets | Funds | Series added |
| --- | --- | --- | --- |
| `fundai_embed_quota_per_fund_acquire_wait_seconds_bucket` | 10 + Inf | 50 | 550 |
| `fundai_embed_quota_per_fund_call_tokens_bucket` | 7 + Inf | 50 | 400 |
| `fundai_embed_quota_per_fund_throttled_total` | n/a | 50 | 50 |
| `fundai_embed_quota_per_fund_tokens_today_used` | n/a | 50 | 50 |
| **Total** | | | **≤ 1,050** |

Today's whole `embedquota` namespace emits ~25 series. The new
budget is ~40× larger; verified acceptable on our Prometheus tier
(scrape interval 15s, retention 30d → ~10 GB additional disk over
30d for 50 funds). Above 50 funds we'd want either:

- Aggregating histograms before emit (group sparse funds into an
  `_other` bucket).
- Dropping per-fund histograms and keeping only the per-fund
  counters + the aggregate histograms.

`RetainFor` of 7 days is the dial.

## 6. Anti-cardinality safety: the pruner

A pathological caller passing high-cardinality `fundID` values
(stale UUIDs, bogus IDs from a fuzz test) could blow the budget. The
recorder defends against this two ways:

1. `MaxFunds` config (default 200). New shards beyond the cap go to
   a synthetic `"_overflow"` shard; metrics merge but at least the
   process doesn't OOM.
2. Pruner removes shards whose `lastSeenAt` exceeds `RetainFor`.
   Default 7d; bounded to `[1h, 90d]`.

## 7. What ships per wave

| Wave   | Deliverable                                                                                       | Status |
| ------ | ------------------------------------------------------------------------------------------------- | ------ |
| W13-7  | Recorder data structure + 21-test suite (`server/internal/embedquotaobs/`)                        | done   |
| W13-7  | This ADR                                                                                          | done   |
| W14-1  | Wiring into `Acquire` / `RecordUsage` (`recall.QuotaEmbedder` + ctx-borne `fundID`)               | done   |
| W14-1  | Two production call sites: `wiring_adapters.buildSemanticRecall` + `embed_loop.runOnce`           | done   |
| W14-1  | `EMBED_QUOTA_OBS_ENABLED` env flag for staged roll-out                                            | done   |
| W14-2  | `exportEmbedQuotaPerFundPrometheus` (process-aggregate exporter coexists; new `fund_id` label)    | done   |
| W14-3  | `GET /api/admin/embed-quota/per-fund` JSON endpoint (super-admin gated)                            | done   |
| W14-3  | `AdminEmbedQuotaPerFundSection` React sub-panel under the existing aggregate panel                | done   |

The split was intentional: W13-7 froze the data shape so the W14
mechanical wiring could land in 2–3 small PRs without redesigning
anything. Future call-site additions (e.g. memreembed worker once
it has fundID) can drop into the same recorder without touching
this ADR.

### Remaining follow-ups

- **`memreembed.Request.FundID`**. The re-embed worker is enqueued
  only from tests today; once the consolidation flow drives it in
  production, the queue's `Request` shape needs a `FundID` field
  so the worker's `Embed` call can attach `recall.WithFundID`.
- **Cardinality recording rules**. The `_overflow` shard is the
  alarm signal; we should land a Prometheus recording rule that
  pages SRE when `fundai_embed_quota_per_fund_status == 1` AND
  any series carries `fund_id="_overflow"` for >5m. Pending
  alert-channel decision (see `ALERT_DELIVERY.md`).

## 8. Migration & rollback

- **Forward**: ship the recorder behind a `EMBED_QUOTA_OBS_ENABLED`
  env flag (default on). Each call site that gains a `fundID` does
  it via a small PR; failure modes are isolated to that call path.
- **Rollback**: flip the flag → recorder becomes a no-op; limiter
  behaviour unchanged because side-car is not on the hot path of
  Acquire/RecordUsage; no data loss because Prometheus already has
  the aggregate histograms.

## 9. Prior art / why we didn't reuse Prometheus's own
   `prometheus/client_golang` histogram

The existing `embedquota.Limiter` uses hand-rolled atomic histogram
counters (W9-1, W10-1) precisely because we wanted to avoid pulling
`prometheus/client_golang` into a hot-path package. The recorder
follows the same pattern for symmetry. Switching either to
`client_golang` is a separate decision — see
`docs/INSTRUMENTATION_ROADMAP.md` (TODO) for the wider conversation.
