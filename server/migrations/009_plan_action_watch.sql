ALTER TABLE plan_actions DROP CONSTRAINT IF EXISTS plan_actions_action_check;

ALTER TABLE plan_actions
    ADD CONSTRAINT plan_actions_action_check
    CHECK (action IN ('buy', 'sell', 'hold', 'reduce', 'add', 'watch'));
