-- Migration 021: KYC and Compliance.
--
-- Introduces KYC (Know Your Customer) status tracking for users and a
-- dedicated table for KYC application records.
--
-- kyc_status tracks the overall state of the user's identity verification.
-- kyc_level determines the tier of verification (e.g., limits, live trading eligibility).

ALTER TABLE users 
    ADD COLUMN IF NOT EXISTS kyc_status VARCHAR(20) NOT NULL DEFAULT 'unverified'
        CHECK (kyc_status IN ('unverified', 'pending', 'verified', 'rejected')),
    ADD COLUMN IF NOT EXISTS kyc_level VARCHAR(20) NOT NULL DEFAULT 'tier1_basic'
        CHECK (kyc_level IN ('tier1_basic', 'tier2_advanced', 'tier3_enterprise'));

CREATE TABLE IF NOT EXISTS user_kyc_records (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kyc_level VARCHAR(20) NOT NULL CHECK (kyc_level IN ('tier1_basic', 'tier2_advanced', 'tier3_enterprise')),
    status VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    full_name VARCHAR(255) NOT NULL,
    id_document_type VARCHAR(50) NOT NULL CHECK (id_document_type IN ('id_card', 'passport', 'driver_license')),
    id_document_number VARCHAR(255) NOT NULL,
    document_image_urls JSONB,
    rejection_reason TEXT,
    reviewed_by UUID REFERENCES users(id) ON DELETE SET NULL,
    reviewed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_kyc_records_user_id ON user_kyc_records(user_id);
CREATE INDEX IF NOT EXISTS idx_user_kyc_records_user_created_at ON user_kyc_records(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_user_kyc_records_status_created_at ON user_kyc_records(status, created_at DESC);
