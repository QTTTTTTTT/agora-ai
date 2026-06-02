# IPO / Restricted-share Lock-up Gate (S6.3)

> Created: 2026-06-01 · Owner: trading-platform team
> Companion: `MARKET_IMPACT_MODEL.md`, `MARKET_STATUS_GATE.md`

## 1. Why

Today the broker simulator happily fills any sell as long as the
fund holds the position. In practice, shares acquired through:

- **IPO allocation** — typically 90 / 180-day lock-up
- **Pre-IPO private placement** — bilaterally negotiated hold
- **RSU vest** — post-vest restricted period
- **Restricted stock** — Rule 144 / similar
- **Block-sale receive-leg** — contractual hold
- **Employee equity grants** — vest schedule

…cannot be sold for a contractually defined period. Without a
gate the simulator misrepresents the realised cost basis (the
fund "books" a sell that the real broker would have rejected
with `LOCKED_SHARES` / `RESTRICTED_SECURITY`), corrupting P&L
attribution and surveillance signals downstream.

Fix: a small, dedicated pre-trade gate that answers "of the
position you hold, are enough unlocked qty available right now"
before the matching engine fills.

## 2. Architecture

```
                       ┌──────────────────────────┐
   Admin UI (web) ──▶ │  /api/admin/lockups CRUD │ ─┐
                       └──────────────────────────┘  │ writes audit log
                                                     ▼
                                       ┌─────────────────────┐
                                       │  position_lockups   │ (PostgreSQL)
                                       └─────────────────────┘
                                                     │
                                       ┌─────────────────────┐
                                       │  internal/lockup    │
                                       │   ├── types.go      │  pure Engine
                                       │   ├── repo.go       │  CRUD + ListActiveFor
                                       │   └── …             │
                                       └─────────────────────┘
                                                     │
                                       ┌─────────────────────┐
   broker.Simulator.PlaceOrder ─▶     │  cmd/server         │
                  (sell only)         │  lockupGate adapter │ ─▶ broker.LockupVerdict
                                       │  - load active rows │     (Rejected / Warnings)
                                       │  - load position qty│
                                       │  - call Engine      │
                                       └─────────────────────┘
```

Two design points worth flagging:

1. **The engine is pure.** `internal/lockup` has zero
   awareness of the broker, the simulator, or the database.
   Tests run in microseconds; the same engine can later be
   reused server-side in a real-broker integration.
2. **The adapter sits in `cmd/server`, not in `internal/broker`
   or `internal/lockup`.** Same pattern as the market-status
   gate (S6.1) and the market-impact stack (S6.2): wiring lives
   at the composition root so the inner packages stay free of
   each other.

## 3. Data model

`migrations/065_position_lockups.sql`:

| Column            | Type                | Note                                          |
| ----------------- | ------------------- | --------------------------------------------- |
| `id`              | UUID                | PK, default `gen_random_uuid()`               |
| `fund_id`         | UUID                | FK funds(id) ON DELETE CASCADE                |
| `instrument_key`  | VARCHAR(64)         | composite of (symbol, exchange)               |
| `symbol`          | VARCHAR(64)         | denormalised for the admin UI                 |
| `locked_qty`      | NUMERIC(20, 4)      | > 0 (constraint)                              |
| `locked_until`    | TIMESTAMPTZ         | when the lock-up naturally expires            |
| `lockup_reason`   | VARCHAR(32)         | enum: ipo / private_placement / rsu / …       |
| `source_lot_id`   | UUID NULL           | optional FK lot_ledger.id                     |
| `note`            | TEXT NULL           | free-form admin note                          |
| `released_at`     | TIMESTAMPTZ NULL    | early-release timestamp                       |
| `released_reason` | VARCHAR(255) NULL   | required iff `released_at` set                |
| `released_by`     | UUID NULL           | who hit the release button                    |
| `created_by`      | UUID NULL           |                                               |
| `created_at`      | TIMESTAMPTZ NOT NULL DEFAULT NOW() |                                |
| `updated_at`      | TIMESTAMPTZ NOT NULL DEFAULT NOW() |                                |

Two indexes:

- `(fund_id, instrument_key, locked_until) WHERE released_at IS NULL`
  — covers the gate's hot query.
- `(locked_until) WHERE released_at IS NULL`
  — covers the admin "active" filter.

### Why per-row instead of `locked_qty` on `holding_positions`

- Different lots can unlock on different dates; "100 IPO shares
  unlock 2026-12-01, 50 RSU shares unlock 2027-03-15" can only
  be modelled with multiple rows.
- History must survive for compliance — "why was this
  untradeable on date X" needs to answer two years later.
- Operator can early-release one record without losing the
  audit trail of the others.

### Lifecycle

| State    | Definition                                                         |
| -------- | ------------------------------------------------------------------ |
| created  | row inserted by admin                                              |
| active   | `released_at IS NULL AND locked_until > now()` — counted by engine |
| expired  | `released_at IS NULL AND locked_until ≤ now()` — automatically inert |
| released | `released_at IS NOT NULL` — early-released, audit-logged           |

The engine only sums **active** rows. Expired rows stay in the
table forever (auditing); a future archival job can move them
to a cold table without code changes.

## 4. Engine semantics

```
locked_at(t)  = Σ rec.LockedQty where rec.Active(t)
available(t)  = max(0, position_qty - locked_at(t))

decision(side, qty, position, snap, t):
    if side != "sell":           ALLOW_NON_SELL
    if no active records:        ALLOW_NO_LOCKUP
    if qty <= available:         ALLOW
    if position <= 0:            REJECT_NO_POSITION
    else:                        REJECT_LOCKED
```

`NextUnlockAt = min(rec.LockedUntil for rec in active)` — surfaced
in both the reject reason and the (within 7 days) warning so
operators know when the position frees up.

The engine never goes negative: if a config bug makes
`locked_qty > position_qty`, available is capped at 0 rather
than negative, and the decision is `REJECT_LOCKED` (correct
outcome — there's *something* misconfigured but the safe answer
is "block the sell").

## 5. Simulator integration

`broker.LockupGate` is a 2-method interface in
`internal/broker`:

```go
type LockupGate interface {
    CheckOrder(ctx context.Context, probe LockupProbe) LockupVerdict
}
```

The simulator runs both gates in sequence in `PlaceOrder`:

1. **MarketStatusGate** first — halt / suspended / price-limit /
   stale-quote / calendar. A reject here uses
   `ErrMarketStatusRejected` so the operator sees the more
   dramatic reason ("halted: news pending") rather than a
   secondary lock-up reason.
2. **LockupGate** next — only if (1) allowed.

A reject from either gate happens **before** the order is
booked, so the orders map / client-index / positions are
never polluted by a phantom row.

### Fail-open

Same posture as marketstatus: any error inside the adapter
(DB unreachable, panic in the engine) returns `Rejected=false`
plus a warning. Reasoning:

- Trading should not stop because a metadata table hiccupped.
- Real-broker integration will enforce the lock-up server-side
  anyway (the broker rejects the FIX 35=8 with `RestrictedShares`).
- Every fail-open path bumps a metric — operators see the
  blind spot immediately.

## 6. Admin REST

| Method | Path                                  | Purpose                                       |
| ------ | ------------------------------------- | --------------------------------------------- |
| GET    | `/api/admin/lockups`                  | list (filters: fund_id, instrument_key, status) |
| GET    | `/api/admin/lockups/{id}`             | one row                                       |
| POST   | `/api/admin/lockups`                  | create                                        |
| PATCH  | `/api/admin/lockups/{id}`             | edit qty / locked_until / reason / note       |
| DELETE | `/api/admin/lockups/{id}`             | hard delete (typo fix)                        |
| POST   | `/api/admin/lockups/{id}/release`     | early-release with reason (audit-logged)      |

All writes go through `requireAdmin`, audit-log via
`audit.MutationEvent`, and bump
`fundai_lockup_events_total{event="admin_*"}`.

### Update can't touch released rows

The SQL is:

```sql
UPDATE position_lockups
   SET …
 WHERE id = $1
   AND released_at IS NULL
RETURNING …
```

A released record cannot be re-edited (would corrupt the audit
trail); to amend a released row, create a new record — the
old one stays as historical evidence.

## 7. Web UI

`AdminLockupSection.tsx` (admin page):

- Table with columns: fund / symbol / qty / locked-until /
  reason / status / note / actions.
- Filters: fund / instrument / status (`active` / `expired` /
  `released`).
- Three modal dialogs: create, edit (active rows only),
  release (with required reason).
- Hard-delete shows a `window.confirm` warning operator that
  release is the safer, audit-preserving path.
- Approaching-unlock badge: when `NextUnlockAt - now ≤ 7 days`
  the gate attaches a warning and the row gets a yellow
  highlight.

i18n: zh-CN + en-US, sourced from `@fundai/api-client/i18n`'s
`messages.lockup` namespace.

## 8. Metrics

`fundai_lockup_events_total{event="..."}`:

| event                         | meaning                                        |
| ----------------------------- | ---------------------------------------------- |
| `check_allow`                 | sell allowed: order ≤ available                |
| `check_allow_non_sell`        | buy short-circuited (no DB query)              |
| `check_allow_no_lockup`       | sell allowed: no active records                |
| `check_reject_locked`         | sell rejected: order > available               |
| `check_reject_no_position`    | sell rejected: no position to sell from        |
| `check_no_repo`               | gate has no repo wired (test / partial wiring) |
| `check_unknown`               | engine returned an unrecognised decision       |
| `gate_lookup_failed`          | DB error reading active records (fail-open)    |
| `position_lookup_failed`      | DB error reading position qty (fail-open)      |
| `admin_create`                | admin created a record                         |
| `admin_update`                | admin edited a record                          |
| `admin_release`               | admin early-released a record                  |
| `admin_delete`                | admin hard-deleted a record                    |

Recommended Prometheus alerts live in
`docs/PROMETHEUS_QUERIES.md` § 18.

## 9. Tests

| Layer            | File                                              | Coverage                                      |
| ---------------- | ------------------------------------------------- | --------------------------------------------- |
| Engine (pure)    | `internal/lockup/types_test.go`                   | buy short-circuit, no records, multiple records summed, locked > position cap, no position, AsOf default |
| Repo             | `internal/lockup/repo_test.go`                    | ListActiveFor / List filters / Get / Create validation / Update / Release / Delete |
| Broker gate      | `internal/broker/simulator_lockup_test.go`        | reject, default reason, warnings attach, buy bypass, no gate, status-then-lockup priority |
| cmd/server adapter | `cmd/server/lockup_gate_test.go`                | buy short-circuit, no records, reject path, fail-open on lookup err / position err, no-position path, approaching-unlock warning, nil-safe |
| Admin REST       | `cmd/server/admin_lockup_test.go`                 | auth (401/403), list happy + status filter, get not-found, create happy + bad date, update, release happy + missing reason, delete not-found / happy |

## 10. Future work

- **Periodic archival job**: move expired rows older than e.g.
  7 years to a cold storage table.
- **Bulk import**: CSV upload for large IPO allocations
  (currently one POST per row).
- **Per-lot link**: enforce `source_lot_id` referential
  integrity (currently nullable / unconstrained because some
  legacy back-fills have no lot id).
- **Real-broker reconciliation**: fetch the live broker's
  `LOCKED_SHARES` field on each holdings sync, diff against
  our store, alert on mismatches. This is the moment we'd
  switch from fail-open to a stricter posture.
