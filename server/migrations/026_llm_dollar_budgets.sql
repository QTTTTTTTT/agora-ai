-- F14: LLM dollar/cents budget hard gate.
--
-- Each row defines a cap on cumulative LLM spend (in price_cents — the
-- amount the platform charges the user, not the underlying provider
-- cost) per (user, fund) over a daily and/or monthly rolling window.
--
-- Resolution rules at check time:
--   1. (user_id, fund_id=<actual>) row wins if present
--   2. else (user_id, fund_id IS NULL) row applies as the user-wide cap
--   3. else no cap (subject to platform-level call-count limiter)
--
-- NULL daily/monthly limits = no cap on that window.
--
-- Rationale for cents (not cents+currency): platform settles in USD;
-- existing usage_entries.price_cents is already cents. Storing as
-- NUMERIC(20,4) so fractional-cent prices accumulate without rounding.

CREATE TABLE IF NOT EXISTS llm_budgets (
    user_id              UUID            NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    fund_id              UUID            REFERENCES funds(id) ON DELETE CASCADE,
    daily_limit_cents    NUMERIC(20, 4),
    monthly_limit_cents  NUMERIC(20, 4),
    created_at           TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    CONSTRAINT llm_budgets_at_least_one_limit CHECK (
        daily_limit_cents IS NOT NULL OR monthly_limit_cents IS NOT NULL
    ),
    CONSTRAINT llm_budgets_non_negative CHECK (
        COALESCE(daily_limit_cents, 0) >= 0 AND COALESCE(monthly_limit_cents, 0) >= 0
    )
);

-- Partial unique indexes to express the "fund_id is part of the key but
-- NULL means user-wide" semantics. Postgres treats two NULLs as distinct
-- in a normal unique index, so we split into two indexes.
CREATE UNIQUE INDEX IF NOT EXISTS llm_budgets_user_fund_uniq
    ON llm_budgets (user_id, fund_id)
    WHERE fund_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS llm_budgets_user_wide_uniq
    ON llm_budgets (user_id)
    WHERE fund_id IS NULL;

-- Index for the budget-check hot path: lookup by (user_id, fund_id) or
-- (user_id, NULL).
CREATE INDEX IF NOT EXISTS llm_budgets_user_idx ON llm_budgets (user_id);
