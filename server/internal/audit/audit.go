package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
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
//
// Concurrency
//
// Both LogAccess and LogMutation maintain a per-table hash chain
// (P0-8). To guarantee chain correctness under concurrent writers
// the logger holds an in-process sync.Mutex for the duration of the
// fetch-prev-hash + insert-new-row critical section. Because the
// hash chain is per-process for the simulator (single-app deployment)
// this is sufficient. Multi-writer deployments with the same DB can
// upgrade to a Postgres advisory_xact_lock by replacing the mutexes
// with the helper in chainAdvisoryLock — kept commented in chain.go
// for that future migration.
type DBLogger struct {
	db *sql.DB

	// accessChainMu serialises writes to data_access_log so the
	// SELECT MAX(row_hash) → INSERT pair is atomic from the
	// chain's perspective.
	accessChainMu sync.Mutex

	// mutationChainMu does the same for admin_change_log.
	mutationChainMu sync.Mutex
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
	return l.appendAccessChain(ctx, nil, actorUserID, action, resourceType, resourceID, detailsJSON)
}

// LogAccessTx records a data access event within an existing transaction.
func (l *DBLogger) LogAccessTx(ctx context.Context, tx *sql.Tx, actorUserID, action, resourceType, resourceID string, details map[string]any) error {
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		detailsJSON = []byte(`{}`)
	}
	return l.appendAccessChain(ctx, tx, actorUserID, action, resourceType, resourceID, detailsJSON)
}

// appendAccessChain is the shared chain-append path used by both
// LogAccess and LogAccessTx. The runner is either the *sql.DB or
// *sql.Tx depending on which entrypoint the caller used.
//
// Behaviour:
//
//  1. Acquire the per-table mutex so the prev-hash lookup and the
//     INSERT happen atomically with respect to other writers in the
//     process.
//  2. SELECT the row_hash of the most recent chained row (NULL is
//     allowed and means "this is the first chained row after a long
//     pre-chain history").
//  3. Compute prev_hash for the new row (NULL when there is no
//     prior chained row), the details_hash, and the row_hash.
//  4. INSERT the row with all three hash columns populated.
//
// If any chain step errors (DB down, encode error) we fall back to
// inserting WITHOUT the chain columns. Losing a single chain edge
// is preferable to losing the audit row entirely; the verifier
// reports such gaps.
func (l *DBLogger) appendAccessChain(ctx context.Context, tx *sql.Tx, actorUserID, action, resourceType, resourceID string, detailsJSON json.RawMessage) error {
	l.accessChainMu.Lock()
	defer l.accessChainMu.Unlock()

	rowID := uuid.NewString()
	createdAt := time.Now().UTC()

	prev, prevErr := l.fetchLastAccessRowHash(ctx, tx)
	detailsHash, detailsErr := hashCanonicalJSON(detailsJSON)
	rowHash, rowErr := computeAccessRowHash(prev, rowID, actorUserID, action, resourceType, resourceID, detailsHash, createdAt)

	chainOK := prevErr == nil && detailsErr == nil && rowErr == nil
	var (
		prevValue        any
		rowHashValue     any
		detailsHashValue any
	)
	if chainOK {
		if len(prev) > 0 {
			prevValue = prev
		}
		rowHashValue = rowHash
		if detailsHash != nil {
			detailsHashValue = detailsHash
		}
	}

	const sqlInsert = `
		INSERT INTO data_access_log
			(id, actor_user_id, action, resource_type, resource_id, details, created_at, prev_hash, row_hash, details_hash)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

	if tx != nil {
		_, err := tx.ExecContext(ctx, sqlInsert,
			rowID, actorUserID, action, resourceType, resourceID, detailsJSON, createdAt,
			prevValue, rowHashValue, detailsHashValue,
		)
		if err != nil {
			return fmt.Errorf("audit: failed to log access tx: %w", err)
		}
		return nil
	}
	_, err := l.db.ExecContext(ctx, sqlInsert,
		rowID, actorUserID, action, resourceType, resourceID, detailsJSON, createdAt,
		prevValue, rowHashValue, detailsHashValue,
	)
	if err != nil {
		return fmt.Errorf("audit: failed to log access: %w", err)
	}
	return nil
}

// fetchLastAccessRowHash returns the row_hash of the most recent
// chained data_access_log row. Returns nil (not an error) when no
// chained row exists yet. Honours the supplied transaction when
// non-nil so a chain append inside a tx sees the tx's own writes.
func (l *DBLogger) fetchLastAccessRowHash(ctx context.Context, tx *sql.Tx) ([]byte, error) {
	const q = `
		SELECT row_hash
		FROM data_access_log
		WHERE row_hash IS NOT NULL
		ORDER BY created_at DESC, id DESC
		LIMIT 1`
	var (
		hash sql.RawBytes
		row  *sql.Row
	)
	if tx != nil {
		row = tx.QueryRowContext(ctx, q)
	} else {
		row = l.db.QueryRowContext(ctx, q)
	}
	if err := row.Scan(&hash); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("audit: lookup last access row hash: %w", err)
	}
	if len(hash) == 0 {
		return nil, nil
	}
	return append([]byte(nil), hash...), nil
}

// LogMutation records a before/after admin mutation diff outside any
// caller-supplied transaction.
func (l *DBLogger) LogMutation(ctx context.Context, ev MutationEvent) error {
	beforeJSON, afterJSON, metaJSON, reqID, err := marshalMutation(ev)
	if err != nil {
		return err
	}
	return l.appendMutationChain(ctx, nil, ev, beforeJSON, afterJSON, metaJSON, reqID)
}

// LogMutationTx records a before/after admin mutation diff inside the
// caller's transaction so the audit row commits or rolls back atomically
// with the underlying change.
func (l *DBLogger) LogMutationTx(ctx context.Context, tx *sql.Tx, ev MutationEvent) error {
	beforeJSON, afterJSON, metaJSON, reqID, err := marshalMutation(ev)
	if err != nil {
		return err
	}
	return l.appendMutationChain(ctx, tx, ev, beforeJSON, afterJSON, metaJSON, reqID)
}

// appendMutationChain mirrors appendAccessChain for the admin
// change log. It computes 3 content hashes (before, after, metadata)
// in addition to row_hash so a verifier can independently spot
// tamper of any of the JSONB payloads.
func (l *DBLogger) appendMutationChain(ctx context.Context, tx *sql.Tx, ev MutationEvent, beforeJSON, afterJSON, metaJSON []byte, reqID sql.NullString) error {
	l.mutationChainMu.Lock()
	defer l.mutationChainMu.Unlock()

	rowID := uuid.NewString()
	createdAt := time.Now().UTC()

	prev, prevErr := l.fetchLastMutationRowHash(ctx, tx)
	beforeHash, beforeErr := hashCanonicalJSON(beforeJSON)
	afterHash, afterErr := hashCanonicalJSON(afterJSON)
	metaHash, metaErr := hashCanonicalJSON(metaJSON)
	rowHash, rowErr := computeMutationRowHash(prev, rowID, ev.ActorUserID, ev.Action, ev.TargetType, ev.TargetID, reqID.String, beforeHash, afterHash, metaHash, createdAt)

	chainOK := prevErr == nil && beforeErr == nil && afterErr == nil && metaErr == nil && rowErr == nil
	var (
		prevValue       any
		rowHashValue    any
		beforeHashValue any
		afterHashValue  any
		metaHashValue   any
	)
	if chainOK {
		if len(prev) > 0 {
			prevValue = prev
		}
		rowHashValue = rowHash
		if beforeHash != nil {
			beforeHashValue = beforeHash
		}
		if afterHash != nil {
			afterHashValue = afterHash
		}
		if metaHash != nil {
			metaHashValue = metaHash
		}
	}

	const sqlInsert = `
		INSERT INTO admin_change_log
			(id, actor_user_id, action, target_type, target_id, request_id, before_snapshot, after_snapshot, metadata, created_at, prev_hash, row_hash, before_hash, after_hash, metadata_hash)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`

	if tx != nil {
		_, err := tx.ExecContext(ctx, sqlInsert,
			rowID, ev.ActorUserID, ev.Action, ev.TargetType, ev.TargetID, reqID,
			beforeJSON, afterJSON, metaJSON, createdAt,
			prevValue, rowHashValue, beforeHashValue, afterHashValue, metaHashValue,
		)
		if err != nil {
			return fmt.Errorf("audit: failed to log mutation tx: %w", err)
		}
		return nil
	}
	_, err := l.db.ExecContext(ctx, sqlInsert,
		rowID, ev.ActorUserID, ev.Action, ev.TargetType, ev.TargetID, reqID,
		beforeJSON, afterJSON, metaJSON, createdAt,
		prevValue, rowHashValue, beforeHashValue, afterHashValue, metaHashValue,
	)
	if err != nil {
		return fmt.Errorf("audit: failed to log mutation: %w", err)
	}
	return nil
}

func (l *DBLogger) fetchLastMutationRowHash(ctx context.Context, tx *sql.Tx) ([]byte, error) {
	const q = `
		SELECT row_hash
		FROM admin_change_log
		WHERE row_hash IS NOT NULL
		ORDER BY created_at DESC, id DESC
		LIMIT 1`
	var (
		hash sql.RawBytes
		row  *sql.Row
	)
	if tx != nil {
		row = tx.QueryRowContext(ctx, q)
	} else {
		row = l.db.QueryRowContext(ctx, q)
	}
	if err := row.Scan(&hash); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("audit: lookup last mutation row hash: %w", err)
	}
	if len(hash) == 0 {
		return nil, nil
	}
	return append([]byte(nil), hash...), nil
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
