# S8.2 — Bull / Bear Forced Debate

Two adversarial researchers — Bull and Bear — argue over the
S8.1 analyst panel's output for one symbol. Neither is allowed
to settle on neutral. Their per-round arguments feed both:

- a lightweight `DebateVerdict` synthesised inline (the PM
  reads this directly), and
- the existing `workflow.DebateGraph` machinery via the
  `AdvocateArgument.ToOpinion()` projection, so support / rebut
  edges and the consensus-item code keep working.

## Motivation

Before S8.2 there were two ways a position got decided:

1. The legacy `RoundtableEngine` ran N generalist researchers
   in parallel, each voting their own direction. The verdict
   was a confidence-weighted majority.
2. The new S8.1 panel ran four specialised analysts whose
   votes were blended by confidence.

Both of those are "information gathering" architectures —
each agent honestly states what its own data supports. They
miss the most interesting moment in real fund meetings: the
moment a senior researcher *forces* the team to argue the
opposite side because nobody has actually stress-tested the
thesis.

S8.2 introduces that explicitly. The two advocates:

- Read the same panel.
- Are *forced* to defend bull / bear (cannot vote neutral).
- See the opponent's previous argument from round 2 onward
  and must rebut it directly.
- Surface the strongest case for their side even when the
  panel is mixed.

The output gives the PM a much richer signal than a vote
count: "the bull case rests on these 3 findings; the bear case
rests on these 3 risks; here is the strongest argument
against each; the late rounds carry more weight."

## Surface map

| Layer | File | Purpose |
| --- | --- | --- |
| Domain | `server/internal/agent/bullbear.go` | `AdvocateStance`, `AdvocateAgent`, `AdvocateArgument`, `BullResearcher`, `BearResearcher`, LLM prompt + JSON envelope |
| Orchestrator | `server/internal/agent/bullbear_debate.go` | `Debate`, `DebateConfig`, `DebateTranscript`, `DebateVerdict`, late-round weighted verdict |
| Tests | `server/internal/agent/bullbear_test.go` | Stance + Validate + forced direction + LLM override + rebuttals from round 2 + verdict weighting |
| Persistence | `server/internal/debaterepo/` + migrations `072_debates.{up,down}.sql` | Two-table parent / child write API |
| REST handler | `server/cmd/server/debate_handler.go` | `POST /run`, `GET /debates`, `GET /debates/{id}` |
| Wiring | `server/cmd/server/wiring_debate.go` | Per-fund default Bull + Bear pair (LLM nil for S8.2; S8.3 swaps it) |
| Shared types | `shared/api-client/src/index.ts` | `AdvocateStance`, `DebateArgument`, `DebateVerdict`, `DebateTranscript`, `DebateRunRequest` |
| i18n | `shared/api-client/src/i18n.ts` | `bullBearDebate` namespace (zh-CN + en-US) |
| API helpers | `web/src/lib/api.ts` | `runFundDebate`, `listFundDebates`, `getFundDebate` |
| UI | `web/src/components/BullBearDebateSection.tsx` | Symbol + rounds form, verdict banner, per-round Bull / Bear side-by-side cards, history list |
| Mount | `web/src/pages/FundPerformance.tsx` | Below the analyst panel, last section on the page |

## Wire shapes

### POST `/api/funds/{fundId}/debates/run`

Body is `AnalystRunRequest` (same as the analyst panel) plus
an optional `rounds` (clamped to `[1, 5]`). Response:

```json
{
  "debate": {
    "id": "uuid",
    "fund_id": "uuid",
    "panel_id": "uuid",
    "symbol": "AAPL",
    "asof": "...",
    "generated_at": "...",
    "verdict": {
      "direction": "bullish",
      "confidence": 62,
      "winner_stance": "bull",
      "bull_confidence": 75,
      "bear_confidence": 58,
      "contested": false,
      "winning_summary": "...",
      "losing_summary": "..."
    },
    "arguments": [
      {
        "agent_id": "bull@<fundId>",
        "agent_name": "Bull Researcher",
        "stance": "bull",
        "symbol": "AAPL",
        "round": 1,
        "direction": "bullish",
        "confidence": 72,
        "thesis": "...",
        "support_points": ["fundamentals: ...", ...],
        "rebuttals": [],
        "cited_reports": ["fundamentals", "technical"],
        "llm_model": "fallback"
      },
      { "stance": "bear", "round": 1, ... },
      { "stance": "bull", "round": 2, ..., "rebuttals": ["counter: ..."] },
      { "stance": "bear", "round": 2, ... }
    ]
  }
}
```

The handler always persists the panel first (so the debate
arguments can FK back to a real `panel_id`), then persists the
debate. If only the debate persist fails, the run is still
returned with `persist_error` set so the operator can see the
result.

### GET `/api/funds/{fundId}/debates`

Same filter shape as the analyst panel listing: `symbol`,
`from` (RFC3339), `to`, `limit` (default 100, max 500).

### GET `/api/funds/{fundId}/debates/{debateId}`

Returns one transcript + child arguments. 404 on cross-fund
access (the handler checks `row.FundID == fundID` after the
DB load).

## Verdict rule

`synthesiseVerdict` in `bullbear_debate.go`:

- For each argument, contribute `confidence * round` to the
  side's running score. Round 2 thus counts twice as much as
  round 1.
- Winner = side with the larger weighted score.
- `Confidence = winner_score / (bull_score + bear_score) * 100`,
  clamped to [30, 95].
- `Contested = true` when the margin between bull and bear is
  less than 20% of the larger side.

This is intentionally different from
`workflow.DebateGraph.ResolveVerdicts` — the graph version
operates on `Roundtable.Opinions` and works on net-of-rebut
strength; we want the PM to see a quick first read that
favours the more recent (better-informed) round. The wiring
layer can still feed `ToOpinion()` projections into the graph
when it wants the full edge analysis.

## Backwards compatibility

`AdvocateArgument.ToOpinion()` returns a loose-typed
projection that mirrors `workflow.ResearcherOpinion`. The
wiring layer can therefore build a `Roundtable` out of a
`DebateTranscript` and feed it into the existing
`BuildDebateGraph` + `ResolveVerdicts` pipeline whenever the
edge-level detail is wanted (e.g. for an audit trail).

## Defaults & follow-ups

- **LLM client is nil in S8.2.** Both advocates fall back to
  their deterministic skeleton (panel findings → support;
  panel risks → support for bear; opponent points → counters).
  S8.3 will introduce `LLMClient.CompleteWithSchema` and the
  wiring layer will swap the nil for the real LLM.
- **Default rounds = 2.** Configurable per-request (1..5).
  S8.4 will expose per-fund defaults via the admin UI.
- **Personas are hard-coded** in `wiring_debate.go` (one
  contrarian-optimist Bull, one risk-first Bear). S8.4 will
  add admin-side persona overrides per fund.

## Test coverage

- `internal/agent/bullbear_test.go`:
  Stance round-trip + Validate negatives;
  Bull always-bullish / Bear always-bearish even when panel
  is the opposite direction;
  Confidence boost with supporting analysts;
  LLM reply override of thesis / confidence / support /
  rebuttals; LLM error → fallback;
  Rebuttals appear from round 2 once opponent context is
  supplied;
  `ToOpinion` projection shape, including `rebut:` prefix;
  `synthesiseVerdict` empty / late-round-weighting / close-margin
  contested cases;
  `NewDebate` rejects nil advocates and empty panel symbols.

- `internal/debaterepo/repo_test.go`:
  Save tx + rollback + bad input;
  List with filters; GetTranscript with children;
  GetTranscriptByPanel happy path; reject empty panelID.

- `cmd/server/debate_handler_test.go`:
  Auth; missing symbol; missing providers (503); happy path
  with panel + debate persistence; list; get; cross-fund 404.
