// Verify functions for the audit hash chain (P0-8).
//
// VerifyAccessChain and VerifyMutationChain walk every chained row
// in (created_at, id) order and re-derive each row_hash from the
// stored fields. The function reports either:
//
//   * VerificationOK with the chain length and the bounds of the
//     hashed segment; or
//   * VerificationFailed with the row_id at which the chain broke
//     and a human-readable reason.
//
// Operators expose this via GET /api/audit/chain/verify (admin-only).

package audit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// VerificationStatus is the result of a chain walk.
type VerificationStatus string

const (
	// VerificationOK indicates every chained row in the table was
	// re-derived and matches the stored row_hash, and every prev_hash
	// links back to the previous row.
	VerificationOK VerificationStatus = "ok"

	// VerificationFailed indicates a chain integrity violation —
	// either a content-tamper (recomputed row_hash differs) or a
	// link-tamper (prev_hash does not match the previous row's
	// row_hash). FailedAtRowID identifies the first offending row.
	VerificationFailed VerificationStatus = "failed"

	// VerificationEmpty means the table has no chained rows yet.
	// Callers usually treat this as OK with a warning.
	VerificationEmpty VerificationStatus = "empty"
)

// VerificationReport is the structured output of VerifyAccessChain /
// VerifyMutationChain. It is the JSON body returned by the
// GET /api/audit/chain/verify endpoint.
type VerificationReport struct {
	// Table is the audit table that was verified
	// ("data_access_log" or "admin_change_log").
	Table string `json:"table"`

	// Status is the high-level outcome.
	Status VerificationStatus `json:"status"`

	// HashedRows is the number of rows that participate in the
	// chain (rows with non-NULL row_hash). Pre-chain legacy rows
	// are not counted.
	HashedRows int `json:"hashedRows"`

	// PreChainRows is the count of rows that exist before the
	// chain began (NULL row_hash). Reported for transparency only;
	// they are not verified.
	PreChainRows int `json:"preChainRows"`

	// FirstChainedRowID and LastChainedRowID bracket the chain so
	// operators can correlate with timestamps in the table.
	FirstChainedRowID string `json:"firstChainedRowId,omitempty"`
	LastChainedRowID  string `json:"lastChainedRowId,omitempty"`

	// FailedAtRowID, FailedAtCreatedAt, and FailedReason are
	// populated when Status == VerificationFailed.
	FailedAtRowID     string    `json:"failedAtRowId,omitempty"`
	FailedAtCreatedAt time.Time `json:"failedAtCreatedAt,omitempty"`
	FailedReason      string    `json:"failedReason,omitempty"`
}

// Verifier walks the audit hash chain.
type Verifier struct {
	db *sql.DB
}

// NewVerifier returns a Verifier bound to db.
func NewVerifier(db *sql.DB) *Verifier {
	return &Verifier{db: db}
}

// VerifyAccessChain walks data_access_log in chronological order and
// reports any tamper detected.
func (v *Verifier) VerifyAccessChain(ctx context.Context) (VerificationReport, error) {
	report := VerificationReport{Table: "data_access_log"}

	preCount, err := countRowsWhereHashNull(ctx, v.db, "data_access_log")
	if err != nil {
		return report, err
	}
	report.PreChainRows = preCount

	rows, err := v.db.QueryContext(ctx, `
		SELECT id, actor_user_id, action, resource_type, resource_id, details, created_at, prev_hash, row_hash, details_hash
		FROM data_access_log
		WHERE row_hash IS NOT NULL
		ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return report, fmt.Errorf("audit: query access chain: %w", err)
	}
	defer rows.Close()

	var prevHash []byte
	for rows.Next() {
		var (
			id           string
			actor        sql.NullString
			action       string
			resourceType string
			resourceID   sql.NullString
			details      json.RawMessage
			createdAt    time.Time
			storedPrev   sql.RawBytes
			storedRow    sql.RawBytes
			storedDetail sql.RawBytes
		)
		if err := rows.Scan(&id, &actor, &action, &resourceType, &resourceID, &details, &createdAt, &storedPrev, &storedRow, &storedDetail); err != nil {
			return report, fmt.Errorf("audit: scan access row: %w", err)
		}
		report.HashedRows++
		if report.FirstChainedRowID == "" {
			report.FirstChainedRowID = id
		}
		report.LastChainedRowID = id

		// 1. prev_hash must match the previous chained row's row_hash.
		if !bytesEqualOrBothEmpty(storedPrev, prevHash) {
			report.Status = VerificationFailed
			report.FailedAtRowID = id
			report.FailedAtCreatedAt = createdAt
			report.FailedReason = fmt.Sprintf(
				"prev_hash mismatch: stored=%s, expected=%s",
				hex.EncodeToString(storedPrev),
				hex.EncodeToString(prevHash),
			)
			return report, nil
		}

		// 2. details_hash must match the canonical hash of the
		//    current details JSONB.
		expectedDetailsHash, err := hashCanonicalJSON(details)
		if err != nil {
			return report, fmt.Errorf("audit: hash details for %s: %w", id, err)
		}
		if !bytesEqualOrBothEmpty(storedDetail, expectedDetailsHash) {
			report.Status = VerificationFailed
			report.FailedAtRowID = id
			report.FailedAtCreatedAt = createdAt
			report.FailedReason = fmt.Sprintf(
				"details_hash mismatch: stored=%s, recomputed=%s — details JSONB tampered",
				hex.EncodeToString(storedDetail),
				hex.EncodeToString(expectedDetailsHash),
			)
			return report, nil
		}

		// 3. row_hash must match the canonical encoding.
		expectedRowHash, err := computeAccessRowHash(
			storedPrev,
			id,
			actor.String,
			action,
			resourceType,
			resourceID.String,
			expectedDetailsHash,
			createdAt,
		)
		if err != nil {
			return report, fmt.Errorf("audit: recompute row hash for %s: %w", id, err)
		}
		if !bytes.Equal(storedRow, expectedRowHash) {
			report.Status = VerificationFailed
			report.FailedAtRowID = id
			report.FailedAtCreatedAt = createdAt
			report.FailedReason = fmt.Sprintf(
				"row_hash mismatch: stored=%s, recomputed=%s — metadata tampered",
				hex.EncodeToString(storedRow),
				hex.EncodeToString(expectedRowHash),
			)
			return report, nil
		}

		prevHash = append([]byte(nil), storedRow...)
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("audit: iterate access chain: %w", err)
	}

	if report.HashedRows == 0 {
		report.Status = VerificationEmpty
		return report, nil
	}
	report.Status = VerificationOK
	return report, nil
}

// VerifyMutationChain walks admin_change_log in chronological order
// and reports any tamper.
func (v *Verifier) VerifyMutationChain(ctx context.Context) (VerificationReport, error) {
	report := VerificationReport{Table: "admin_change_log"}

	preCount, err := countRowsWhereHashNull(ctx, v.db, "admin_change_log")
	if err != nil {
		return report, err
	}
	report.PreChainRows = preCount

	rows, err := v.db.QueryContext(ctx, `
		SELECT id, actor_user_id, action, target_type, target_id, request_id,
		       before_snapshot, after_snapshot, metadata, created_at,
		       prev_hash, row_hash, before_hash, after_hash, metadata_hash
		FROM admin_change_log
		WHERE row_hash IS NOT NULL
		ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return report, fmt.Errorf("audit: query mutation chain: %w", err)
	}
	defer rows.Close()

	var prevHash []byte
	for rows.Next() {
		var (
			id              string
			actor           sql.NullString
			action          string
			targetType      string
			targetID        string
			requestID       sql.NullString
			beforeSnap      json.RawMessage
			afterSnap       json.RawMessage
			metadata        json.RawMessage
			createdAt       time.Time
			storedPrev      sql.RawBytes
			storedRow       sql.RawBytes
			storedBeforeH   sql.RawBytes
			storedAfterH    sql.RawBytes
			storedMetadataH sql.RawBytes
		)
		if err := rows.Scan(&id, &actor, &action, &targetType, &targetID, &requestID,
			&beforeSnap, &afterSnap, &metadata, &createdAt,
			&storedPrev, &storedRow, &storedBeforeH, &storedAfterH, &storedMetadataH); err != nil {
			return report, fmt.Errorf("audit: scan mutation row: %w", err)
		}
		report.HashedRows++
		if report.FirstChainedRowID == "" {
			report.FirstChainedRowID = id
		}
		report.LastChainedRowID = id

		if !bytesEqualOrBothEmpty(storedPrev, prevHash) {
			report.Status = VerificationFailed
			report.FailedAtRowID = id
			report.FailedAtCreatedAt = createdAt
			report.FailedReason = fmt.Sprintf(
				"prev_hash mismatch: stored=%s, expected=%s",
				hex.EncodeToString(storedPrev),
				hex.EncodeToString(prevHash),
			)
			return report, nil
		}

		expectedBeforeHash, err := hashCanonicalJSON(beforeSnap)
		if err != nil {
			return report, fmt.Errorf("audit: hash before for %s: %w", id, err)
		}
		expectedAfterHash, err := hashCanonicalJSON(afterSnap)
		if err != nil {
			return report, fmt.Errorf("audit: hash after for %s: %w", id, err)
		}
		expectedMetaHash, err := hashCanonicalJSON(metadata)
		if err != nil {
			return report, fmt.Errorf("audit: hash metadata for %s: %w", id, err)
		}
		if !bytesEqualOrBothEmpty(storedBeforeH, expectedBeforeHash) {
			report.Status = VerificationFailed
			report.FailedAtRowID = id
			report.FailedAtCreatedAt = createdAt
			report.FailedReason = "before_hash mismatch — before_snapshot JSONB tampered"
			return report, nil
		}
		if !bytesEqualOrBothEmpty(storedAfterH, expectedAfterHash) {
			report.Status = VerificationFailed
			report.FailedAtRowID = id
			report.FailedAtCreatedAt = createdAt
			report.FailedReason = "after_hash mismatch — after_snapshot JSONB tampered"
			return report, nil
		}
		if !bytesEqualOrBothEmpty(storedMetadataH, expectedMetaHash) {
			report.Status = VerificationFailed
			report.FailedAtRowID = id
			report.FailedAtCreatedAt = createdAt
			report.FailedReason = "metadata_hash mismatch — metadata JSONB tampered"
			return report, nil
		}

		expectedRowHash, err := computeMutationRowHash(
			storedPrev,
			id,
			actor.String,
			action,
			targetType,
			targetID,
			requestID.String,
			expectedBeforeHash,
			expectedAfterHash,
			expectedMetaHash,
			createdAt,
		)
		if err != nil {
			return report, fmt.Errorf("audit: recompute mutation row hash for %s: %w", id, err)
		}
		if !bytes.Equal(storedRow, expectedRowHash) {
			report.Status = VerificationFailed
			report.FailedAtRowID = id
			report.FailedAtCreatedAt = createdAt
			report.FailedReason = "row_hash mismatch — metadata tampered"
			return report, nil
		}
		prevHash = append([]byte(nil), storedRow...)
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("audit: iterate mutation chain: %w", err)
	}
	if report.HashedRows == 0 {
		report.Status = VerificationEmpty
		return report, nil
	}
	report.Status = VerificationOK
	return report, nil
}

func countRowsWhereHashNull(ctx context.Context, db *sql.DB, table string) (int, error) {
	var n int
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE row_hash IS NULL", table)
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("audit: count pre-chain rows in %s: %w", table, err)
	}
	return n, nil
}

// bytesEqualOrBothEmpty treats a nil/empty slice as equivalent to a
// non-nil zero-length slice. Postgres BYTEA round-trip can produce
// either form depending on driver path; we must accept both as "no
// previous row" / "no payload".
func bytesEqualOrBothEmpty(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return bytes.Equal(a, b)
}
