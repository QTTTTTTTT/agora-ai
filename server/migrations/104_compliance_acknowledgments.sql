-- Migration: 104_compliance_acknowledgments
-- Description:
--   Implements the "user must affirmatively acknowledge the
--   Publishers' Exclusion disclosure before using any advisor
--   surface" requirement from the SEC compliance review.
--
--   This single table tracks every (user × surface × mode ×
--   locale) disclosure click. Storing per-surface lets us
--   later show e.g. only the "Paper Trading" disclosure for
--   read-only browse but require the full Advisor disclosure
--   before any consultation is fired.
--
--   The acknowledged_text column stores the EXACT text the
--   user clicked through. SEC Rule 204-2 ("books and records")
--   requires keeping all communications with clients for 5
--   years; under Publisher mode + Marketing Rule, the safe
--   floor is 7 years. The column lets us reconstruct what each
--   user saw historically even if we update the disclosure
--   wording later.
--
-- Companion to:
--   internal/compliance/* (text + scanner + geo helpers)
--   server/cmd/server/advisor_handler.go     (consume_check)
--   web/src/lib/compliance.tsx               (modal + provider)

CREATE TABLE IF NOT EXISTS compliance_acknowledgments (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           UUID NOT NULL,
    surface           TEXT NOT NULL CHECK (surface IN ('advisor','paper_trading','backtest','cn_intraday','global')),
    mode              TEXT NOT NULL CHECK (mode IN ('publisher','ria_registered')),
    locale            TEXT NOT NULL DEFAULT 'en',
    acknowledged_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    acknowledged_text TEXT NOT NULL,
    ip_country        TEXT,
    ip_subregion      TEXT,
    user_agent        TEXT,
    -- A user only needs to acknowledge once per (surface × mode).
    -- If we change the disclosure text materially we bump a
    -- text_version and the unique constraint includes it so
    -- the user is re-prompted.
    text_version      INTEGER NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX IF NOT EXISTS uniq_compliance_ack_user_surface_mode_version
    ON compliance_acknowledgments(user_id, surface, mode, text_version);

CREATE INDEX IF NOT EXISTS idx_compliance_ack_user
    ON compliance_acknowledgments(user_id, acknowledged_at DESC);

-- compliance_phrase_violations:
--   Whenever the forbidden-phrase scanner flags an LLM output,
--   we append one row per violation so the compliance team can
--   sample / audit. The redacted text is what the user
--   actually saw; the original is what the LLM emitted.
--
--   Kept distinct from any general audit_log so a single SQL
--   query like
--     SELECT rule, COUNT(*) FROM compliance_phrase_violations
--      WHERE flagged_at > NOW() - INTERVAL '30 days'
--      GROUP BY rule ORDER BY 2 DESC;
--   gives the legal team a flat dashboard.

CREATE TABLE IF NOT EXISTS compliance_phrase_violations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID,
    surface         TEXT NOT NULL,
    rule            TEXT NOT NULL,
    original_phrase TEXT NOT NULL,
    replacement     TEXT NOT NULL,
    full_redacted   TEXT,
    flagged_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Foreign reference to whichever entity produced the text;
    -- nullable because the scanner can also run on free-form
    -- agent debug logs.
    source_entity   TEXT,
    source_id       TEXT
);

CREATE INDEX IF NOT EXISTS idx_compliance_violations_recent
    ON compliance_phrase_violations(flagged_at DESC);
CREATE INDEX IF NOT EXISTS idx_compliance_violations_rule
    ON compliance_phrase_violations(rule, flagged_at DESC);

-- paper_orders RETENTION marker:
--   Marketing Rule + Books and Records require 7-year archive
--   of any "communication that constitutes an advertisement",
--   which includes the public Paper Trading order ledger. We
--   tag the table with a comment so DBAs and any future
--   "DELETE FROM paper_orders" PR reviewers get a loud signal.

COMMENT ON TABLE paper_orders IS
    'Books-and-records: rows MUST be retained for 7 years from decided_at per Advisers Act Rule 204-2 and Marketing Rule. DO NOT delete or anonymise without a written compliance memo.';
COMMENT ON TABLE paper_portfolios IS
    'Books-and-records: rows MUST be retained for 7 years per Advisers Act Rule 204-2. Status changes go in a separate audit table; never DELETE.';
