WITH ranked_workflow_runs AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY fund_id, trading_date
               ORDER BY created_at DESC, id DESC
           ) AS rn
    FROM workflow_runs
)
DELETE FROM workflow_runs
WHERE id IN (
    SELECT id FROM ranked_workflow_runs WHERE rn > 1
);

ALTER TABLE workflow_runs
    ADD CONSTRAINT workflow_runs_fund_id_trading_date_key UNIQUE (fund_id, trading_date);
