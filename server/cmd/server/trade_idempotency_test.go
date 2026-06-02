package main

import (
	"database/sql"
	"strings"
	"testing"
)

// TestMintTradeIdempotencyKey locks the wire format of the
// runtime-engine -> trade_executions.client_idempotency_key bridge.
// The format MUST stay stable: existing rows in production carry
// keys minted by an earlier deployment, and changing the format
// would break the ON CONFLICT idempotency guarantee for in-flight
// retries that span a deployment boundary.
func TestMintTradeIdempotencyKey(t *testing.T) {
	cases := []struct {
		name     string
		actionID string
		side     string
		quantity int
		want     sql.NullString
	}{
		{
			name:     "happy path buy",
			actionID: "00000000-0000-0000-0000-000000000001",
			side:     "buy",
			quantity: 100,
			want: sql.NullString{
				String: "trade:00000000-0000-0000-0000-000000000001:buy:100",
				Valid:  true,
			},
		},
		{
			name:     "happy path sell",
			actionID: "abc",
			side:     "sell",
			quantity: 5,
			want: sql.NullString{
				String: "trade:abc:sell:5",
				Valid:  true,
			},
		},
		{
			name:     "side normalised to lower-case",
			actionID: "abc",
			side:     "BUY",
			quantity: 1,
			want: sql.NullString{
				String: "trade:abc:buy:1",
				Valid:  true,
			},
		},
		{
			name:     "side defaults to buy when blank",
			actionID: "abc",
			side:     "",
			quantity: 1,
			want: sql.NullString{
				String: "trade:abc:buy:1",
				Valid:  true,
			},
		},
		{
			name:     "side trimmed",
			actionID: "abc",
			side:     "  sell  ",
			quantity: 1,
			want: sql.NullString{
				String: "trade:abc:sell:1",
				Valid:  true,
			},
		},
		{
			name:     "empty action id falls back to non-idempotent path",
			actionID: "",
			side:     "buy",
			quantity: 1,
			want:     sql.NullString{},
		},
		{
			name:     "whitespace action id falls back",
			actionID: "   ",
			side:     "buy",
			quantity: 1,
			want:     sql.NullString{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mintTradeIdempotencyKey(tc.actionID, tc.side, tc.quantity)
			if got != tc.want {
				t.Errorf("mintTradeIdempotencyKey(%q, %q, %d) = %#v, want %#v",
					tc.actionID, tc.side, tc.quantity, got, tc.want)
			}
		})
	}
}

// TestMintTradeIdempotencyKey_DeterministicAcrossCalls verifies that
// the function is pure: two invocations with the same inputs return
// the same key. This is the property the ON CONFLICT path depends on
// — a network-retry of the same plan-action submission must mint
// the same key both times so the second insert collapses.
func TestMintTradeIdempotencyKey_DeterministicAcrossCalls(t *testing.T) {
	first := mintTradeIdempotencyKey("action-1", "buy", 10)
	second := mintTradeIdempotencyKey("action-1", "buy", 10)
	if first != second {
		t.Errorf("non-deterministic: %#v vs %#v", first, second)
	}
}

// TestMintTradeIdempotencyKey_FormatPrefixed asserts the key always
// starts with "trade:" so the operations team can grep
// trade_executions.client_idempotency_key and know which subsystem
// minted it. (investment_plans rows mint with their own prefix; this
// avoids collisions in the partial UNIQUE index across both tables —
// each table's index is its own, but having distinct prefixes makes
// log mining painless.)
func TestMintTradeIdempotencyKey_FormatPrefixed(t *testing.T) {
	got := mintTradeIdempotencyKey("action-1", "buy", 10)
	if !got.Valid {
		t.Fatalf("expected a valid key")
	}
	if !strings.HasPrefix(got.String, "trade:") {
		t.Errorf("missing trade: prefix in %q", got.String)
	}
}
