package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// DataAccessLog represents a single audit record for data access.
type DataAccessLog struct {
	ID           string          `json:"id"`
	ActorUserID  string          `json:"actorUserId"`
	Action       string          `json:"action"`       // e.g., "read", "export", "snapshot"
	ResourceType string          `json:"resourceType"` // e.g., "memory", "agent_config"
	ResourceID   string          `json:"resourceId"`
	Details      json.RawMessage `json:"details"`
	CreatedAt    time.Time       `json:"createdAt"`
}

// MutationEvent represents a single before/after change made by an admin
// actor. It is the F27 building block: every super_admin mutation is
// expected to produce one of these so operators can answer the question
// "who changed X from A to B at time T?".
type MutationEvent struct {
	ActorUserID string         // the human or service principal that performed the change
	Action      string         // e.g. "update_platform_settings", "upsert_llm_budget"
	TargetType  string         // e.g. "platform_settings", "user_llm_budget"
	TargetID    string         // resource id (or "_global_" / "_singleton_" when not applicable)
	RequestID   string         // optional admin_requests.id when the change went through dual-control
	Before      any            // pre-mutation snapshot (nil if creation)
	After       any            // post-mutation snapshot (nil if deletion)
	Metadata    map[string]any // arbitrary context: IP, user agent, reason, etc.
}

// Logger defines the interface for recording audit events.
type Logger interface {
	LogAccess(ctx context.Context, actorUserID, action, resourceType, resourceID string, details map[string]any) error
	LogAccessTx(ctx context.Context, tx *sql.Tx, actorUserID, action, resourceType, resourceID string, details map[string]any) error
	// LogMutation records a before/after diff for an admin mutation. Failures
	// are returned but callers SHOULD generally treat them as non-fatal:
	// losing an audit row is bad but should not roll back a customer-impacting
	// mutation that already committed. The transactional variant LogMutationTx
	// is preferred when atomicity matters.
	LogMutation(ctx context.Context, ev MutationEvent) error
	LogMutationTx(ctx context.Context, tx *sql.Tx, ev MutationEvent) error
}

// DBLogger implements Logger backed by the data_access_log table.
type DBLogger struct {
	db *sql.DB
}

// NewDBLogger creates a new DBLogger.
func NewDBLogger(db *sql.DB) *DBLogger {
	return &DBLogger{db: db}
}

// LogAccess records a data access event.
func (l *DBLogger) LogAccess(ctx context.Context, actorUserID, action, resourceType, resourceID string, details map[string]any) error {
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		detailsJSON = []byte(`{}`)
	}

	_, err = l.db.ExecContext(ctx,
		`INSERT INTO data_access_log (actor_user_id, action, resource_type, resource_id, details)
		 VALUES ($1, $2, $3, $4, $5)`,
		actorUserID, action, resourceType, resourceID, detailsJSON,
	)
	if err != nil {
		return fmt.Errorf("audit: failed to log access: %w", err)
	}
	return nil
}

// LogAccessTx records a data access event within an existing transaction.
func (l *DBLogger) LogAccessTx(ctx context.Context, tx *sql.Tx, actorUserID, action, resourceType, resourceID string, details map[string]any) error {
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		detailsJSON = []byte(`{}`)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO data_access_log (actor_user_id, action, resource_type, resource_id, details)
		 VALUES ($1, $2, $3, $4, $5)`,
		actorUserID, action, resourceType, resourceID, detailsJSON,
	)
	if err != nil {
		return fmt.Errorf("audit: failed to log access tx: %w", err)
	}
	return nil
}

// LogMutation records a before/after admin mutation diff outside any
// caller-supplied transaction.
func (l *DBLogger) LogMutation(ctx context.Context, ev MutationEvent) error {
	beforeJSON, afterJSON, metaJSON, reqID, err := marshalMutation(ev)
	if err != nil {
		return err
	}
	_, err = l.db.ExecContext(ctx,
		`INSERT INTO admin_change_log (actor_user_id, action, target_type, target_id, request_id, before_snapshot, after_snapshot, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		ev.ActorUserID, ev.Action, ev.TargetType, ev.TargetID, reqID, beforeJSON, afterJSON, metaJSON,
	)
	if err != nil {
		return fmt.Errorf("audit: failed to log mutation: %w", err)
	}
	return nil
}

// LogMutationTx records a before/after admin mutation diff inside the
// caller's transaction so the audit row commits or rolls back atomically
// with the underlying change.
func (l *DBLogger) LogMutationTx(ctx context.Context, tx *sql.Tx, ev MutationEvent) error {
	beforeJSON, afterJSON, metaJSON, reqID, err := marshalMutation(ev)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO admin_change_log (actor_user_id, action, target_type, target_id, request_id, before_snapshot, after_snapshot, metadata)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		ev.ActorUserID, ev.Action, ev.TargetType, ev.TargetID, reqID, beforeJSON, afterJSON, metaJSON,
	)
	if err != nil {
		return fmt.Errorf("audit: failed to log mutation tx: %w", err)
	}
	return nil
}

func marshalMutation(ev MutationEvent) (before, after, metadata []byte, requestID sql.NullString, err error) {
	if ev.Before != nil {
		before, err = json.Marshal(ev.Before)
		if err != nil {
			return nil, nil, nil, sql.NullString{}, fmt.Errorf("audit: marshal before snapshot: %w", err)
		}
	}
	if ev.After != nil {
		after, err = json.Marshal(ev.After)
		if err != nil {
			return nil, nil, nil, sql.NullString{}, fmt.Errorf("audit: marshal after snapshot: %w", err)
		}
	}
	meta := ev.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	metadata, err = json.Marshal(meta)
	if err != nil {
		metadata = []byte(`{}`)
	}
	if ev.RequestID != "" {
		requestID = sql.NullString{String: ev.RequestID, Valid: true}
	}
	return before, after, metadata, requestID, nil
}

// NopLogger is a no-op implementation for tests.
type NopLogger struct{}

func (NopLogger) LogAccess(ctx context.Context, actorUserID, action, resourceType, resourceID string, details map[string]any) error {
	return nil
}

func (NopLogger) LogAccessTx(ctx context.Context, tx *sql.Tx, actorUserID, action, resourceType, resourceID string, details map[string]any) error {
	return nil
}

func (NopLogger) LogMutation(ctx context.Context, ev MutationEvent) error {
	return nil
}

func (NopLogger) LogMutationTx(ctx context.Context, tx *sql.Tx, ev MutationEvent) error {
	return nil
}
