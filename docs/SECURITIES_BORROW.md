# Securities Borrow & Locate Fee (S6.4)

> Created: 2026-06-01 · Owner: trading-platform team
> Companion: `MARKET_IMPACT_MODEL.md`, `MARKET_STATUS_GATE.md`,
> `LOCKUP_GATE.md`

## 1. Why

The simulator used to let any fund open an arbitrary-size
short position with zero ongoing cost. In reality:

1. The broker must **locate** borrowable shares before
   accepting the order (Reg SHO). Hard-to-borrow names may be
   refused outright, and even when borrowable, supply is
   finite ("only 50,000 TSLA shares left today").
2. The broker charges a daily **borrow fee** equal to
   `notional × annualised_rate / day_count`. Easy-to-borrow
   names cost 25-100 bps/yr; hard-to-borrow names can cost
   30%+ annualised. Without this fee the simulator overstates
   short P&L by 1-30% annualised.
3. Some brokers charge a one-time **locate fee** at order
   entry, typically 0-50 bps of notional for HTB names.

We need all three modelled so backtests / paper-trading PMs
don't lock in unrealistic short returns.

## 2. Architecture

```
┌─────────────────────┐      ┌─────────────────────────────┐
│  Admin UI (web)    │ ───▶ │  /api/admin/borrow/*  REST  │
└─────────────────────┘      └──────────────┬──────────────┘
                                            │  CRUD + audit log
                                            ▼
                              ┌─────────────────────────────┐
                              │   security_borrow_rates     │  (PostgreSQL)
                              │   security_locate_events    │
                              │   short_position_borrow_ledger │
                              └──────────────┬──────────────┘
                                            │
                              ┌─────────────────────────────┐
                              │  internal/securitiesborrow  │
                              │   ├── types.go (engines)    │
                              │   ├── repo.go               │
                              │   └── cache.go (hot lookup) │
                              └──────────┬──────────────────┘
                                         │
        ┌────────────────────────────────┴───────────┐
        │                                            │
┌────────────────┐                        ┌─────────────────────┐
│ broker.Simulator│                       │ borrow_accrual_loop │
│  borrowGate     │ ──▶ LocateEngine     │  (leader-gated,     │
│  (pre-trade)    │     +  Cache          │   daily 23:00 UTC)  │
└────────────────┘                        └─────────────────────┘
```

Two integration points, both adapter-mediated:

1. **Pre-trade `broker.BorrowGate`** — the third gate after
   marketstatus and lockup. Only runs when the order would
   open or grow a short position; adapter computes
   `shortQty = order_qty - max(0, position_qty)` and runs the
   `LocateEngine` against the cache row. Verdict shape mirrors
   the other gates.
2. **Daily `borrow_accrual_loop`** — once per day, leader-gated.
   Scans `holding_positions.quantity < 0`, calls
   `AccrualEngine`, books a `borrow_fee` debit to
   `cash_ledger_entries` (with `idempotency_key =
   borrow:{fund}:{key}:{date}` so retries are safe), and
   upserts a row into `short_position_borrow_ledger`.

Why the package stays out of `internal/broker`: keeps broker
free of cash_ledger and DB dependencies; the gate is wired in
at the composition root (`cmd/server/main.go`) — same pattern
as S6.1 / S6.2 / S6.3.

## 3. Data model

### `security_borrow_rates`

| Column                   | Type                  | Note                                                  |
| ------------------------ | --------------------- | ----------------------------------------------------- |
| `instrument_key`         | VARCHAR(64) UNIQUE    | composite of (symbol, exchange)                       |
| `symbol`                 | VARCHAR(64)           | denormalised for UI                                   |
| `market`                 | VARCHAR(16)           | default `US`                                          |
| `asset_class`            | VARCHAR(16)           | default `equity`                                      |
| `borrow_rate_bps_annual` | NUMERIC(10, 2)        | annualised, ≥ 0                                       |
| `locate_fee_bps`         | NUMERIC(10, 2)        | optional one-time, ≥ 0                                |
| `availability`           | VARCHAR(16)           | enum: `easy` / `hard` / `restricted` / `unavailable`  |
| `available_shares`       | BIGINT NULL           | NULL = unbounded (only valid when availability=easy)  |
| `min_locate_qty`         | BIGINT NULL           | reject if requested below                             |
| `max_locate_qty`         | BIGINT NULL           | reject if requested above                             |
| `source`                 | VARCHAR(32)           | enum: manual / broker_quote / agent_lender / …        |
| `last_calibrated_at`     | TIMESTAMPTZ           | when the rate was last updated by source              |
| `note`                   | TEXT NULL             | free-form admin note                                  |
| `updated_by`             | UUID NULL             |                                                       |

### `security_locate_events`

Append-only audit log. One row per pre-trade locate decision
(allow *and* reject). Indexed by `fund_id × created_at desc`
and by `instrument_key × created_at desc`.

| Column            | Type            | Note                                          |
| ----------------- | --------------- | --------------------------------------------- |
| `decision`        | VARCHAR(24)     | enum: allow / reject_* / no_calibration / fail_open |
| `rate_bps_annual` | NUMERIC NULL    | resolved rate at the time of decision         |
| `locate_fee_bps`  | NUMERIC NULL    | resolved fee bps                              |
| `locate_fee_amount` | NUMERIC NULL  | requestedQty × price × locate_fee_bps / 10000 |
| `intended_price`  | NUMERIC NULL    | for forensic reconstruction                   |
| `notional`        | NUMERIC NULL    | computed at decision time                     |
| `reason`          | TEXT NULL       | human-readable explanation                    |
| `client_order_id` | VARCHAR(128)    | links to broker order                         |

### `short_position_borrow_ledger`

| Column                 | Type           | Note                                          |
| ---------------------- | -------------- | --------------------------------------------- |
| (fund_id, instrument_key, accrual_date) | UNIQUE | idempotency guarantee for daily loop |
| `short_qty`            | NUMERIC > 0    | abs(holding qty) on the accrual day          |
| `market_price`         | NUMERIC        | closing price                                |
| `notional`             | NUMERIC        | qty × price                                  |
| `rate_bps_annual`      | NUMERIC        | bps used                                      |
| `day_count_basis`      | INT (360/365)  | 365 default (Act/365 Fixed)                   |
| `fee_amount`           | NUMERIC ≥ 0    | notional × rate / day_count                  |
| `cash_ledger_entry_id` | UUID NULL      | cross-ref to the cash_ledger debit            |

### cash_ledger entry types

Two new entries added to the `cash_ledger_entry_type_chk` CHECK:

- `borrow_fee` — daily debit booked by the accrual loop.
- `locate_fee` — one-time debit booked by the borrow gate
  adapter when locate fee > 0 (wired to the cash_ledger in a
  future iteration; v1 surfaces it only via the gate's
  `BorrowVerdict.LocateFee` and the audit row).

Why not "adjustment": dedicated types let analytics
subtotal `SUM(borrow_fee) by fund` cleanly without joining
through metadata.

## 4. Engine semantics

### Locate (pre-trade)

```
if rate == nil:                  NO_CALIBRATION  (adapter chooses allow-warn vs reject)
if rate.availability == unavailable: REJECT_UNAVAIL
if requested_qty <= 0:           REJECT_INSUFF (defensive)
if requested_qty < min:          REJECT_BELOW_MIN
if requested_qty > max:          REJECT_ABOVE_MAX
if requested_qty > available:    REJECT_INSUFF
else:                            ALLOW
                                    locate_fee = notional × locate_fee_bps / 10000
```

### Accrual (daily)

```
if short_qty <= 0:        return 0  ("no short position")
if market_price <= 0:     return 0  ("missing closing price")
if rate_bps <= 0:         return 0  ("zero borrow cost")
notional   = short_qty * market_price
daily_rate = rate_bps_annual / day_count_basis / 10000
fee        = notional * daily_rate
```

Day count = 365 (Act/365 Fixed). The constant is configurable
on the loop's `borrowAccrualConfig` for venues that use 360.

## 5. Simulator integration

`broker.BorrowGate`:

```go
type BorrowGate interface {
    CheckOrder(ctx context.Context, probe BorrowProbe) BorrowVerdict
}
```

Gate priority in `PlaceOrder` (first reject wins):

1. **MarketStatusGate** (halt / price-limit / calendar)
2. **LockupGate** (lock-up on existing longs)
3. **BorrowGate** (locate for short opens)

Buys short-circuit immediately. For sells, the adapter:

1. Reads `holding_positions.quantity` for (fund, instrument).
2. Computes `shortQtyNeeded = order_qty - max(0, position_qty)`.
3. If 0 → no borrow needed, allow.
4. Otherwise cache-lookup rate → engine → log event → verdict.

### Fail-open

Same posture as the other Sprint-6 gates. Every fail-open path
bumps a Prometheus metric; the operator dashboard alerts on
`BorrowFailOpenHigh` when the ratio exceeds 1%.

### "No calibration" toggle

The adapter has a `rejectOnNoCalibration` boolean. Default
**false** (allow with warning + log `no_calibration` event)
so a partially-loaded calibration table doesn't break trading.
Production deployments with complete calibration coverage can
flip to **true** for strict enforcement.

## 6. Daily accrual loop

Lifecycle:

```
every Interval (default 1h):
    if not leader: skip
    if now.Hour < HourOfDay (default 23 UTC): skip
    if lastRun is today: skip
    AccrueOnce(now):
        SELECT fund_id, instrument_key, symbol, quantity, current_price
            FROM holding_positions WHERE quantity < 0
        for each:
            rate := cache.Lookup(instrument_key)
            res := engine.Evaluate({short_qty, price, rate, day_count})
            if res.fee > 0:
                cash_ledger.Append({type=borrow_fee, amount=-fee,
                                     idempotency_key=borrow:{fund}:{key}:{date}})
                borrow_ledger.UpsertLedgerEntry({…})
```

Idempotency: the cash_ledger's `idempotency_key` is `borrow:{fund}:{instrument}:{date}` —
a retry on the same day is a no-op. The borrow_ledger has a
`UNIQUE (fund_id, instrument_key, accrual_date)` constraint —
upsert preserves prior values.

Single-fund failure does not stop the loop; each iteration
logs and continues to the next row.

## 7. Admin REST

| Method | Path                                  | Purpose                                       |
| ------ | ------------------------------------- | --------------------------------------------- |
| GET    | `/api/admin/borrow/rates`             | list (filters: market / asset_class / availability) |
| GET    | `/api/admin/borrow/rates/{key}`       | one row                                       |
| POST   | `/api/admin/borrow/rates`             | upsert (cache.ApplyChange runs sync)          |
| DELETE | `/api/admin/borrow/rates/{key}`       | hard delete + cache eviction                  |
| POST   | `/api/admin/borrow/locate/preview`    | dry-run a locate decision                     |
| GET    | `/api/admin/borrow/locate/events`     | audit log (filters: fund / instrument / decision) |
| GET    | `/api/admin/borrow/ledger`            | daily fee ledger                              |
| GET    | `/api/admin/borrow/cache`             | cache stats                                   |
| POST   | `/api/admin/borrow/cache/refresh`     | force reload from DB                          |

Auth: `requireAdmin` on all routes. Writes audit-log via
`audit.MutationEvent` and bump `fundai_borrow_events_total{event="admin_*"}`.

## 8. Web UI

`AdminBorrowSection.tsx` provides one section with four panels:

- **Rates table + upsert form** — list / create / update / delete
  calibrations.
- **Locate preview** — input instrument, qty, price; see the
  gate decision, fee, notional.
- **Cache panel** — size, last-refresh, force-refresh button.
- **Locate audit log** — filterable by fund / decision.
- **Borrow-fee ledger** — daily accrual rows.

i18n: zh-CN + en-US in `@fundai/api-client/i18n`'s
`messages.borrow` namespace.

## 9. Metrics

`fundai_borrow_events_total{event="..."}`:

| event                          | meaning                                        |
| ------------------------------ | ---------------------------------------------- |
| `check_allow_non_sell`         | buy short-circuit (no DB)                      |
| `check_allow_no_borrow`        | sell purely closing a long                     |
| `check_allow_short`            | short open allowed                             |
| `check_reject_unavailable`     | rate.availability=unavailable                  |
| `check_reject_insufficient`    | requested > available                          |
| `check_reject_below_min`       | requested < min_locate_qty                     |
| `check_reject_above_max`       | requested > max_locate_qty                     |
| `no_calibration`               | no rate row → fail-open allow (or reject)      |
| `position_lookup_failed`       | DB err reading position → fail-open            |
| `audit_log_failed`             | DB err writing locate event                    |
| `accrual_booked`               | daily loop booked a fee                        |
| `accrual_skipped_*`            | engine returned 0 fee (no short / no price / zero rate) |
| `book_failed`                  | cash_ledger or borrow_ledger upsert error      |
| `scan_failed` / `scan_row_failed` | loop's holding scan errors                  |
| `run_completed`                | loop iteration finished                        |
| `admin_upsert_rate`            | admin wrote a rate row                         |
| `admin_delete_rate`            | admin deleted a rate row                       |
| `admin_cache_refresh`          | admin pressed Force Refresh                    |

Recommended alerts live in `docs/PROMETHEUS_QUERIES.md` § 19.

## 10. Tests

| Layer            | File                                              | Coverage                                      |
| ---------------- | ------------------------------------------------- | --------------------------------------------- |
| Locate engine    | `internal/securitiesborrow/types_test.go`         | nil rate, unavailable, insufficient, min/max bounds, easy allow no fee, HTB allow with fee, validation |
| Accrual engine   | (same file)                                       | no-short / no-price / zero-rate skips, 360 vs 365 day count, default 365, TZ neutrality |
| Repo             | `internal/securitiesborrow/repo_test.go`          | rate get / upsert validation / delete; locate-event log + filter; ledger upsert + validation |
| Cache            | `internal/securitiesborrow/cache_test.go`         | hit / miss / case-insensitive / copy-on-read / ApplyChange / concurrency / nil-safe / start-stop idempotency |
| Broker gate      | `internal/broker/simulator_borrow_test.go`        | rejected blocks short; default reason; warnings attach; no-gate; gate priority status > lockup > borrow |
| cmd/server adapter | `cmd/server/borrow_gate_test.go`                | buy short-circuit; long-close skip; partial short; HTB warnings + fee; unavailable reject; no-calibration fail-open + fail-closed; position lookup err fail-open; no-position-row; nil-safe |
| Accrual loop     | `cmd/server/borrow_accrual_loop_test.go`          | books fee + ledger; no-short no-op; no-calibration skip; scan err; not-leader; same-day idempotency |
| Admin REST       | `cmd/server/admin_borrow_test.go`                 | auth (401/403); list / upsert + cache-sync / delete + cache-evict / locate preview / cache stats / list locate events |

## 11. Future work

- **Wire `locate_fee` to cash_ledger** — currently the gate's
  `BorrowVerdict.LocateFee` is surfaced via warnings only; the
  cash debit is deferred to a future PR that updates the
  simulator's post-fill cash-flow pipeline.
- **Recall handling** — admin endpoint to mark a position as
  "recalled" → emit a forced buy-in plan via the workflow
  scheduler. Real broker FIX integration replaces this.
- **Per-fund borrow caps** — risk-budget table limiting total
  borrow notional per fund (today only per-instrument
  available_shares is enforced).
- **Real-time rate feed** — periodically pull
  `agent_lender` / `broker_quote` and update calibration rows
  via the same Upsert path (cache.ApplyChange handles it).
- **Variable day-count by venue** — Asian futures, repo desks,
  and some FX markets use 360; future work to load
  `day_count_basis` from a per-market config table.
