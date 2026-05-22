CREATE TABLE wallet_accounts (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id UUID NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    base_currency VARCHAR(8) NOT NULL DEFAULT 'USD',
    balance_minor BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_wallet_accounts_user_id ON wallet_accounts (user_id);

CREATE TABLE wallet_ledger_entries (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    account_id UUID NOT NULL REFERENCES wallet_accounts(id) ON DELETE CASCADE,
    entry_type VARCHAR(40) NOT NULL,
    amount_minor BIGINT NOT NULL,
    balance_after_minor BIGINT NOT NULL,
    currency VARCHAR(8) NOT NULL DEFAULT 'USD',
    reference_type VARCHAR(40),
    reference_id TEXT,
    created_by_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_wallet_ledger_entries_account_id ON wallet_ledger_entries (account_id, created_at DESC);
CREATE INDEX idx_wallet_ledger_entries_created_by_user_id ON wallet_ledger_entries (created_by_user_id);
