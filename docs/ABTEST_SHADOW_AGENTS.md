# A/B Test Shadow Agent Comparison (Card D)

> Status: shipped 2026-05-29.
> Audience: backend / frontend engineers maintaining the A/B comparison surface.

## What this document covers

Card D adds two read-only endpoints + two web panels so the A/B comparison
page can answer two questions that the old surface left blank:

1.  **What did the *other* strategy's agents learn during the shadow run?**
    (lessons, adjustments, summaries, proposed `evolution_config` diff)
2.  **On which symbols did A vs B differ on operational return / turnover?**
    (per-symbol pivot of `ab_test_variant_trades`)

Everything is **fund-agnostic by design** — the endpoints are keyed by
`testId`, not `fundId`, and work for every `strategy_compare` test
regardless of which company / fund created it.

## Endpoints

### `GET /api/abtests/{testId}/shadow-agents`

Returns per-variant shadow agent learning. Wire shape:

```
{
  "testId": "...",
  "variants": [
    {
      "variantKey": "A" | "B",
      "variantName": "...",
      "strategyConfig": { ... },
      "agents": [
        {
          "agentId": "...",
          "agentName": "Portfolio Manager",
          "role": "pm",
          "eventCount": 1,
          "latestTradingDate": "2026-05-28",
          "lessons": [ "..." ],
          "adjustments": [ "..." ],
          "summaries": [ "..." ],
          "specializationLearning": [ { ... } ],
          "proposedEvolutionDiff": {
            "added": { "shadowVariantKey": "B" },
            "changed": { "recentLessons": [ ["prev"], ["new"] ] },
            "removed": [ "lastDailyReturn" ]
          },
          "memories": [ ... ],
          "timeline": [ { tradingDate, summary, lessons, adjustments } ]
        }
      ]
    },
    { ... B side ... }
  ]
}
```

Bounds:

- Lessons / adjustments deduped & capped at 12 per agent.
- Summaries capped at 5 per agent.
- Memories capped at 20 per agent.
- Variants always exactly two elements (A, B). Empty side carries `agents: []`.

Auth: caller must be a member of BOTH the control fund AND the treatment
fund (same gate as `POST /promote-learning`). Service-unwired branch
returns 503 so legacy deployments stay healthy.

### `GET /api/abtests/{testId}/operational-attribution`

Per-symbol A vs B pivot of `ab_test_variant_trades`. Wire shape:

```
{
  "testId": "...",
  "totalA": { "tradeCount": 0, "turnover": 0, "realizedPnL": 0,
              "winTradeRate": 0, "avgPnL": 0 },
  "totalB": { ... },
  "bySymbol": [
    {
      "symbol": "AAPL",
      "tradeCountA": 1, "tradeCountB": 1,
      "realizedPnLA": 100, "realizedPnLB": 200,
      "turnoverA": 10000, "turnoverB": 15000,
      "pnlGap": 100, "gapPctOfNotional": 0.66,
      "winner": "A" | "B" | "tie"
    }
  ]
}
```

Bounds:

- Top 50 rows by `|pnlGap|`; ties broken alphabetically by symbol so
  the order is stable across requests.
- Empty `bySymbol` is normalised to `[]` (never `null`).
- Same auth + 503 contract as `/shadow-agents`.

## Where the data comes from

Both endpoints read from tables the existing AB analyzer already
populates. **No new schema, no new writers, no migration.**

| Endpoint | Table |
|---|---|
| `/shadow-agents` | `ab_test_variant_memory`, `ab_test_agent_learning_events`, `ab_test_variants.team_snapshot`, `agents.evolution_config` (for the diff) |
| `/operational-attribution` | `ab_test_variant_trades` (group by `variant_key, symbol`) |

The existing analyzer fills these tables via:

1. `ensureABShadowExecution` (in `wiring_adapters.go`) — runs as part
   of `AnalyzeTest`. Writes A-side from real fund trades (return bias
   = 1) and B-side from a deterministic scaling of A. Writes one
   learning event per agent per variant per trading date.
2. `corpActionIngestLoop` etc. don't touch these tables.

So Card D is purely a read-side surface — the data flow:

```
AnalyzeTest
  → ensureABShadowExecution  (writes ab_test_variants / _nav / _trades / _learning_events)
  → buildABTestResults       (writes ab_tests.results)

GET /shadow-agents          ─┐
                             ├──── pure read aggregation, NO recompute
GET /operational-attribution ─┘
```

## Frontend

Two new components, both lazy-loaded and collapsible by default:

- `web/src/components/ABShadowAgentPanel.tsx` — A vs B side-by-side
  agent cards with expandable timeline / memories / diff blocks.
- `web/src/components/ABOperationalAttributionTable.tsx` — totals
  cards + per-symbol table with winner badges.

Both panels are mounted into `web/src/pages/ABTestCompare.tsx` between
the promotion section and the decision-diff section. They render for
every `strategy_compare` test regardless of fund. The shadow agent
panel is hidden for `model_change` tests because that pipeline does
not write `ab_test_agent_learning_events`.

States the panels handle correctly:

| State | Rendering |
|---|---|
| Test not yet `analyzed` | "Run Generate analysis first" hint, no network call |
| Service unwired (503) | Inline retry banner |
| User not a member of either fund (403) | Inline retry banner |
| `analyzed` but no learning events | Empty-state copy |
| `analyzed` with data | Full panel |

i18n strings live in `shared/api-client/src/i18n.ts` under `abShadow.*`
and `abAttribution.*`. Both `zh-CN` and `en-US` are populated.

## Smoke verification

The endpoints have been smoked against the local docker compose stack
across three different funds (A-share OCS, A-share 存储基金, US 美股
存储基金) using a manually-minted JWT for the fund owner:

```
shadow-agents (3 different funds)   → 200 + 2 variants × 4-5 agents
operational-attribution (no trades) → 200 + empty bySymbol (correct)
unauthenticated                     → 401
cross-user (smoke@test.local)       → 403
unknown testId                      → 404
```

Reproducing locally:

```sh
docker compose build app && docker compose up -d app
TOKEN=$(python3 mint_jwt.py)                             # see scripts/
curl -H "Authorization: Bearer $TOKEN" \
     http://localhost:8080/api/abtests/{testId}/shadow-agents | jq
curl -H "Authorization: Bearer $TOKEN" \
     http://localhost:8080/api/abtests/{testId}/operational-attribution | jq
```

## Future work

- **Card K**: today's B-side numbers come from deterministic scaling
  (the "[auto-shadow]" rows). The shadow agent panel surfaces a
  banner reminding users that B is sanity-check only. When Card K
  lands (real LLM shadow runs), the banner can be removed.
- **Android parity**: the `@fundai/api-client` types are already
  shared, so a future Android screen can reuse the wire shapes
  verbatim. We did not ship an RN UI in this card.
- **CSV export**: the `bySymbol` table is small enough today that
  paging isn't needed; if that changes, expose a `?format=csv` query
  param on `/operational-attribution` rather than paginating.

## Tests

| Layer | File |
|---|---|
| Handler (HTTP wire shape, 401 / 403 / 404 / 503 / nil-normalisation) | `server/internal/api/ab_shadow_agent_handler_test.go`, `server/internal/api/ab_attribution_handler_test.go` |
| Wiring (pure helpers: `jsonEqual`, `totalsToDTO`, `sortedAgentIDs`, evo-diff no-op) | `server/cmd/server/ab_shadow_agent_wiring_test.go` |
| End-to-end | docker compose smoke (above) |
