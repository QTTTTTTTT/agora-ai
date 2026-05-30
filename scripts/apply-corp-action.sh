#!/usr/bin/env bash
# scripts/apply-corp-action.sh
#
# Apply a corporate-action (stock split / 送股 / 转增股本) to a single
# fund's holding of a single instrument. The system has no general
# corporate-action ingestion path yet (see TODO in docs/), so this is
# the operations escape hatch when the upstream quote provider's price
# drops on ex-dividend day and the cost basis is left unadjusted —
# without this, the next P&L refresh shows a phantom loss of
# (1 - 1/ratio) * notional which has nothing to do with trading.
#
# What the script does, in one transaction:
#   1. holding_positions:      quantity *= ratio,  cost_price /= ratio
#   2. holding_positions:      recompute market_value + unrealized_pnl
#                              from the NEW quantity * (current_price,
#                              cost_price)
#   3. position_lots:          quantity_opened, quantity_remaining,
#                              available_qty *= ratio
#                              entry_price, highest_price_seen /= ratio
#                              (lowest_price_seen left untouched —
#                              it's almost always the post-split low
#                              by the time we notice)
#
# What the script intentionally does NOT do:
#   - cash dividend handling — the gross dividend (per-share) is
#     emitted as an INFO line so the operator can manually credit the
#     fund's cash balance if needed. A 10-fen-per-share dividend on a
#     ¥100k position is ~¥30 and rarely worth the round-trip; auto-
#     posting it requires a `cash_balances` model that's still spec.
#   - history rewriting on trade_executions — past trades stay denominated
#     in their original (pre-split) shares and price. This is the
#     accounting-correct convention; the lot-level adjustment carries
#     the post-split state.
#
# Usage (positional args, no flags — short and explicit):
#   scripts/apply-corp-action.sh FUND_ID INSTRUMENT_KEY RATIO [DIVIDEND_PER_SHARE]
#
# Examples:
#   # 688195 (腾景科技) 2025 年度 10送4 + 派 0.164/股 (含税):
#   scripts/apply-corp-action.sh \
#     b8434d1c-f2d1-4463-aac6-4631ec0bdbb9 SSE:688195 1.4 0.164
#
#   # Pure 1股拆 2股 (no dividend):
#   scripts/apply-corp-action.sh fund-uuid SSE:600519 2.0
#
# Exit status: non-zero on any psql error (transaction rolls back).

set -euo pipefail

if [[ $# -lt 3 || $# -gt 4 ]]; then
  cat >&2 <<USAGE
usage: $0 FUND_ID INSTRUMENT_KEY RATIO [DIVIDEND_PER_SHARE]

  FUND_ID                fund UUID (matches holding_positions.fund_id)
  INSTRUMENT_KEY         e.g. "SSE:688195", "NASDAQ:AAPL"
  RATIO                  > 0 number; new_shares = old_shares * ratio
                         (e.g. 1.4 for 10送4, 2.0 for 1拆2)
  DIVIDEND_PER_SHARE     optional, gross cash dividend per old share
                         (informational only — not posted automatically)
USAGE
  exit 2
fi

FUND_ID=$1
INSTRUMENT_KEY=$2
RATIO=$3
DIVIDEND=${4:-0}

CONTAINER=${CONTAINER:-fundai-postgres}
DB_USER=${POSTGRES_USER:-fundai}
DB_NAME=${POSTGRES_DB:-fundai}

# Sanity check the ratio: must be a positive number, not "0", not negative.
# Using bc rather than bash arithmetic so 1.4 etc. are accepted.
if ! awk -v r="$RATIO" 'BEGIN { exit !(r+0 > 0) }'; then
  echo "ERROR: RATIO must be a positive number, got '$RATIO'" >&2
  exit 2
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: docker not on PATH" >&2
  exit 1
fi

echo "==> Applying corporate action to ${FUND_ID} ${INSTRUMENT_KEY}"
echo "    ratio=${RATIO}  dividend_per_share=${DIVIDEND}"
echo

echo "--- before ---"
docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -At -F'|' -c "
SELECT 'holding:'||instrument_key, quantity::text, cost_price::text, current_price::text, market_value::text, unrealized_pnl::text
FROM holding_positions WHERE fund_id='${FUND_ID}' AND instrument_key='${INSTRUMENT_KEY}'
UNION ALL
SELECT 'lot:'||id::text, quantity_opened::text, entry_price::text, last_price::text, '-'::text, highest_price_seen::text
FROM position_lots WHERE fund_id='${FUND_ID}' AND instrument_key='${INSTRUMENT_KEY}';"

echo
echo "--- applying ---"
docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -v ON_ERROR_STOP=1 <<SQL
BEGIN;

UPDATE holding_positions
SET quantity      = round(quantity      * ${RATIO}::numeric, 4),
    available_qty = round(available_qty * ${RATIO}::numeric, 4),
    cost_price    = round(cost_price    / ${RATIO}::numeric, 4),
    updated_at    = NOW()
WHERE fund_id = '${FUND_ID}' AND instrument_key = '${INSTRUMENT_KEY}';

UPDATE holding_positions
SET market_value   = round(quantity * current_price, 4),
    unrealized_pnl = round(quantity * (current_price - cost_price), 4),
    updated_at     = NOW()
WHERE fund_id = '${FUND_ID}' AND instrument_key = '${INSTRUMENT_KEY}';

UPDATE position_lots
SET quantity_opened    = round(quantity_opened    * ${RATIO}::numeric, 4),
    quantity_remaining = round(quantity_remaining * ${RATIO}::numeric, 4),
    entry_price        = round(entry_price        / ${RATIO}::numeric, 4),
    highest_price_seen = round(highest_price_seen / ${RATIO}::numeric, 4),
    updated_at         = NOW()
WHERE fund_id = '${FUND_ID}' AND instrument_key = '${INSTRUMENT_KEY}';

COMMIT;
SQL

echo
echo "--- after ---"
docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -At -F'|' -c "
SELECT 'holding:'||instrument_key, quantity::text, cost_price::text, current_price::text, market_value::text, unrealized_pnl::text
FROM holding_positions WHERE fund_id='${FUND_ID}' AND instrument_key='${INSTRUMENT_KEY}'
UNION ALL
SELECT 'lot:'||id::text, quantity_opened::text, entry_price::text, last_price::text, '-'::text, highest_price_seen::text
FROM position_lots WHERE fund_id='${FUND_ID}' AND instrument_key='${INSTRUMENT_KEY}';"

if awk -v d="$DIVIDEND" 'BEGIN { exit !(d+0 > 0) }'; then
  echo
  echo "==> Cash dividend reminder:"
  echo "    Per-share gross dividend = ${DIVIDEND}"
  echo "    If applicable, manually credit the fund's cash balance:"
  echo "    UPDATE funds SET cash = cash + (old_shares * ${DIVIDEND}) WHERE id='${FUND_ID}';"
  echo "    (look up old_shares = post-split-quantity / ${RATIO} from the 'before' block above)"
fi

echo
echo "==> Done."
