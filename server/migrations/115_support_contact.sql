-- 115_support_contact.sql — single-row "Need help? Contact us" config.
--
-- Powers the floating "Get help" button rendered on every page in the
-- SPA. Single row by design (id = TRUE primary key) — there is exactly
-- one platform support contact, edited by super_admins, read by every
-- authenticated user. Mirrors the platform_settings shape.

CREATE TABLE IF NOT EXISTS support_contact (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    discord_url TEXT NOT NULL DEFAULT '',
    qr_image_url TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (id = TRUE)
);

INSERT INTO support_contact (id, enabled, discord_url, qr_image_url, message)
VALUES (TRUE, FALSE, '', '', '')
ON CONFLICT (id) DO NOTHING;
