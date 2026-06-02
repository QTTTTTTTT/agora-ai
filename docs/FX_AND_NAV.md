# P1-4 FX provider + cross-currency NAV

## Goal

Until P1-4 the platform implicitly treated every position, every cash entry, and
every NAV row as USD. That worked for a US-only fund but broke in three places:

- A fund that holds an HKEX (HKD) or A-share (CNY) position couldn't show NAV
  honestly — the `total_assets` column silently summed CNY-denominated values
  alongside USD as if 1 CNY = 1 USD.
- A deposit booked through P1-2 (`/api/funds/{id}/funding-requests`) accepted a
  `currency` field but the cash-ledger summary collapsed everything as if it
  were USD.
- An LP resident in CNY had no way to ask "show me my NAV in 元".

P1-4 closes those gaps.

## Components

```
        ┌────────────────────────────────────┐
        │     fx_rates table (058)           │
        │  base / quote / rate / rate_at /   │
        │  source / metadata / created_by    │
        └────────────────────────────────────┘
                 ▲                    ▲
                 │                    │
         ┌───────┴──────┐    ┌────────┴───────────┐
         │  daily loop  │    │  manual upsert     │
         │  (Yahoo)     │    │  (admin REST)      │
         └──────────────┘    └────────────────────┘

        ┌────────────────────────────────────┐
        │     funds.base_currency (059)      │
        │     CHECK ∈ {USD, CNY, HKD, EUR,   │
        │             JPY, GBP, SGD}         │
        └────────────────────────────────────┘
                       │
                       ▼
        ┌────────────────────────────────────┐
        │   navfx.Aggregator                 │
        │   Convert(amount, from, to, asOf)  │
        │   triangulates via USD             │
        └────────────────────────────────────┘
                       │
                       ▼
        ┌────────────────────────────────────┐
        │  cash_ledger summary handler       │
        │  emits balance + fx_stale flag     │
        │  in fund.base_currency             │
        └────────────────────────────────────┘
```

## Storage choices

- We store **only USD-anchored pairs**. Every cross-rate (CNY/HKD, EUR/JPY, …)
  is computed on read by triangulating through USD (`USD/HKD ÷ USD/CNY`). This
  halves the write volume and avoids triangular-arbitrage drift between cells.
- `(base, quote, rate_at, source)` is the unique key — re-runs of the daily
  fetch collapse on conflict, and operators can write a `manual` row at the
  same `rate_at` to override a bad `yahoo` row without deleting history.
- Source preference on read: `manual > override > yahoo > eod`. An operator's
  override always wins.

## Provider

Yahoo's `query2.finance.yahoo.com/v7/finance/quote?symbols=USDCNY=X` endpoint
already powers the rest of the platform's market-data pulls, so re-using it
keeps the rate-limit + auth surface minimal. The `Provider` interface lets
us swap to ECB or openexchangerates later without touching any caller.

Failure modes the daily loop tolerates:

- 429 / 5xx / network error — `ErrRateUnavailable`, the loop logs & moves on.
- Zero / negative rate — same treatment; this is Yahoo's "pair hasn't traded
  today" mode, not a bona-fide quote.
- Cross-pair fetched directly — `ErrUnsupportedPair`; the loop never asks
  Yahoo for a non-USD-anchored pair anyway.

## Stale-rate handling

The cash-ledger summary and the NAV aggregator both adopt a **best-effort
with banner** posture:

- If a leg's rate is missing, the offending bucket is counted at face value
  (so AUM doesn't collapse to zero on a transient outage).
- The response carries `fx_stale: true` and the UI renders an "≈ partial FX"
  banner.
- A `convert_stale` Prometheus event fires so ops can correlate spikes with
  the loop's fetch errors.

## Triggering

| Action                          | Path                                           |
| ------------------------------- | ---------------------------------------------- |
| Set fund's reporting currency   | `POST /api/funds/{id}/settings/base-currency`  |
| List FX rate history            | `GET  /api/admin/fx-rates`                     |
| Manual upsert / operator override | `POST /api/admin/fx-rates`                   |

## Audit

Every base-currency change writes a hash-chained `audit.MutationEvent`
(`fund.base_currency.update`). Every manual FX upsert writes one too
(`fx_rate.upsert`). The chain remains tamper-evident under the existing
`/api/admin/audit/verify` endpoint.

## Forward-compat notes

- The aggregator returns `float64`; we'll switch to `math/big.Rat` if a
  trillion-yen fund ever needs sub-1e-7 NAV precision. The interface stays
  the same.
- New currencies are added by inserting a few `fx_rates` rows + bumping the
  `funds_base_currency_chk` CHECK + `fx.SupportedCurrencies` allowlist. No
  migration required for ingest-only currencies.
