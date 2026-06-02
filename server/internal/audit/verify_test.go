package audit

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// helper: build a 4-row chain and return the last row_hash so the
// next test can extend it if needed.
type accessFixtureRow struct {
	id, actor, action, rt, rid string
	details                    json.RawMessage
	createdAt                  time.Time
}

func buildAccessChainRows(t *testing.T) []accessFixtureRow {
	base := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	return []accessFixtureRow{
		{"row-1", "u1", "read", "memory", "m-1", json.RawMessage(`{"k":1}`), base},
		{"row-2", "u2", "read", "memory", "m-2", json.RawMessage(`{"k":2}`), base.Add(time.Second)},
		{"row-3", "u1", "export", "audit", "x", json.RawMessage(`{"reason":"q1"}`), base.Add(2 * time.Second)},
	}
}

// ---------------------------------------------------------------------------
// VerifyAccessChain
// ---------------------------------------------------------------------------

func TestVerifyAccessChain_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT COUNT").
		WithArgs().
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, actor_user_id`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "actor_user_id", "action", "resource_type", "resource_id",
			"details", "created_at", "prev_hash", "row_hash", "details_hash",
		}))

	v := NewVerifier(db)
	rep, err := v.VerifyAccessChain(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Status != VerificationEmpty {
		t.Errorf("status = %s, want empty", rep.Status)
	}
	if rep.HashedRows != 0 {
		t.Errorf("hashedRows = %d, want 0", rep.HashedRows)
	}
}

func TestVerifyAccessChain_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	rows := buildAccessChainRows(t)

	// Pre-derive the expected chain so we can feed the rows back
	// to the verifier in their canonical, unmodified form.
	var prev []byte
	resultRows := sqlmock.NewRows([]string{
		"id", "actor_user_id", "action", "resource_type", "resource_id",
		"details", "created_at", "prev_hash", "row_hash", "details_hash",
	})
	for _, r := range rows {
		dh, err := hashCanonicalJSON(r.details)
		if err != nil {
			t.Fatalf("details hash: %v", err)
		}
		rh, err := computeAccessRowHash(prev, r.id, r.actor, r.action, r.rt, r.rid, dh, r.createdAt)
		if err != nil {
			t.Fatalf("row hash: %v", err)
		}
		var prevForRow any
		if prev == nil {
			prevForRow = nil
		} else {
			prevForRow = prev
		}
		resultRows.AddRow(r.id, r.actor, r.action, r.rt, r.rid, []byte(r.details), r.createdAt, prevForRow, rh, dh)
		prev = rh
	}

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, actor_user_id`)).WillReturnRows(resultRows)

	v := NewVerifier(db)
	rep, err := v.VerifyAccessChain(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Status != VerificationOK {
		t.Errorf("status = %s, want ok (reason=%s)", rep.Status, rep.FailedReason)
	}
	if rep.HashedRows != len(rows) {
		t.Errorf("hashedRows = %d, want %d", rep.HashedRows, len(rows))
	}
	if rep.PreChainRows != 2 {
		t.Errorf("preChainRows = %d, want 2", rep.PreChainRows)
	}
	if rep.FirstChainedRowID != rows[0].id {
		t.Errorf("firstChainedRowId = %q, want %q", rep.FirstChainedRowID, rows[0].id)
	}
	if rep.LastChainedRowID != rows[len(rows)-1].id {
		t.Errorf("lastChainedRowId = %q, want %q", rep.LastChainedRowID, rows[len(rows)-1].id)
	}
}

func TestVerifyAccessChain_TamperedDetails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	rows := buildAccessChainRows(t)
	var prev []byte
	resultRows := sqlmock.NewRows([]string{
		"id", "actor_user_id", "action", "resource_type", "resource_id",
		"details", "created_at", "prev_hash", "row_hash", "details_hash",
	})
	for i, r := range rows {
		dh, _ := hashCanonicalJSON(r.details)
		rh, _ := computeAccessRowHash(prev, r.id, r.actor, r.action, r.rt, r.rid, dh, r.createdAt)
		// Simulate someone editing the JSONB on the second row
		// without rotating the chain — verifier must catch it.
		if i == 1 {
			r.details = json.RawMessage(`{"k":"hacked"}`)
		}
		var prevForRow any
		if prev == nil {
			prevForRow = nil
		} else {
			prevForRow = prev
		}
		resultRows.AddRow(r.id, r.actor, r.action, r.rt, r.rid, []byte(r.details), r.createdAt, prevForRow, rh, dh)
		prev = rh
	}

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, actor_user_id`)).WillReturnRows(resultRows)

	v := NewVerifier(db)
	rep, err := v.VerifyAccessChain(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Status != VerificationFailed {
		t.Errorf("status = %s, want failed", rep.Status)
	}
	if rep.FailedAtRowID != "row-2" {
		t.Errorf("failedAtRowId = %q, want row-2", rep.FailedAtRowID)
	}
	if rep.FailedReason == "" {
		t.Errorf("failedReason should be populated")
	}
}

func TestVerifyAccessChain_TamperedRowHash(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	rows := buildAccessChainRows(t)
	var prev []byte
	resultRows := sqlmock.NewRows([]string{
		"id", "actor_user_id", "action", "resource_type", "resource_id",
		"details", "created_at", "prev_hash", "row_hash", "details_hash",
	})
	for i, r := range rows {
		dh, _ := hashCanonicalJSON(r.details)
		rh, _ := computeAccessRowHash(prev, r.id, r.actor, r.action, r.rt, r.rid, dh, r.createdAt)
		// Adversary flips a byte of row_hash on the third row.
		if i == 2 {
			rh = append([]byte(nil), rh...)
			rh[0] ^= 0xff
		}
		var prevForRow any
		if prev == nil {
			prevForRow = nil
		} else {
			prevForRow = prev
		}
		resultRows.AddRow(r.id, r.actor, r.action, r.rt, r.rid, []byte(r.details), r.createdAt, prevForRow, rh, dh)
		prev = rh
	}

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, actor_user_id`)).WillReturnRows(resultRows)

	v := NewVerifier(db)
	rep, err := v.VerifyAccessChain(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Status != VerificationFailed {
		t.Errorf("status = %s, want failed", rep.Status)
	}
	if rep.FailedAtRowID != "row-3" {
		t.Errorf("failedAtRowId = %q, want row-3", rep.FailedAtRowID)
	}
}

func TestVerifyAccessChain_BrokenLink(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	rows := buildAccessChainRows(t)
	var prev []byte
	resultRows := sqlmock.NewRows([]string{
		"id", "actor_user_id", "action", "resource_type", "resource_id",
		"details", "created_at", "prev_hash", "row_hash", "details_hash",
	})
	for i, r := range rows {
		dh, _ := hashCanonicalJSON(r.details)
		// Break the prev-link on row 2 by feeding garbage prev.
		rowPrev := prev
		if i == 1 {
			rowPrev = make([]byte, 32) // wrong, but sized
			rowPrev[0] = 0x42
		}
		rh, _ := computeAccessRowHash(rowPrev, r.id, r.actor, r.action, r.rt, r.rid, dh, r.createdAt)
		var prevForRow any
		if rowPrev == nil {
			prevForRow = nil
		} else {
			prevForRow = rowPrev
		}
		resultRows.AddRow(r.id, r.actor, r.action, r.rt, r.rid, []byte(r.details), r.createdAt, prevForRow, rh, dh)
		prev = rh
	}

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, actor_user_id`)).WillReturnRows(resultRows)

	v := NewVerifier(db)
	rep, err := v.VerifyAccessChain(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Status != VerificationFailed {
		t.Errorf("status = %s, want failed", rep.Status)
	}
	if rep.FailedAtRowID != "row-2" {
		t.Errorf("failedAtRowId = %q, want row-2", rep.FailedAtRowID)
	}
}

// ---------------------------------------------------------------------------
// VerifyMutationChain
// ---------------------------------------------------------------------------

type mutationFixtureRow struct {
	id, actor, action, tType, tID, reqID string
	before, after, metadata              json.RawMessage
	createdAt                            time.Time
}

func buildMutationRows(t *testing.T) []mutationFixtureRow {
	base := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	return []mutationFixtureRow{
		{"m-1", "admin-1", "update", "platform_settings", "_singleton_", "req-1",
			json.RawMessage(`{"version":1}`),
			json.RawMessage(`{"version":2}`),
			json.RawMessage(`{"reason":"manual"}`),
			base},
		{"m-2", "admin-2", "delete", "user", "u-9", "req-2",
			json.RawMessage(`{"id":"u-9","status":"active"}`),
			json.RawMessage(`null`),
			json.RawMessage(`{"reason":"gdpr"}`),
			base.Add(time.Minute)},
	}
}

func TestVerifyMutationChain_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	rows := buildMutationRows(t)
	var prev []byte
	resultRows := sqlmock.NewRows([]string{
		"id", "actor_user_id", "action", "target_type", "target_id", "request_id",
		"before_snapshot", "after_snapshot", "metadata", "created_at",
		"prev_hash", "row_hash", "before_hash", "after_hash", "metadata_hash",
	})
	for _, r := range rows {
		bh, _ := hashCanonicalJSON(r.before)
		ah, _ := hashCanonicalJSON(r.after)
		mh, _ := hashCanonicalJSON(r.metadata)
		rh, err := computeMutationRowHash(prev, r.id, r.actor, r.action, r.tType, r.tID, r.reqID, bh, ah, mh, r.createdAt)
		if err != nil {
			t.Fatalf("compute mutation: %v", err)
		}
		var prevForRow any
		if prev == nil {
			prevForRow = nil
		} else {
			prevForRow = prev
		}
		resultRows.AddRow(r.id, r.actor, r.action, r.tType, r.tID, r.reqID,
			[]byte(r.before), []byte(r.after), []byte(r.metadata), r.createdAt,
			prevForRow, rh, bh, ah, mh)
		prev = rh
	}

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM admin_change_log`)).WillReturnRows(resultRows)

	v := NewVerifier(db)
	rep, err := v.VerifyMutationChain(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Status != VerificationOK {
		t.Errorf("status = %s, want ok (reason=%s)", rep.Status, rep.FailedReason)
	}
	if rep.HashedRows != len(rows) {
		t.Errorf("hashedRows = %d, want %d", rep.HashedRows, len(rows))
	}
}

func TestVerifyMutationChain_TamperedAfterSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	rows := buildMutationRows(t)
	var prev []byte
	resultRows := sqlmock.NewRows([]string{
		"id", "actor_user_id", "action", "target_type", "target_id", "request_id",
		"before_snapshot", "after_snapshot", "metadata", "created_at",
		"prev_hash", "row_hash", "before_hash", "after_hash", "metadata_hash",
	})
	for i, r := range rows {
		bh, _ := hashCanonicalJSON(r.before)
		ah, _ := hashCanonicalJSON(r.after)
		mh, _ := hashCanonicalJSON(r.metadata)
		rh, _ := computeMutationRowHash(prev, r.id, r.actor, r.action, r.tType, r.tID, r.reqID, bh, ah, mh, r.createdAt)
		// Editor flips the after_snapshot on the first row.
		afterPayload := r.after
		if i == 0 {
			afterPayload = json.RawMessage(`{"version":99}`)
		}
		var prevForRow any
		if prev == nil {
			prevForRow = nil
		} else {
			prevForRow = prev
		}
		resultRows.AddRow(r.id, r.actor, r.action, r.tType, r.tID, r.reqID,
			[]byte(r.before), []byte(afterPayload), []byte(r.metadata), r.createdAt,
			prevForRow, rh, bh, ah, mh)
		prev = rh
	}

	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM admin_change_log`)).WillReturnRows(resultRows)

	v := NewVerifier(db)
	rep, err := v.VerifyMutationChain(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Status != VerificationFailed {
		t.Errorf("status = %s, want failed", rep.Status)
	}
	if rep.FailedAtRowID != "m-1" {
		t.Errorf("failedAtRowId = %q, want m-1", rep.FailedAtRowID)
	}
}
