CREATE TABLE IF NOT EXISTS platform_settings (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE,
    access_mode VARCHAR(20) NOT NULL DEFAULT 'paid_open'
        CHECK (access_mode IN ('paid_open', 'free_open')),
    default_team_interval_minutes INTEGER NOT NULL DEFAULT 15,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (id = TRUE),
    CHECK (default_team_interval_minutes >= 5 AND default_team_interval_minutes <= 1440)
);

INSERT INTO platform_settings (id, access_mode, default_team_interval_minutes)
VALUES (TRUE, 'paid_open', 15)
ON CONFLICT (id) DO NOTHING;
