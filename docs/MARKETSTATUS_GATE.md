# Market-status pre-trade gate (S6.1)

## Goal

Before the simulator (or, eventually, a live broker adapter)
hands an order to the matching engine, ask: "is the market
willing to take this trade right now, at this price?". If the
honest answer is no, refuse. If the answer is "yes, but with
caveats", let it through and tag the caveats so they ride into
attribution.

This is the foundation of the larger **S6 模拟可信度** project —
a sequence of changes that turn the simulator from "fills any
order at last + slippage" into a credible production rehearsal.
Phase 1 (this round) ships: trading calendar + halt / suspended
+ price-limit + stale-quote warning. Future rounds layer on
large-order impact, securities lending fees, IPO lock-up, and a
WebSocket realtime feed.

## Why now

The simulator today fills:

  * a halted ticker (the gate didn't know it was halted);
  * a limit at 1 700 RMB on Maotai when the daily lower limit
    was 1 800 (impossible at the real exchange);
  * an order placed at 02:00 UTC on a Sunday on the SSE
    (closed);
  * an order priced off a 30-minute-old quote (no warning).

Each of those is a silent simulation lie. They make backtest /
paper-trading numbers more optimistic than reality, and worse,
they make the reflexion loop's "lessons learned" pile up on
fictional fills.

## Components

### Schema (migration `063_marketstatus.sql`)

Three small tables:

  * **`instrument_market_status`** — per-instrument live state:
    `status ∈ {trading, halted, suspended}`, halt reason / reopen
    window, daily price-limit floor / ceiling, asset-class hint,
    optional staleness-budget override, last-quote timestamp +
    price (denormalised so the gate reads in one row).
  * **`trading_calendar`** — `(market, trading_date)` keyed:
    `is_open`, local `open_local` / `close_local` in `market_tz`,
    `half_day` flag.
  * **`marketstatus_events`** — append-only audit of every
    reject / warn the gate emitted. We do NOT log allows; that
    would balloon. Operators can replay rejections to verify
    the gate is honouring policy.

### Engine (`internal/marketstatus/`)

Pure / stateless / no DB handles.

  * `Engine.Check(probe, status, day)` returns a `CheckResult`
    with one combined `Decision` and the per-rule `Event` list.
  * Hardest rejects first (`suspended > halted > calendar >
    price_limit > stale`). The first reject short-circuits, so
    the metadata you get back belongs to the most useful primary
    cause.
  * Stale-quote rule never rejects — only warns. Some legitimate
    bonds and OTC names sit between sessions without quote
    updates. Default budget per asset class:

    | Asset class | Default budget |
    |-------------|----------------|
    | equity / etf | 60 s |
    | futures      | 5 s  |
    | crypto       | 10 s |
    | option       | 10 s |
    | otc / bond   | 300 s |
    | (other)      | 60 s fallback |

    Operators override per instrument via
    `instrument_market_status.staleness_budget_seconds`.
  * Price-limit rule is skipped for orders without an intended
    price (market orders) — the matcher's quote will be inside
    the exchange's enforced limits anyway.
  * Calendar enforcement uses the `market_tz` IANA zone so
    DST drift is handled correctly.

### Repo (`internal/marketstatus/repo.go`)

  * `GetByKey` / `UpsertStatus` / `TouchQuote` / `ListStatus`
  * `GetCalendarDay` / `UpsertCalendarDay` / `ListCalendarDays`
  * `InsertEvent` / `ListEvents`

`UpsertStatus` validates: status ∈ {trading, halted, suspended},
lower ≤ upper, staleness budget ∈ [1, 3600] s.
`InsertEvent` refuses to persist `Decision == allow` (no point —
the audit log is for explainability of rejects/warns).

### Broker integration (`broker.MarketStatusGate`)

The broker package exports an interface (`MarketStatusGate`) with
one method `CheckOrder(ctx, MarketStatusProbe) MarketStatusVerdict`.
The `Simulator` calls it BEFORE booking the order, BEFORE
idempotent duplicate detection. A reject returns
`ErrMarketStatusRejected`; warnings ride on `Order.Warnings`.

`broker` does NOT depend on `marketstatus`. The cmd/server-level
glue file `marketstatus_gate.go` is the only place where the two
meet — it loads the rows, runs the engine, persists events, and
returns the verdict.

The gate is **fail-open**: any internal error (DB lookup, engine
failure, persist failure) returns an empty verdict so a misconfig
never becomes a denial-of-service for trading. The metric
`fundai_marketstatus_events_total{event="lookup_failed"}` (and
siblings) tracks how often the gate fell open so operators see
silent degradation.

### REST API (`cmd/server/admin_marketstatus.go`)

| Method | Path                                                              | Purpose |
| ------ | ---------                                                         | ------- |
| GET    | `/api/admin/marketstatus/instruments`                             | List rows (filters: market, status, symbol) |
| GET    | `/api/admin/marketstatus/instruments/{key}`                       | Get one row |
| PUT    | `/api/admin/marketstatus/instruments/{key}`                       | Upsert all fields |
| POST   | `/api/admin/marketstatus/instruments/{key}/halt`                  | Convenience: status=halted with reason+until |
| POST   | `/api/admin/marketstatus/instruments/{key}/unhalt`                | Convenience: status=trading |
| POST   | `/api/admin/marketstatus/instruments/{key}/limits`                | Convenience: set lower/upper |
| GET    | `/api/admin/marketstatus/calendar?market=&from=&to=`              | List calendar days |
| PUT    | `/api/admin/marketstatus/calendar/{market}/{date}`                | Upsert one day |
| GET    | `/api/admin/marketstatus/events`                                  | Audit list (filters: fund_id, instrument_key, rule_code, decision) |

All endpoints require admin and audit-log every mutation.

### Web UI (`AdminMarketStatusSection`)

  * Filters bar (market / status / symbol).
  * Instruments table with status badge, halt reason, lower /
    upper, last quote timestamp, plus per-row Halt / Unhalt /
    Set-limits buttons.
  * Calendar sub-section with from/to date inputs and an
    "Add / edit calendar day" expandable form.
  * Events table at the bottom for the audit feed.
  * Two modal dialogs (Halt + Limits) so operators can act
    without leaving the screen.

Mounted in `Admin.tsx` after `AdminDrawdownSection`.

### Metrics

`fundai_marketstatus_events_total{event="..."}` counter with
event labels:

  * `allow`
  * `reject_<rule>` — `halted`, `suspended`, `price_limit`,
    `market_closed`, `half_day_closed`
  * `warn_<rule>` — `stale_quote`, `half_day_closed`,
    `market_closed` (TZ misconfig)
  * `lookup_failed`, `calendar_lookup_failed`,
    `evaluate_failed`, `persist_failed` — internal hiccups
    that fail-open
  * `admin_halt`, `admin_unhalt`, `admin_set_limits`,
    `admin_upsert`, `admin_calendar_upsert` — operator UI

See `docs/PROMETHEUS_QUERIES.md` §16 for queries and alert
rules.

## Testing

  * `internal/marketstatus/types_test.go` — engine: 20 cases
    covering allow, halt+expired-halt-doesn't-block, suspended,
    price-limit lower/upper/market-skip, stale-quote across
    asset classes + override + missing timestamp, calendar
    closed/before-open, half-day warn-during / reject-after,
    bad-tz warn, hardest-reject short-circuit, nil engine and
    invalid probe.
  * `internal/marketstatus/repo_test.go` — repo: get-not-found,
    get-happy, upsert-validation (bad status / lower>upper /
    bad staleness budget), upsert-happy, touch-quote-happy,
    calendar-defaults, get-calendar-not-found, insert-event
    refuses-allow / happy.
  * `internal/broker/simulator_marketstatus_test.go` — gate
    integration: reject blocks order, default reason, warnings
    attach to Order, nil gate is no-op, probe carries
    LimitPrice / StopPrice as IntendedPrice.
  * `cmd/server/admin_marketstatus_test.go` — admin endpoints:
    auth (401/403), list happy, upsert validation (bad status,
    lower>upper), halt missing-key, calendar missing-market,
    calendar bad-date; helper parsers.

All tests pass: `go test ./internal/marketstatus
./internal/broker ./cmd/server` exit 0.

## Future work (subsequent S6 rounds)

  1. **Large-order market impact** — slippage proportional to
    `notional / ADV` rather than a flat bps. Likely a new
    `marketimpact` package consumed by `matching.Engine`.
  2. **Securities lending / locate** — short-sells need a borrow
    fee; some names are HTB or impossible to borrow. Wire into
    the same gate pipeline (extra rule).
  3. **IPO lock-up** — block sells on positions inside the
    insider lock-up window. New rule + per-position metadata.
  4. **WebSocket realtime feed** — replace the REST polling
    quote pipeline with a push channel. Will let
    `last_quote_at` be updated continuously rather than per
    fetch, tightening the staleness budgets.
  5. **`defensive_only` integration with drawdown breaker** —
    when a fund has a `defensive_only` drawdown event live,
    inject a synthetic rule into the gate so new long orders
    are rejected with a unified surface.
