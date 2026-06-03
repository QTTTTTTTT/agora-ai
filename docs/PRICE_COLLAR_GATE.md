# Broker price-collar pre-trade gate (S6.6)

## Goal

Before the simulator (or, eventually, a live broker adapter) sends
a limit order to the matching engine, ask: "is this limit price
plausibly close to a recent reference quote?". If the gap is wide
enough that the order looks like a fat-finger / bad-quote / LLM
hallucination, refuse. Otherwise allow.

This is the fourth and last pre-trade gate, after marketstatus
(halt / calendar / price-limit / stale-quote), lockup (IPO / pre-
IPO / restricted shares), and securities-borrow (locate /
financing). Each upstream gate guards a structural reason the
market would reject the order; the collar is the catch-all that
prevents the *order itself* from being absurd.

## Why now

On 2026-06-02 the simulator filled a buy of 1 share of 301308
(江南奕帆, ChiNext) at **96,226.4188 CNY/share**. The true
intraday mid was around **500 CNY**.

Root cause was upstream: a PM fallback path stamped the notional
buy budget (96,226 CNY) into `PlanAction.Price` with `Quantity=1`
because `quoteForAction` couldn't reach the symbol. The simulator
honoured the resulting 96,226-CNY limit. The PM bug was the
proximate cause; the broker was the accomplice.

We fixed the PM bug in two places (`wiring_adapters.go:14741`,
`:16856`) by downgrading quote-unavailable to `Action="watch"` —
the production-grade default. The collar is the second layer of
defense so the **next** fat-finger / pasted-price / LLM
hallucination can't make a 96,226 fill happen, regardless of
which upstream path produced it.

## Components

### Engine (`internal/pricecollar`)

A pure rule engine with no DB / network deps. Inputs:

  * `Probe` — fund, instrument, side, qty, **intended price**.
    IntendedPrice ≤ 0 ⇒ market order ⇒ engine short-circuits
    to allow (collar only applies to limits / stops).
  * `ReferenceSource.GetReferenceQuote(probe)` — the wiring layer
    plugs in `marketdata.Service` so the engine itself stays
    deps-free.

Outputs:

  * `Allow` — within tolerance.
  * `Warn` (default) — no usable reference quote, or reference
    is too stale. Order still proceeds; warning rides on the
    order so the UI can render a badge.
  * `Reject` — `|intended − reference| / reference > tolerance`.

The no-reference verdict is configurable. Strict deployments can
flip `EngineOptions.NoReferenceDecision = DecisionReject` so the
gate fails closed when marketdata is dark. Default is warn so a
transient outage doesn't turn into a trading halt.

### Tolerances

Asset-class defaults expressed in basis points (10,000 = 100%).
A-share boards specialise on the symbol prefix because the
exchange daily band differs.

| Asset class                  | Threshold | Source                                |
| ---------------------------- | --------: | ------------------------------------- |
| A-share main board (600/000/001/002) | 11% | 10% daily band + 1% buffer      |
| A-share ChiNext/STAR/BSE (300/301/688/689/8) | 21% | 20% daily band + 1% buffer |
| US equity (NYSE / Nasdaq)    | 15%       | LULD T1 5% / T2 10% + fat-finger cap  |
| HK equity                    | 30%       | no daily band; HKEX "extreme deviation" guard |
| Crypto                       | 30%       | 24/7 high vol                          |
| Futures (CN / US)            | 20%       | typical 10% daily band + margin       |
| Bond / OTC                   | 30%       | illiquid; quote refresh can be hours stale |
| (unknown asset class)        | 15%       | safety-net fallback                   |

`EngineOptions.OverrideThresholdBpsByMarket` lets ops tighten or
loosen per market without code changes.

### Broker integration (`internal/broker`)

`PriceCollarGate` interface mirrors the other three gates.
`WithPriceCollarGate(gate)` plugs into `NewSimulator`. The gate
runs **last** so the dramatic reject reasons (halted, calendar,
lockup, borrow) keep precedence in the surfaced `RejectReason`.
Reject errors wrap `ErrPriceCollarRejected` so callers can
type-assert without parsing strings.

### Production wiring (`cmd/server/price_collar_gate.go`)

`newPriceCollarGate(marketData, metrics, logger, opts)` constructs
the engine with a `marketDataReferenceSource` that calls
`marketdata.Service.GetQuote`. Wired into the simulator at the
same site as the other three gates in `main.go` ~line 870.

### Metrics

`fundai_pricecollar_events_total{event="..."}` Prometheus counter
with these labels:

  * `allow` — happy path
  * `reject_price_collar` — limit too far from reference
  * `warn_price_collar_no_reference` — no usable reference (default)
  * `reject_price_collar_no_reference` — strict deployments only
  * `evaluate_failed` — engine internal failure (fail-open)

## Testing

  * `internal/pricecollar/engine_test.go` — 17 cases covering
    every decision path, the 96,226-CNY regression, threshold
    resolution, stale + missing reference, source error fail-open.
  * `internal/broker/simulator_pricecollar_test.go` — wires a
    fake gate into the simulator and proves reject blocks the
    order map, warnings ride on the booked order, market orders
    carry IntendedPrice=0 to the gate, stop orders fall back to
    StopPrice.
  * `cmd/server/translate_buy_quote_unavailable_test.go` — locks
    in the upstream fix (PM downgrades to watch on
    quote-unavailable) so we don't regress back into the buggy
    path the gate is also guarding against.

## Failure modes (and what we picked)

  * **Engine misconfiguration** → constructor returns an error;
    the wiring logs and falls back to nil gate. Broker keeps
    working. (Loud-but-soft.)
  * **Source error mid-trade** → engine emits a warn event and
    returns Allow. Metric ticks `evaluate_failed` /
    `warn_price_collar_no_reference`.
  * **Stale reference** → same as no-reference (warn by default).
  * **Reference price = 0** → treated as missing.
  * **Market order** → engine short-circuits before calling the
    source. The matcher's own quote fn (already gated by
    marketstatus) is the only price authority.

The fail-open posture is deliberate. A 96,226 fill costs cash; a
gate that hard-rejects every order when marketdata stutters costs
the whole trading day. The collar is the second layer of
defense — marketstatus catches the obviously-broken-market case;
the collar catches the bad-order case. Both turning to "warn"
simultaneously is what alerting + the PM downgrade are for.
