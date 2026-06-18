BEGIN;

ALTER TABLE subscriptions DROP COLUMN IF EXISTS seat_count;

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_plan_tier_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_plan_tier_check
    CHECK (plan_tier IN ('free','pro','premium','enterprise'));

COMMIT;
