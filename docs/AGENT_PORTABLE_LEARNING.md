# Agent-Portable Learning

ADR for the AP1–AP9 commit sequence on the `main` branch
(`5fa4e16` → `b2c37e1`, plus this doc).

## TL;DR

A researcher / analyst agent's lessons used to be hard-bound
to the `fund_id` of the fund they were emitted at. When the
same agent joined a sister fund's team — the platform's
`fund_team_members` table is many-to-many between funds and
agents — its NVDA notes did not come along, and the new
fund's PM prompt was blind to the agent's prior IP. The fix
is a new `'agent_portable'` row in
`memories.visibility`: such a row is owned by an `agent_id`
rather than a `fund_id`, and any fund whose team contains
that `agent_id` can read it at retrieval time, subject to a
sensitivity guard and a per-fund opt-out flag. The retrieval
path additionally filters cross-fund lessons by the current
market regime so a tech-rally NVDA lesson never injects into
a bear-2026 prompt.

## Problem statement

Pre-AP1 schema (`memories`, after migration 020):

```sql
visibility   VARCHAR(20) NOT NULL DEFAULT 'private'
             CHECK (visibility IN ('private', 'fund', 'marketplace'))
sensitivity  VARCHAR(20) NOT NULL DEFAULT 'internal'
             CHECK (sensitivity IN ('public', 'internal', 'secret'))
fund_id      UUID NOT NULL REFERENCES funds(id)
agent_id     UUID REFERENCES agents(id) ON DELETE SET NULL
agent_tag    TEXT
```

`alphalesson.WriteAlphaLessons` (S9.1 backfill driver +
realtime sink) hard-coded `visibility = 'fund'` for every
row and never populated `agent_id`. `alphalesson.ListLessons`
filtered strictly by `fund_id = $1`. Net effect: every
agent's IP was siloed in its origin fund forever.

User signal (verbatim):

> 单研究agent是针对某一方向或者某个股票的，他的学习研究
> 也是根据具体操作进行总结学习，然后这些学习可以在这个
> agent去到其他团队的相同领域基金也需要生效产生收益的，
> 不需要和某个团队进行强绑定，是agent自己的学习经验

Translated: a research agent's instrument-level learnings
are the agent's own IP and should follow it across teams,
not stay locked in the first fund that paid for them.

## Goals

1. **Agent-portable learning.** A researcher / analyst that
   moves from Fund A to Fund B brings its lesson backlog
   along.
2. **Per-fund opt-out.** A regulated / multi-LP fund that
   cannot ingest other funds' lessons can hard-disable the
   import.
3. **Per-row opt-out.** A PM can mark an individual lesson
   "stays in this fund" without disabling the whole feature.
4. **Regime correctness.** A `tech_rally` NVDA lesson must
   not inject into a `bear_2026` PM prompt — the regime
   mismatch makes the lesson actively misleading.
5. **Backwards-compat.** Every existing alphalesson caller
   that doesn't migrate sees byte-identical SQL and rendered
   markdown.
6. **Discoverability.** The PM (and the LLM reading the
   prompt) can tell at a glance which lessons came from
   another fund versus this fund's own history.

## Non-goals

- Reusing the marketplace propagation flow (visibility =
  `marketplace`) for cross-fund lessons. Marketplace is an
  explicit sale listing with money changing hands; agent
  portability is the agent moving employer and bringing its
  notebook.
- Real-time cross-fund replication via Kafka / a side
  channel. The platform is read-after-write consistent on
  `memories` and the retrieval cost of the OR-merge is
  bounded by a partial index, so we just let Postgres do it.
- Cross-tenant (multi-org) propagation. The platform is
  single-tenant per cluster; agent portability stays inside
  a tenant boundary by construction.

## Design

### 1. Visibility enum widening (AP1, migration 091)

Add `'agent_portable'` to the `memories.visibility` CHECK
constraint:

```sql
ALTER TABLE memories
  DROP CONSTRAINT IF EXISTS memories_visibility_check;
ALTER TABLE memories
  ADD CONSTRAINT memories_visibility_check
  CHECK (visibility IN ('private', 'fund', 'marketplace', 'agent_portable'));
```

Semantics:

| value             | owner    | propagation rule                                              |
| ----------------- | -------- | ------------------------------------------------------------- |
| `private`         | user_id  | only the owning user sees it                                  |
| `fund`            | fund_id  | every agent on the fund team sees it (status quo)             |
| `marketplace`     | listing  | listed for sale; consumers pay to subscribe                   |
| `agent_portable`  | agent_id | every fund whose team contains agent_id sees it (NEW IN AP1)  |

Partial index keyed on the hot read pattern:

```sql
CREATE INDEX idx_memories_agent_portable
  ON memories (agent_id, created_at DESC)
  WHERE visibility = 'agent_portable';
```

Backfill of `memories.agent_id` from `memories.agent_tag`
where the tag is a parseable UUID matching an `agents.id`.
This unsticks the existing alphalesson population
(historically `agent_tag` was populated but `agent_id` was
left `NULL`) so AP3's read path can use the FK column
directly.

### 2. Write path (AP2)

`alphalesson.WriteAlphaLessons` now:

- Populates `memories.agent_id` (UUID FK) in addition to the
  legacy `agent_tag` text column. Non-UUID tags
  (`'fund_analyst'`, `'bull_researcher'`) downgrade
  to `agent_id = NULL` via the `nullableUUID()` Go-side gate
  — the row still writes and is still visible fund-locally
  but won't be served via the cross-fund branch (correct,
  since we can't safely propagate a row whose agent identity
  is opaque).
- Resolves `visibility` per-outcome by `AgentKind`:

| `AgentKind`   | Default visibility |
| ------------- | ------------------ |
| `researcher`  | `agent_portable`   |
| `analyst`     | `agent_portable`   |
| `pm`          | `fund`             |
| `advocate`    | `fund`             |
| (unknown)     | `fund`             |

  PM lessons stay fund-private because they encode the
  fund's sleeve weights / risk limits / team interaction
  patterns — none of which transfer when the agent joins a
  different fund.

- Caller-supplied `WriteOptions.Visibility` (explicit
  non-empty value) still wins. Useful for forced-fund-private
  backfills and tests.

- Optional `WriteOptions.RegimeStamp` (e.g. `"trend_up"`)
  appends `"regime:<stamp>"` to the tags array on every row
  in the batch. The writer is the only place that knows the
  regime at lesson-emission time; AP5 retrieval consumes it.

### 3. Read path OR-merge (AP3 + AP4)

`alphalesson.ListLessons` now optionally UNIONs two branches:

```sql
WHERE agent_tag IS NOT NULL
  AND (
    fund_id = $1                                  -- branch 1
    OR (                                          -- branch 2
      visibility   = 'agent_portable'
      AND agent_id = ANY($team::uuid[])
      AND sensitivity <> 'secret'
    )
  )
```

Branch 1 is the legacy fund-scoped path; branch 2 is the
agent-portable path. Set semantics dedupe rows that match
both (common: a researcher emits a lesson at fund A, fund A
is also the querying fund, so both branches match).

New `ListLessonsParams` fields:

- `TeamAgentIDs []string` — UUIDs of agents currently on the
  querying fund's team. Resolved by the caller (typically
  from `fund_team_members WHERE fund_id = $1 AND status =
  'active'`). Empty list disables the cross-fund branch.
- `AllowAgentPortableImports bool` — companion to the
  per-fund opt-out flag. Reserved for future logging /
  audit; the gate today is `ExplicitlyOptedOut`.
- `ExplicitlyOptedOut bool` — hard-disables the cross-fund
  branch even when `TeamAgentIDs` is populated. This is
  what a regulated / multi-LP fund flips when its compliance
  team says "no, we do not ingest other funds' learnings".

Each returned `LessonRow` carries derived
`InheritedFromOtherFund` (true when the row came in via
branch 2 AND its `fund_id != $1`).

### 4. Regime gate (AP5)

When `ListLessonsParams.CurrentRegime` is non-empty, the
cross-fund branch additionally requires:

```sql
AND (
  $regime_tag = ANY(tags)
  OR NOT EXISTS (SELECT 1 FROM unnest(tags) t WHERE t LIKE 'regime:%')
)
```

Two-arm disjunction:

- Lesson tagged with the matching regime → pass.
- Lesson with no regime tag at all → pass (treated as
  regime-agnostic; conservative default for pre-AP5 backlog
  rows where the writer never stamped a regime).
- Lesson tagged with a different regime → blocked.

The regime gate applies to branch 2 ONLY. The fund-scoped
branch (`fund_id = $1`) never sees the filter because a
lesson living in your fund's namespace was already chosen
to apply here — regime mismatch is your operational concern,
not a propagation bug.

### 5. Historical backfill (AP6, migration 092)

Migration 092 retroactively relabels qualifying historical
rows so the AP3 retrieval has something to serve:

```sql
UPDATE memories SET visibility = 'agent_portable',
                    tags = array_append(tags, 'ap6_backfilled')
WHERE visibility   = 'fund'
  AND origin_kind  = 'alpha_lesson'
  AND agent_id     IS NOT NULL
  AND sensitivity != 'secret'
  AND tags && ARRAY['researcher', 'analyst']::text[];
```

The `'ap6_backfilled'` sentinel lets the down migration
revert exactly the rows it relabelled, leaving native AP2
writes alone.

No per-fund opt-out on the write side. The privacy story
lives on the read side (`ExplicitlyOptedOut`). Per-row
opt-out is `sensitivity = 'secret'` which the eligibility
filter respects.

### 6. Secret gate (AP7)

`sensitivity = 'secret'` rows are never propagated cross-fund
regardless of their `visibility`. Two enforcement layers:

- **SQL**: branch 2 of the WHERE clause carries the
  `sensitivity <> 'secret'` literal.
- **Go**: `IsCrossFundEligible(LessonRow) bool` mirrors the
  SQL gate exactly so UI / debug code can render lock
  badges without re-querying.

Drift between the two layers is a package invariant — any
future change to the gate MUST touch BOTH.

### 7. UI / prompt surface (AP8)

There is no React `LessonList` page in this codebase. The
"UI" for alphalesson rows is the markdown context block
that `alphalesson.BuildContext` produces and
`attachAlphaContext` splices into the PM prompt.

`ContextOptions` gained two fields:

- `TeamProvider func(ctx, fundID) (team []string, regime string, optedOut bool)`
  — optional callback the caller hands in to resolve, at
  render time, the cross-fund retrieval params. When nil,
  the legacy fund-only SQL is preserved (backwards-compat
  invariant).
- `InheritedLabel string` — the marker appended to each
  inherited lesson's bullet in the markdown. Defaults to
  `" [inherited]"`; CN seats can pass `" [继承自其他基金]"`.

Each lesson bullet now renders as:

```
- {title} [α=+X.YY%] [inherited]
```

(suffix is empty for fund-native lessons).

## Backwards compatibility

- **Schema** (091): adding a CHECK value is backwards-compat
  for any existing reader; existing rows keep their current
  visibility values until AP6 explicitly relabels them.
- **Write path** (AP2): callers that pass an explicit
  `WriteOptions.Visibility` see no behaviour change. Empty
  `Visibility` now SIGNALS "let the resolver pick" instead of
  defaulting to `'fund'` — pinned by
  `TestWriteOptions_Normalize`.
- **Read path** (AP3): callers that don't pass
  `TeamAgentIDs` see byte-identical SQL — pinned by
  `TestListLessons_HappyPath` and
  `TestBuildContext_NoTeamProviderStaysLegacy`.
- **Backfill** (092): the down migration is exact-inverse
  via the `'ap6_backfilled'` sentinel tag.
- **Markdown** (AP8): callers that don't pass a
  `TeamProvider` see byte-identical markdown.

## Operational notes

### Enabling cross-fund learning for a fund

Three things need to be true:

1. The fund's team has at least one agent that emitted
   `visibility = 'agent_portable'` lessons at another fund.
   Verify with:

   ```sql
   SELECT count(*) FROM memories
   WHERE visibility = 'agent_portable'
     AND agent_id IN (
       SELECT agent_id FROM fund_team_members
       WHERE fund_id = '<your_fund_id>' AND status = 'active'
     )
     AND fund_id != '<your_fund_id>'
     AND sensitivity != 'secret';
   ```

2. `fund.config.allow_agent_portable_imports` is NOT
   explicitly set to `false`. (Absent flag = opt-in;
   explicit `false` = opt-out.)

3. The wiring layer (`attachAlphaContext`) passes a
   `TeamProvider` into `BuildContext`. This is the only
   piece NOT YET wired in production as of AP9 — see
   "Future work" below.

### Disabling for a regulated fund

Set `fund.config.allow_agent_portable_imports = false`. The
TeamProvider implementation will return
`optedOut = true` and the cross-fund branch will be
suppressed at SQL level.

### Marking an individual lesson "stays here"

The PM (or an audit job) sets `sensitivity = 'secret'` on
the row. Both the SQL and Go gates enforce — see
`TestListLessons_SecretRowsBlockedFromCrossFund` and
`TestIsCrossFundEligible_Matrix`.

### Monitoring

Logs to grep for:

- Migration 091 / 092 `RAISE NOTICE` output for backfill
  row counts.
- `alphalesson: insert lesson (agent=... symbol=...)` —
  per-row write logs.
- The `[inherited]` marker in PM prompt logs — confirms
  cross-fund retrieval is actually serving rows.

## Future work

1. **Wire `TeamProvider` in `attachAlphaContext`.** Today
   `BuildContext` is called without a provider so the
   cross-fund branch is dormant. Plumbing requires:
   - FundRepo or a thin query helper for `fund_team_members`.
   - The regime classifier output for the fund's primary
     instrument (already produced by the regime package for
     other consumers).
   - `fund.config.allow_agent_portable_imports` parser.
2. **Per-row export opt-out.** Currently only
   `sensitivity = 'secret'` blocks a row. A future shape
   could be a per-row `cross_fund_disabled` boolean on
   `memories` that the writer sets when the PM clicks a
   "don't share this" toggle. The `IsCrossFundEligible`
   helper would extend; the SQL gate would gain one more
   AND.
3. **Marketplace ↔ agent_portable bridge.** If an agent's
   lessons become valuable to funds that DON'T have that
   agent on their team, the right UX is "buy this agent's
   notebook" via the marketplace flow — a separate ADR.
4. **React LessonList surface.** If operators want to
   audit / filter / curate the lessons backlog, we'll
   eventually need a dedicated UI page. AP8 made the
   `InheritedFromOtherFund` flag available via the LessonRow
   shape so a future page can render the badge directly.

## File index

| File                                              | Phase |
| ------------------------------------------------- | ----- |
| `server/migrations/091_agent_portable_visibility.sql`  | AP1 |
| `server/migrations/092_backfill_agent_portable_lessons.sql` | AP6 |
| `server/internal/alphalesson/repo.go`                  | AP2 / AP3 / AP5 / AP7 |
| `server/internal/alphalesson/repo_test.go`             | AP2 / AP4 / AP5 / AP7 |
| `server/internal/alphalesson/context.go`               | AP8 |
| `server/internal/alphalesson/context_test.go`          | AP3 (row-shape update) / AP8 |
| `docs/AGENT_PORTABLE_LEARNING.md`                      | AP9 (this doc) |

## Commit ledger

| SHA       | Phase | Notes                                          |
| --------- | ----- | ---------------------------------------------- |
| `5fa4e16` | AP1   | migration 091 + agent_id backfill              |
| `bb06235` | AP2   | write path per-AgentKind visibility + FK       |
| `4363992` | AP3+4 | read path OR-merge + 8-case cross-fund matrix  |
| `3a70bc0` | AP5   | regime-aware filter for cross-fund retrieval   |
| `066b896` | AP6   | migration 092 backfill historical alpha lessons |
| `30caa36` | AP7   | secret-sensitivity gate + IsCrossFundEligible  |
| `b2c37e1` | AP8   | inheritance label + TeamProvider hook          |
| (this commit) | AP9 | ADR                                          |
