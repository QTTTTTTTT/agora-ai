-- Down: 把 'mixed' 行回滚到 'completed'（更接近原义），然后还原 check 约束。
UPDATE investment_plans SET status = 'completed' WHERE status = 'mixed';

ALTER TABLE investment_plans
    DROP CONSTRAINT IF EXISTS investment_plans_status_check;

ALTER TABLE investment_plans
    ADD CONSTRAINT investment_plans_status_check
    CHECK (status IN (
        'draft', 'risk_review', 'pending_user',
        'approved', 'rejected', 'executing', 'completed'
    ));
