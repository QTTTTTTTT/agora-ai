# Historical one-off migrations

This directory holds **already-executed, one-off SQL scripts** that
were run in production (and any environments that reproduced the
same dirty-data conditions) once and only once. They are kept here
for audit / forensic reference, not as part of the forward
migration sequence.

## Why these are separated from the main `migrations/` directory

`server/cmd/server/main.go::runMigrations` walks the **top level** of
the configured migrations directory (`MIGRATIONS_PATH`, default
`migrations/`) and executes every file that:

  - has a `.sql` extension
  - is not already recorded in the `schema_migrations` tracking table

Subdirectories are skipped (see the `entry.IsDir() ... continue`
check in the runner). Putting one-off scripts here therefore guarantees
the boot path **never** re-applies them, even on a fresh environment
that has no `schema_migrations` row for the file.

That matters because every script in this directory is **business-data
manipulation**, not schema evolution:

  - they edit specific fund / position / trade rows by primary key,
  - they assume the inconsistency they are repairing is actually
    present (the WHERE clauses are NOT idempotent for environments
    that never had the bug),
  - re-running them on a clean DB would either no-op silently
    (best case) or, worse, partially apply because the WHERE
    matched something the script was not designed to touch.

## Inventory

| File                                                      | Date       | Incident                                                     |
| --------------------------------------------------------- | ---------- | ------------------------------------------------------------ |
| `manual_reversal_erroneous_fills_20260603.sql`            | 2026-06-03 | Two erroneous A-share fills on fund `aa11…` reversed (PM quote-unavailable fallback stamped notional into Price with Quantity=1). Phase 1 of incident response. |
| `manual_s12_lotsize_cleanup_20260603.sql`                 | 2026-06-03 | Lot-size + corp-action residual dirty-data on fund `b8434d1c…` cleaned up after S12 audit. Phase 1 of incident response. |
| `manual_full_dirty_data_sweep_20260603.sql`               | 2026-06-03 | Phase 2 of the 2026-06-03 incident — full-table sweep after the S12 lot-size gate and corp-action whole-share fix landed in code. |
| `manual_s12_pmpath_lotsize_cleanup_20260604.sql`          | 2026-06-04 | Follow-up cleanup of OCS fund STAR-Market positions left behind by the PM-direct-fill bypass of `broker.LotSizeGate`. Code-side guard added in `wiring_adapters.go::runtimeTradingEngine.pmPathLotSizeGuard`. |

Each script's own header has the full context: root cause, affected
rows, corrective actions, and the cash-ledger entries created.

## When to add a new script here

You're writing a one-off SQL script for an incident if **all** of the
following are true:

1. You're repairing an inconsistency that exists in **specific
   already-affected production environments**, not a schema change
   every environment needs.
2. The WHERE clauses are tied to specific PKs / fund IDs / trade IDs
   from the incident.
3. You want a permanent record of the fix in version control for
   audit, but you do **not** want fresh dev / staging / DR rebuild
   environments to execute it.

If those hold, add the script here following the
`manual_<short_summary>_<YYYYMMDD>.sql` naming convention and update
the inventory table above.

## How operators run scripts from this directory

These are **never** run automatically. The recovering operator
applies them by hand against a specific environment using `psql`,
inside a transaction the operator owns:

```bash
docker compose exec -T postgres psql -U fundai -d fundai \
    -f /tmp/manual_reversal_erroneous_fills_20260603.sql
```

For multi-script incidents, start a single transaction in `psql`,
`\i` each script in order, verify expected row counts with
`SELECT * FROM cash_ledger WHERE …`, then `COMMIT` (or `ROLLBACK`
if anything looks wrong).

## When to remove a script

Never. These are part of the audit trail for what touched
production data and when. Even after the affected environments are
decommissioned, the file should stay here so the original incident
can be reconstructed from version control alone.
