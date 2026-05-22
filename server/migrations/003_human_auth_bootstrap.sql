-- Migration: 003_human_auth_bootstrap
-- Description: 最小人类账号认证与首个超级管理员引导

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS role VARCHAR(20) NOT NULL DEFAULT 'user'
        CHECK (role IN ('super_admin', 'user'));

UPDATE users
SET role = 'user'
WHERE role IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email_lower_unique
    ON users (LOWER(email))
    WHERE email IS NOT NULL;
