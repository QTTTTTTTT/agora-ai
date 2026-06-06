# S8.1 — Specialised Analyst Panel

Four first-class analyst agents that vote on a symbol, plus a
panel coordinator that fans them out and aggregates the result.
This replaces the v3 single-`ResearcherAgent` design (one prompt
juggling fundamentals + news + technicals) with role-specialised
prompts and a structured `AnalystReport` schema that downstream
Bull/Bear researchers (S8.2) and the reputation ledger (S8.4)
consume directly.

## Motivation

Before S8.1, the system shipped one `ResearcherAgent` that
switched its behaviour via a `Focus` enum. That worked for a
single-symbol generalist report but produced "jack-of-all-trades"
LLM outputs because the same prompt had to weigh financials,
crowd mood, breaking news, and chart patterns simultaneously.

S8.1 splits that into four roles, each with its own:
- **Input slice** (e.g. fundamentals reads `quality_score` +
  reported metrics; technical reads `quantsnapshot` + signals).
- **Rule-based directional anchor** so the analyst can't drift
  even when the LLM hallucinates.
- **Persona-able LLM prompt** the operator can change without
  touching code.
- **JSON-shaped output** (`AnalystReport`) that's persisted
  verbatim and consumed by Bull/Bear in S8.2.

## Surface map

| Layer | File | Purpose |
| --- | --- | --- |
| Domain types & interface | `server/internal/agent/analyst.go` | `AnalystAgent`, `AnalystReport`, `AnalystInput.*Block`, JSON parsing helpers |
| Fundamentals analyst | `server/internal/agent/analyst_fundamentals.go` | Quality z-score anchored, reports company financials |
| Sentiment analyst | `server/internal/agent/analyst_sentiment.go` | Aggregate-mood anchored, flags source bias |
| News analyst | `server/internal/agent/analyst_news.go` | Material-event-tag anchored, surfaces catalysts |
| Technical analyst | `server/internal/agent/analyst_technical.go` | Regime + MA + MACD + RSI anchored |
| Panel coordinator | `server/internal/agent/analyst_panel.go` | Parallel fan-out, fail-tolerant aggregation |
| Persistence | `server/internal/analystreport/repo.go` + migration `071_analyst_reports.sql` | Two-table parent / child write + read API |
| REST handler | `server/cmd/server/analyst_panel_handler.go` | `POST /run`, `GET /panels`, `GET /panels/{id}` |
| Default wiring | `server/cmd/server/wiring_analyst_panel.go` | Per-fund `AnalystPanelProvider` (nil LLM for S8.1) |
| Shared types | `shared/api-client/src/index.ts` | `AnalystCategory`, `AnalystReport`, `AnalystPanelReport`, `AnalystRunRequest` |
| i18n | `shared/api-client/src/i18n.ts` | `analystPanel` namespace (zh-CN + en-US) |
| API helpers | `web/src/lib/api.ts` | `runFundAnalystPanel`, `listFundAnalystPanels`, `getFundAnalystPanel` |
| UI | `web/src/components/AnalystPanelSection.tsx` | Symbol input + per-category card grid + history |
| Mount point | `web/src/pages/FundPerformance.tsx` | Below Brinson, last section on the Performance page |

## Wire shapes

### POST `/api/funds/{fundId}/analysts/run`

```json
{
  "symbol": "AAPL",
  "asset_class": "equity",
  "market": "us",
  "asof": "2026-06-02",
  "price_last": 198.3,
  "price_change": 0.012,
  "fundamentals": {
    "quality_score": {
      "profitability_z": 0.8,
      "growth_z": 0.6,
      "safety_z": 0.7,
      "composite_z": 0.72,
      "quartile": 1
    },
    "metrics": {"pe": 27, "roe": 0.32}
  },
  "sentiment": {
    "aggregate": {"average": 0.42, "count": 18, "polarity": "bullish"},
    "recent_items": [
      {"title": "AAPL beats Q3", "source": "reuters", "score": 0.7}
    ]
  },
  "news": {
    "headlines": [
      {"title": "AAPL beats Q3", "source": "reuters", "published_at": "2026-06-02T13:00:00Z"}
    ],
    "material_event_tags": ["earnings_beat"]
  },
  "technical": {
    "snapshot": {"regime": "TrendUp", "close": 198.3, "atr14": 3.1, "atr_pct": 1.6, "position_size_ceiling_pct": 6.0},
    "signals": {
      "ma50_over_ma200": 1,
      "macd_hist": 0.21,
      "rsi14": 58,
      "breakout": 1,
      "relative_volume": 1.8,
      "support_distance_pct": 0.06,
      "resistance_distance_pct": -0.01
    }
  },
  "persist": true
}
```

#### Technical signal vocabulary

The technical analyst hard-votes on the following well-known
keys; missing keys are silently skipped, so a partial map is
fine. Use `indicator.Snapshot.AsAnalystSignals()` on the server
side to fill it consistently.

| Key | Type | Vote | Notes |
| --- | --- | --- | --- |
| `ma50_over_ma200` | ±1 | directional | Sign of (SMA50 − SMA200). |
| `macd_hist` | float | directional | Sign of MACD histogram. |
| `rsi14` | 0–100 | reversal | ≥70 → bearish vote (overbought); ≤30 → bullish vote (oversold). |
| `breakout` | ±1 | directional | +1 = close above prior 20-bar high; -1 = close below prior 20-bar low. Range-bound bars omit this key. |
| `relative_volume` | float | confirmation/dilution | ≥1.5 amplifies the existing direction; ≤0.5 drags conviction without voting. Volume alone never sets a direction. |
| `support_distance_pct` | float | informational | (close − support) / close. Surfaces a finding when within 1.5% of support. |
| `resistance_distance_pct` | float | informational | (resistance − close) / close. Surfaces a risk when within 1.5% of resistance. |

Response:

```json
{
  "panel": {
    "id": "uuid",
    "fund_id": "uuid",
    "symbol": "AAPL",
    "asof": "...",
    "generated_at": "...",
    "aggregate_direction": "bullish",
    "aggregate_confidence": 78,
    "categories_voted": 4,
    "per_category_votes": {"fundamentals": 1, "sentiment": 1, "news": 1, "technical": 1},
    "reports": [
      {"agent_id": "fundamentals@uuid", "category": "fundamentals", "direction": "bullish", "confidence": 70, "thesis": "...", "key_findings": [...], "risks": [...], "data_points": [...]}
    ]
  }
}
```

Every block in the input is optional; the analyst whose block is
absent reports `direction: neutral, confidence: 20` with a
"sitting out" finding rather than failing.

### GET `/api/funds/{fundId}/analysts/panels`

Query: `symbol`, `from` (RFC3339), `to` (RFC3339), `limit`,
`include=children`. Returns `{ "panels": AnalystPanelReport[] }`,
descending by `asof`.

### GET `/api/funds/{fundId}/analysts/panels/{panelId}`

Returns one panel with all four child reports inlined.
404 when the panel belongs to a different fund (cross-fund leak
check is enforced by the handler, not just the DB).

## Database schema (migration `071`)

Two tables:
- `analyst_panel_reports` — one row per panel run
  (`fund_id`, `symbol`, `asof`, aggregate fields,
  `per_category_votes` JSONB).
- `analyst_reports` — one row per (panel, category). Links via
  `panel_id` with `ON DELETE CASCADE`. `key_findings`, `risks`,
  `data_points`, `sources` are JSONB arrays. `UNIQUE (panel_id,
  category)` so a single panel run cannot duplicate a category.

Indexes target three query shapes:
- "Last N panels for this fund" → `idx_panel_reports_fund_asof`.
- "Last N panels for this symbol on this fund" →
  `idx_panel_reports_fund_symbol_asof`.
- "Last N reports authored by this agent_id" (S8.4 reputation
  calibration) → `idx_analyst_reports_agent`.

## Aggregation rule

`AnalystPanel.aggregateReports`:

1. Each analyst votes -1 / 0 / +1 based on its `Direction`.
2. Weight = analyst's `Confidence` (0..100).
3. `weighted_score / total_weight ≥ +0.2` → bullish;
   `≤ -0.2` → bearish; else neutral.
4. Aggregate confidence = mean of participating analysts'
   confidence, with +10 boost when 4-of-4 voted and -10 dampen
   when only 1-of-4 voted.

A failing analyst (LLM error, panic, timeout) is logged and
skipped — the panel still publishes the surviving 3 reports.
Only an all-fail panel returns an error.

## Backwards compatibility

`AnalystReport.ToBrief()` projects a report back into the
legacy `ResearchBrief` shape so the dormant `RoundtableEngine`
+ any tests still wired to `ResearcherAgent` keep working.
`AnalystCategory` maps to `ResearchFocus` as:

- `fundamentals` → `fundamental`
- `technical` → `quant`
- `news` / `sentiment` → `stock`

## Defaults & follow-ups

- **LLM client is nil in S8.1.** Every analyst falls back to
  its deterministic rule path. S8.3 will add
  `LLMClient.CompleteWithSchema(ctx, sys, user, schema)` and
  swap the nil here for the real adapter so analysts start
  producing native structured output.
- **Data inputs are POST-body only in S8.1.** Operators (or
  Bull/Bear in S8.2) build the `AnalystInput` from existing
  data they already hold (quality service, sentiment scorer,
  quantsnapshot, news feeds). A future ticket can add a
  one-call `/snapshot` endpoint that auto-builds the input from
  the latest holdings + scorers.
- **Personas are hard-coded per category** in
  `wiring_analyst_panel.go`. S8.4 will introduce an admin
  endpoint to override per-fund per-category personas at
  runtime.

## Test coverage

- `internal/agent/analyst_test.go` — Validate, parse, normalise,
  merge, all 4 analysts (no-LLM fallback + LLM-reply override +
  conflict + LLM-error + no-data floor).
- `internal/agent/analyst_panel_test.go` — Parallel happy path,
  all-agree boost, one-voted dampen, conflicting-direction
  neutral, single failure tolerance, all-fail error,
  per-analyst timeout.
- `internal/analystreport/repo_test.go` — Save tx, rollback,
  list + filter, get + children, list-by-agent.
- `cmd/server/analyst_panel_handler_test.go` — Auth, missing
  symbol, no provider, happy-path no-persist, happy-path with
  persist, list filters, get + 404, cross-fund leak check.
