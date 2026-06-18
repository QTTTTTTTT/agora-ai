BEGIN;

DROP TABLE IF EXISTS lemonsqueezy_webhook_events;
DROP TABLE IF EXISTS checkout_intents;
DROP TABLE IF EXISTS plan_lemonsqueezy_variants;

DROP INDEX IF EXISTS idx_subs_ls_subscription_id;

ALTER TABLE subscriptions
    DROP COLUMN IF EXISTS ls_subscription_id,
    DROP COLUMN IF EXISTS ls_customer_id,
    DROP COLUMN IF EXISTS ls_variant_id,
    DROP COLUMN IF EXISTS billing_period,
    DROP COLUMN IF EXISTS locked_price_cents,
    DROP COLUMN IF EXISTS current_period_start,
    DROP COLUMN IF EXISTS current_period_end,
    DROP COLUMN IF EXISTS renews_at,
    DROP COLUMN IF EXISTS cancelled_at,
    DROP COLUMN IF EXISTS ends_at;

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_payment_method_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_payment_method_check
    CHECK (payment_method IN ('wechat','alipay','manual','system'));

COMMIT;
