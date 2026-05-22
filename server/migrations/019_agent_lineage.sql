-- Migration 019: agent lineage graph + cycle prevention.
--
-- Today, when marketplace buyout clones a seller's agent into a buyer's
-- account, we create a brand-new agent row with no link back to its
-- origin. Subscribe mode skips cloning altogether but still creates a
-- consumption relationship. Neither path is recorded as a graph, which
-- means the platform cannot detect "matryoshka" listings — A buys from
-- B, slightly tweaks, then B subscribes to A's derived agent — that
-- launder back-into-original IP through nominal modifications.
--
-- This migration introduces:
--
--  1. agent_lineage: edge table. One row per derivation event, regardless
--     of mode (buyout / subscribe / abtest_clone / manual_copy).
--
--  2. agent_lineage_closure: transitive-closure table maintained by app
--     code on every edge insert. Lets us answer "is X a descendant of Y?"
--     in a single index lookup at CreateListing/PurchaseListing time
--     without recursive CTE on the hot path.
--
--  3. funds.parent_fund_id: scaffolding for the (still unimplemented)
--     ABTest FundCloner; not used in this migration's logic but lets the
--     fund schema evolve without further breaking changes.

CREATE TABLE IF NOT EXISTS agent_lineage (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    child_agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    parent_agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    derived_via VARCHAR(20) NOT NULL
        CHECK (derived_via IN ('buyout', 'subscribe', 'abtest_clone', 'manual_copy')),
    source_listing_id UUID REFERENCES agent_market_listings(id) ON DELETE SET NULL,
    source_subscription_id UUID REFERENCES agent_market_subscriptions(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- A child cannot derive from itself (the structural cycle base case).
    CHECK (child_agent_id <> parent_agent_id)
);

-- Each (child, parent) pair is unique: a single derivation event per pair.
CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_lineage_child_parent
    ON agent_lineage (child_agent_id, parent_agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_lineage_parent
    ON agent_lineage (parent_agent_id, created_at DESC);

-- Transitive closure. depth=0 self-pairs are intentionally NOT included;
-- ancestor/descendant queries should explicitly handle "self" if needed.
-- depth >= 1 always.
CREATE TABLE IF NOT EXISTS agent_lineage_closure (
    ancestor_agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    descendant_agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    depth INTEGER NOT NULL CHECK (depth >= 1),
    PRIMARY KEY (ancestor_agent_id, descendant_agent_id)
);

CREATE INDEX IF NOT EXISTS idx_lineage_closure_descendant
    ON agent_lineage_closure (descendant_agent_id);

-- Fund-level parent pointer for future fund-cloning (ABTest etc.).
ALTER TABLE funds
    ADD COLUMN IF NOT EXISTS parent_fund_id UUID REFERENCES funds(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_funds_parent_fund_id
    ON funds (parent_fund_id)
    WHERE parent_fund_id IS NOT NULL;
