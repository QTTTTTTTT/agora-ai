package main

import (
	"database/sql"
	"regexp"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectAccessLogInsert programs the sqlmock expectation pair that the
// audit hash-chain path requires:
//
//  1. SELECT row_hash from data_access_log to look up the previous
//     row's hash. Since most test DBs are empty for the access log,
//     the helper returns sql.ErrNoRows so the chain treats the new
//     row as the chain genesis.
//  2. INSERT INTO data_access_log with the full 10-column hash-chain
//     payload. The actor/action/resource_type/resource_id arguments
//     are matched exactly so callers retain full attribution checks;
//     the remaining columns (uuid id, JSONB details, created_at,
//     prev_hash, row_hash, details_hash) are captured with
//     sqlmock.AnyArg() because the test never wants to be tightly
//     coupled to the chain encoding details.
//
// Tests that exercise the access-log path SHOULD use this helper
// instead of hand-rolling the expectations so a future schema or
// chain-encoding change only has to be reflected in one place.
func expectAccessLogInsert(mock sqlmock.Sqlmock, actorUserID, action, resourceType, resourceID string) {
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT row_hash
		FROM data_access_log
		WHERE row_hash IS NOT NULL
		ORDER BY created_at DESC, id DESC
		LIMIT 1`)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta(`
		INSERT INTO data_access_log
			(id, actor_user_id, action, resource_type, resource_id, details, created_at, prev_hash, row_hash, details_hash)
		VALUES
			($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`)).
		WithArgs(
			sqlmock.AnyArg(), // id (uuid)
			actorUserID, action, resourceType, resourceID,
			sqlmock.AnyArg(), // details JSON
			sqlmock.AnyArg(), // created_at
			nil,              // prev_hash (genesis)
			sqlmock.AnyArg(), // row_hash
			sqlmock.AnyArg(), // details_hash
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
}
