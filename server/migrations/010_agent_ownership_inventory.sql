ALTER TABLE agents ADD COLUMN IF NOT EXISTS user_id UUID REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS pending_marketplace_snapshot JSONB NOT NULL DEFAULT '{}';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS marketplace_snapshot_imported_at TIMESTAMPTZ;

UPDATE agents AS a
SET user_id = ownership.owner_user_id
FROM (
    SELECT DISTINCT ON (ftm.agent_id)
        ftm.agent_id,
        fc.owner_user_id
    FROM fund_team_members ftm
    JOIN funds f ON f.id = ftm.fund_id
    JOIN fund_companies fc ON fc.id = f.company_id
    WHERE fc.owner_user_id IS NOT NULL
    ORDER BY ftm.agent_id, ftm.joined_at DESC, ftm.id DESC
) AS ownership
WHERE a.id = ownership.agent_id
  AND a.user_id IS NULL;

UPDATE agents AS a
SET user_id = listing_owner.owner_user_id
FROM (
    SELECT DISTINCT ON (aml.source_agent_id)
        aml.source_agent_id AS agent_id,
        aml.seller_user_id AS owner_user_id
    FROM agent_market_listings aml
    WHERE aml.seller_user_id IS NOT NULL
    ORDER BY aml.source_agent_id, aml.created_at DESC, aml.id DESC
) AS listing_owner
WHERE a.id = listing_owner.agent_id
  AND a.user_id IS NULL;

UPDATE agents AS a
SET user_id = delivered_owner.owner_user_id
FROM (
    SELECT DISTINCT ON (amo.delivered_agent_id)
        amo.delivered_agent_id AS agent_id,
        amo.buyer_user_id AS owner_user_id
    FROM agent_market_orders amo
    WHERE amo.buyer_user_id IS NOT NULL
    ORDER BY amo.delivered_agent_id, amo.created_at DESC, amo.id DESC
) AS delivered_owner
WHERE a.id = delivered_owner.agent_id
  AND a.user_id IS NULL;

UPDATE agents AS a
SET user_id = source_owner.owner_user_id
FROM (
    SELECT DISTINCT ON (amo.source_agent_id)
        amo.source_agent_id AS agent_id,
        amo.seller_user_id AS owner_user_id
    FROM agent_market_orders amo
    WHERE amo.seller_user_id IS NOT NULL
    ORDER BY amo.source_agent_id, amo.created_at DESC, amo.id DESC
) AS source_owner
WHERE a.id = source_owner.agent_id
  AND a.user_id IS NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM agents WHERE user_id IS NULL) THEN
        ALTER TABLE agents ALTER COLUMN user_id SET NOT NULL;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_agents_user_id ON agents (user_id, created_at DESC);

ALTER TABLE agent_market_orders ALTER COLUMN buyer_fund_id DROP NOT NULL;
