package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ComplianceRepo persists the two compliance-side tables added
// in migration 104:
//
//   - compliance_acknowledgments: who clicked through which
//     disclosure text and when. Read by the advisor /
//     papertrading handlers as a soft gate before serving
//     content; written by the dedicated /api/compliance/ack
//     endpoint.
//
//   - compliance_phrase_violations: append-only audit trail of
//     LLM outputs the post-processor had to redact. Lets the
//     compliance team sample LLM behaviour over time and
//     justify continued use of Publisher mode to the legal
//     review board.
//
// Both tables are deliberately schema-light so the legal team
// can hand-query them via psql; we don't add ORM abstractions
// that would hide the SQL.

type ComplianceRepo struct {
	db *sql.DB
}

func NewComplianceRepo(db *sql.DB) *ComplianceRepo { return &ComplianceRepo{db: db} }

func (r *ComplianceRepo) DB() *sql.DB { return r.db }

// AckRow mirrors one row of compliance_acknowledgments. Only
// fields the API surfaces today are exposed; we'll add
// columns as the legal team asks for them rather than
// pre-populating speculative fields.
type AckRow struct {
	ID               string
	UserID           string
	Surface          string
	Mode             string
	Locale           string
	AcknowledgedAt   time.Time
	AcknowledgedText string
	IPCountry        sql.NullString
	IPSubRegion      sql.NullString
	UserAgent        sql.NullString
	TextVersion      int
}

// UpsertAcknowledgment writes the user's click-through. Re-clicks
// from the same user for the same (surface × mode × text_version)
// silently no-op via ON CONFLICT — that prevents a noisy
// "duplicate key" error if the user double-clicks the modal.
//
// Returns the ID of the row (existing or new). The caller can
// log it as an audit reference.
func (r *ComplianceRepo) UpsertAcknowledgment(ctx context.Context, row AckRow) (string, error) {
	if r == nil || r.db == nil {
		return "", errors.New("compliance repo not configured")
	}
	if row.UserID == "" || row.Surface == "" || row.Mode == "" {
		return "", errors.New("compliance ack requires user_id, surface, mode")
	}
	if row.TextVersion <= 0 {
		row.TextVersion = 1
	}
	if row.Locale == "" {
		row.Locale = "en"
	}
	const q = `
INSERT INTO compliance_acknowledgments
  (user_id, surface, mode, locale, acknowledged_text, ip_country, ip_subregion, user_agent, text_version)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (user_id, surface, mode, text_version) DO UPDATE
  SET acknowledged_at = NOW(),
      acknowledged_text = EXCLUDED.acknowledged_text,
      ip_country = COALESCE(EXCLUDED.ip_country, compliance_acknowledgments.ip_country),
      ip_subregion = COALESCE(EXCLUDED.ip_subregion, compliance_acknowledgments.ip_subregion),
      user_agent = COALESCE(EXCLUDED.user_agent, compliance_acknowledgments.user_agent)
RETURNING id;`
	var id string
	err := r.db.QueryRowContext(ctx, q,
		row.UserID,
		strings.ToLower(row.Surface),
		strings.ToLower(row.Mode),
		strings.ToLower(row.Locale),
		row.AcknowledgedText,
		row.IPCountry,
		row.IPSubRegion,
		row.UserAgent,
		row.TextVersion,
	).Scan(&id)
	return id, err
}

// HasAcknowledged is the gate-check used by handlers. Returns
// true if the user has at least one ack row for (surface, mode)
// at >= the required textVersion. The "global" surface acts as
// a catch-all — a user who acknowledged the global disclosure
// is considered to have implicitly accepted every per-surface
// disclosure too, which simplifies the modal UX without
// weakening the audit trail (the global ack text is the
// strictest).
func (r *ComplianceRepo) HasAcknowledged(ctx context.Context, userID, surface, mode string, textVersion int) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("compliance repo not configured")
	}
	if userID == "" {
		return false, nil
	}
	if textVersion <= 0 {
		textVersion = 1
	}
	const q = `
SELECT 1
  FROM compliance_acknowledgments
 WHERE user_id = $1
   AND mode = $2
   AND text_version >= $3
   AND surface IN ($4, 'global')
 LIMIT 1;`
	var x int
	err := r.db.QueryRowContext(ctx, q,
		userID,
		strings.ToLower(mode),
		textVersion,
		strings.ToLower(surface),
	).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListAcknowledgments returns every ack row for a user, sorted
// newest-first. Used by the user profile / data export endpoint
// (GDPR-style "what compliance disclosures did I sign?").
func (r *ComplianceRepo) ListAcknowledgments(ctx context.Context, userID string) ([]AckRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("compliance repo not configured")
	}
	const q = `
SELECT id, user_id, surface, mode, locale, acknowledged_at, acknowledged_text,
       ip_country, ip_subregion, user_agent, text_version
  FROM compliance_acknowledgments
 WHERE user_id = $1
 ORDER BY acknowledged_at DESC;`
	rows, err := r.db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AckRow
	for rows.Next() {
		var row AckRow
		if err := rows.Scan(
			&row.ID, &row.UserID, &row.Surface, &row.Mode, &row.Locale,
			&row.AcknowledgedAt, &row.AcknowledgedText,
			&row.IPCountry, &row.IPSubRegion, &row.UserAgent, &row.TextVersion,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// PhraseViolationRow mirrors one row of
// compliance_phrase_violations.
type PhraseViolationRow struct {
	ID             string
	UserID         sql.NullString
	Surface        string
	Rule           string
	OriginalPhrase string
	Replacement    string
	FullRedacted   sql.NullString
	FlaggedAt      time.Time
	SourceEntity   sql.NullString
	SourceID       sql.NullString
}

// InsertPhraseViolations appends one row per violation. Pass an
// empty slice — it's a no-op (and returns nil) so the caller
// doesn't need to guard.
//
// Single-statement INSERT with a positional bind list (one
// $N per column × N rows). PostgreSQL caps that at ~65K but
// we're well below: typical batch is 1-3 violations per LLM
// response.
func (r *ComplianceRepo) InsertPhraseViolations(ctx context.Context, rows []PhraseViolationRow) error {
	if r == nil || r.db == nil {
		return errors.New("compliance repo not configured")
	}
	if len(rows) == 0 {
		return nil
	}
	const cols = 8 // user_id, surface, rule, original_phrase, replacement, full_redacted, source_entity, source_id
	var b strings.Builder
	args := make([]any, 0, len(rows)*cols)
	b.WriteString(`INSERT INTO compliance_phrase_violations
  (user_id, surface, rule, original_phrase, replacement, full_redacted, source_entity, source_id)
VALUES `)
	for i, row := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		base := i*cols + 1
		// Use Sprintf-via-builder to keep the placeholder
		// indices aligned without pulling in fmt.
		writePlaceholders(&b, base, cols)
		args = append(args,
			row.UserID,
			strings.ToLower(row.Surface),
			row.Rule,
			row.OriginalPhrase,
			row.Replacement,
			row.FullRedacted,
			row.SourceEntity,
			row.SourceID,
		)
	}
	b.WriteString(";")
	_, err := r.db.ExecContext(ctx, b.String(), args...)
	return err
}

// RecentViolations returns the most recent phrase violations
// for the legal-team dashboard. Limit is clamped to [1,500] so
// a hostile query parameter can't dump the whole table.
func (r *ComplianceRepo) RecentViolations(ctx context.Context, limit int) ([]PhraseViolationRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("compliance repo not configured")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	const q = `
SELECT id, user_id, surface, rule, original_phrase, replacement,
       full_redacted, flagged_at, source_entity, source_id
  FROM compliance_phrase_violations
 ORDER BY flagged_at DESC
 LIMIT $1;`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PhraseViolationRow, 0, limit)
	for rows.Next() {
		var row PhraseViolationRow
		if err := rows.Scan(
			&row.ID, &row.UserID, &row.Surface, &row.Rule,
			&row.OriginalPhrase, &row.Replacement, &row.FullRedacted,
			&row.FlaggedAt, &row.SourceEntity, &row.SourceID,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func writePlaceholders(b *strings.Builder, base, count int) {
	b.WriteString("(")
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("$")
		writeInt(b, base+i)
	}
	b.WriteString(")")
}

func writeInt(b *strings.Builder, n int) {
	if n == 0 {
		b.WriteByte('0')
		return
	}
	if n < 0 {
		b.WriteByte('-')
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	b.Write(buf[i:])
}
