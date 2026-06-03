// Sprint 12.2 — AlertEventRepo backs the alertmanager webhook
// receiver. Stores one row per (fingerprint, status, starts_at)
// transition; relies on the partial unique index in migration 078
// for idempotency so the alertmanager can safely retry the same
// payload.

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type AlertEventRepo struct {
	db *sql.DB
}

func NewAlertEventRepo(db *sql.DB) *AlertEventRepo {
	return &AlertEventRepo{db: db}
}

// AlertEvent is the persisted shape. Labels and Annotations are
// retained as JSON RawMessage so the admin UI can render them
// generically — we explicitly do not project them into typed Go
// fields beyond severity / component, since alertmanager allows any
// label key.
type AlertEvent struct {
	ID                  string          `json:"id"`
	Fingerprint         string          `json:"fingerprint"`
	AlertName           string          `json:"alertName"`
	Severity            string          `json:"severity"`
	Component           string          `json:"component,omitempty"`
	Status              string          `json:"status"`
	Summary             string          `json:"summary,omitempty"`
	Description         string          `json:"description,omitempty"`
	Labels              json.RawMessage `json:"labels"`
	Annotations         json.RawMessage `json:"annotations"`
	StartsAt            time.Time       `json:"startsAt"`
	EndsAt              *time.Time      `json:"endsAt,omitempty"`
	ReceivedAt          time.Time       `json:"receivedAt"`
	AcknowledgedBy      string          `json:"acknowledgedBy,omitempty"`
	AcknowledgedAt      *time.Time      `json:"acknowledgedAt,omitempty"`
	AcknowledgementNote string          `json:"acknowledgementNote,omitempty"`
}

// ErrAlertEventDuplicate is returned by Insert when the
// fingerprint+status+starts_at unique index trips. Callers MUST treat
// this as a no-op success, not a real error — alertmanager retries
// the same payload after a network blip and we want the second call
// to look successful.
var ErrAlertEventDuplicate = errors.New("alert_event_repo: duplicate event")

// Insert persists one transition. Returns the new row id, or
// ErrAlertEventDuplicate when the unique index trips.
func (r *AlertEventRepo) Insert(ctx context.Context, ev *AlertEvent) (string, error) {
	if ev == nil {
		return "", fmt.Errorf("alert_event_repo: nil event")
	}
	if strings.TrimSpace(ev.Fingerprint) == "" {
		return "", fmt.Errorf("alert_event_repo: empty fingerprint")
	}
	if strings.TrimSpace(ev.Status) == "" {
		return "", fmt.Errorf("alert_event_repo: empty status")
	}
	if ev.StartsAt.IsZero() {
		return "", fmt.Errorf("alert_event_repo: zero starts_at")
	}

	labels := ev.Labels
	if len(labels) == 0 {
		labels = json.RawMessage(`{}`)
	}
	annotations := ev.Annotations
	if len(annotations) == 0 {
		annotations = json.RawMessage(`{}`)
	}

	var endsAt any
	if ev.EndsAt != nil && !ev.EndsAt.IsZero() {
		endsAt = *ev.EndsAt
	}
	var component any
	if strings.TrimSpace(ev.Component) != "" {
		component = ev.Component
	}

	var id string
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO admin_alert_events
		   (fingerprint, alertname, severity, component, status,
		    summary, description, labels, annotations,
		    starts_at, ends_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (fingerprint, status, starts_at)
		   WHERE starts_at IS NOT NULL
		   DO NOTHING
		 RETURNING id`,
		ev.Fingerprint,
		ev.AlertName,
		strings.ToLower(strings.TrimSpace(ev.Severity)),
		component,
		strings.ToLower(strings.TrimSpace(ev.Status)),
		ev.Summary,
		ev.Description,
		[]byte(labels),
		[]byte(annotations),
		ev.StartsAt.UTC(),
		endsAt,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Dedup hit. The pre-existing row is the source of truth;
		// the caller should treat this as success.
		return "", ErrAlertEventDuplicate
	}
	if err != nil {
		return "", fmt.Errorf("alert_event_repo: insert: %w", err)
	}
	return id, nil
}

// ListRecent returns recent alert events ordered newest-first.
// `status` filters to firing / resolved / "" (any). `limit` is
// clamped to [1, 500].
func (r *AlertEventRepo) ListRecent(ctx context.Context, status string, limit int) ([]AlertEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	status = strings.ToLower(strings.TrimSpace(status))

	q := `SELECT id, fingerprint, alertname, severity, COALESCE(component, ''), status,
	             COALESCE(summary, ''), COALESCE(description, ''),
	             labels::text, annotations::text,
	             starts_at, ends_at, received_at,
	             COALESCE(acknowledged_by, ''), acknowledged_at,
	             COALESCE(acknowledgement_note, '')
	        FROM admin_alert_events`
	var rows *sql.Rows
	var err error
	if status == "" {
		q += ` ORDER BY received_at DESC LIMIT $1`
		rows, err = r.db.QueryContext(ctx, q, limit)
	} else {
		q += ` WHERE status = $1 ORDER BY received_at DESC LIMIT $2`
		rows, err = r.db.QueryContext(ctx, q, status, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("alert_event_repo: list: %w", err)
	}
	defer rows.Close()
	var out []AlertEvent
	for rows.Next() {
		ev, err := scanAlertEventRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alert_event_repo: iterate: %w", err)
	}
	return out, nil
}

// Acknowledge stamps an admin acknowledgement on an alert event row.
// Returns ErrNotFound when the id doesn't exist. Returns nil for an
// already-acknowledged row — re-acknowledging is a no-op so flaky
// admin clicks don't overwrite the original ack metadata.
func (r *AlertEventRepo) Acknowledge(ctx context.Context, id, userID, note string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("alert_event_repo: empty id")
	}
	if strings.TrimSpace(userID) == "" {
		return fmt.Errorf("alert_event_repo: empty user id")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE admin_alert_events
		    SET acknowledged_by = $2,
		        acknowledged_at = NOW(),
		        acknowledgement_note = $3
		  WHERE id = $1
		    AND acknowledged_by IS NULL`,
		id, userID, note,
	)
	if err != nil {
		return fmt.Errorf("alert_event_repo: acknowledge: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("alert_event_repo: rows affected: %w", err)
	}
	if affected == 0 {
		// Either the row doesn't exist OR was already acked. We
		// disambiguate with a follow-up SELECT only when the caller
		// cares — for the admin UI, "no-op" is fine.
		exists, checkErr := r.exists(ctx, id)
		if checkErr != nil {
			return checkErr
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

func (r *AlertEventRepo) exists(ctx context.Context, id string) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM admin_alert_events WHERE id = $1`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("alert_event_repo: exists: %w", err)
	}
	return true, nil
}

func scanAlertEventRow(rows *sql.Rows) (AlertEvent, error) {
	var (
		ev             AlertEvent
		labels         string
		annotations    string
		endsAt         sql.NullTime
		acknowledgedAt sql.NullTime
	)
	if err := rows.Scan(
		&ev.ID, &ev.Fingerprint, &ev.AlertName, &ev.Severity, &ev.Component, &ev.Status,
		&ev.Summary, &ev.Description,
		&labels, &annotations,
		&ev.StartsAt, &endsAt, &ev.ReceivedAt,
		&ev.AcknowledgedBy, &acknowledgedAt,
		&ev.AcknowledgementNote,
	); err != nil {
		return AlertEvent{}, fmt.Errorf("alert_event_repo: scan: %w", err)
	}
	ev.Labels = json.RawMessage(labels)
	ev.Annotations = json.RawMessage(annotations)
	if endsAt.Valid {
		t := endsAt.Time
		ev.EndsAt = &t
	}
	if acknowledgedAt.Valid {
		t := acknowledgedAt.Time
		ev.AcknowledgedAt = &t
	}
	return ev, nil
}
