# S9.1 — Alpha-aware Memory

## Why

S8.4 closes the measurement loop by writing every agent decision's
realised α into `agent_reputation_outcomes` and rolling the per-agent
aggregates into `agent_reputation_stats`. But the LLM PM still doesn't
read those numbers when it composes tomorrow's plan — every decision
starts with the same flat prior over every analyst voice in the
roundtable.

S9.1 closes the **feedback loop**. The system now (a) mints durable
memory "lessons" out of the outcomes that crossed an alpha threshold,
(b) stamps them with the agent that called the trade so the PM can
later cite the source, and (c) renders both the rolling leaderboard
and the recent alpha-tagged lessons as a single markdown block that
lands inside the PM prompt right next to `sleeveScorecard` and
`lessonReplay`. The LLM PM treats it as a soft prior on *whose voice
to trust*, not a hard rule.

The upshot is operational: the longer the fund runs, the more
personalised the prompt gets. Brand-new funds see the block omitted
(legacy behaviour); funds with months of history get a tailored
"trust these analysts more, these less" signal threaded into every
decision.

## Data model

### Memory rows (extended)

Migration `074_alpha_aware_memory.sql` adds three columns to both
`memories` and `memories_archive`:

| Column                | Type             | Purpose                                                     |
| --------------------- | ---------------- | ----------------------------------------------------------- |
| `agent_tag`           | `TEXT`           | Stable tag identifying the agent (e.g. `fund_analyst`).     |
| `alpha_vs_benchmark`  | `DOUBLE PRECISION` | Realised α at the lesson's horizon.                       |
| `source_outcome_id`   | `UUID`           | FK back to `agent_reputation_outcomes.id` — keeps lessons idempotent and auditable. |

`alphalesson` uses these to mint a new memory row whenever an
outcome's `|α|` clears `WriteOptions.AlphaThreshold` (default ±1.5%).
The unique constraint on `source_outcome_id` means re-running the
reputation backfill never duplicates a lesson.

## `internal/alphalesson` package

Two thin pieces:

- `alphalesson.Repo`
  - `WriteAlphaLessons(ctx, outcomes, opts)` — single-transaction
    writer the backfill calls after `UpsertOutcomes` succeeds. Skips
    sub-threshold rows and dedupes on `source_outcome_id` so re-runs
    are no-ops.
  - `WriteAlphaLessonsForOutcomes` — satisfies the
    `agentreputation.LessonWriter` interface so the reputation loop
    can chain it in via `WithLessonWriter`.
  - `ListLessons(ctx, ListLessonsParams{FundID, Limit})` — read side
    used by `BuildContext`.
- `alphalesson.BuildContext(ctx, repStats, lessons, fundID, opts)`
  → pure markdown renderer that produces the prompt block. Empty
    string when neither side has data, so the PM prompt simply
    omits the section.

The default `ContextOptions` show the top 3 + bottom 3 agents (only
those with ≥ 5 decisions to filter out noise) plus the 5 most
recent alpha-tagged lessons.

## Wiring

`main.go`:

1. `services.AlphaLessonRepo = alphalesson.NewRepo(db)` builds the
   shared repo once (S8.4 wiring already did this).
2. `agentReputationLoop` is constructed with
   `LessonWriter: services.AlphaLessonRepo`, so every nightly wave
   that produces fresh outcomes also mints fresh lessons in the same
   transaction window.
3. `workflowService.WithAlphaAwareMemory(services.AgentReputationRepo,
   services.AlphaLessonRepo)` forwards both repos into the per-fund
   `runtimePMAgent` so `buildAgentTrackRecord` has something to
   render.

`runtimePMAgent.buildAgentTrackRecord(ctx, fundID)` calls
`alphalesson.BuildContext` and stuffs the result into
`DecisionInput.AgentTrackRecord`. The same defensive contract as
`buildSleeveScorecard` / `buildLessonReplay`: nil repos → empty
string → prompt omits the block.

`decision/prompt.go` adds `agentTrackRecord` to the JSON payload
serialised into the user prompt, and the system prompt's reading
rules teach the model how to weight it (cite the agent label + avg
α; require a stronger thesis when contradicting a high-α agent;
treat absence as "no track record yet").

## Failure modes

- `agentreputation` table empty (brand-new fund): `BuildContext`
  returns `""`; prompt block omitted.
- `alphalesson` table empty (no outcome has crossed α threshold):
  same — leaderboard section may still render, lessons section is
  skipped.
- Either repo unwired (very old test paths): builder no-ops, block
  omitted.
- DB blip mid-render: warning logged, block omitted, plan generation
  continues. Alpha-aware memory is a SOFT prior — the decision must
  never depend on it being present.

## Operational knobs

| Knob                            | Default | Effect                                                        |
| ------------------------------- | ------- | ------------------------------------------------------------- |
| `WriteOptions.AlphaThreshold`   | `0.015` | Minimum |α| before a memory row is minted.                    |
| `WriteOptions.Layer`            | `long_term` | Memory layer the lessons land in.                          |
| `ContextOptions.TopAgents`      | `3`     | Leaderboard top-N rows.                                       |
| `ContextOptions.BottomAgents`   | `3`     | Leaderboard bottom-N rows (skipped when no overlap-free fit). |
| `ContextOptions.MaxLessons`     | `5`     | Cap on alpha-tagged lessons in the prompt.                    |
| `ContextOptions.MinDecisions`   | `5`     | Filter floor — rows with fewer decisions never appear.        |

Operators tune these per-fund in a follow-up PR; today the loop
uses the defaults across every fund.
