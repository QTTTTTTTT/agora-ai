-- ============================================================
-- 119. LemonSqueezy subscription status compatibility
--
-- LemonSqueezy can send subscription.status values beyond our
-- original active/expired/cancelled triad. The webhook stores those
-- provider statuses so plan-gating can distinguish non-active paid
-- states without failing the webhook transaction.
-- ============================================================
BEGIN;

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS subscriptions_status_check;
ALTER TABLE subscriptions ADD CONSTRAINT subscriptions_status_check
    CHECK (status IN ('active','expired','cancelled','on_trial','paused','past_due','unpaid'));

COMMIT;
