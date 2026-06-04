package main

import (
	"encoding/json"
	"testing"
)

// TestPMPathChildSplittingEnabled_Matrix nails down each branch of
// the feature-flag resolver. The "missing key" and "malformed JSON"
// rows are the safety-critical ones — a regression that flipped
// either to true would silently turn on child splitting for every
// legacy fund. Keep the assertions tight.
func TestPMPathChildSplittingEnabled_Matrix(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{name: "nil raw", raw: nil, want: false},
		{name: "empty raw", raw: json.RawMessage(``), want: false},
		{name: "empty object — key absent", raw: json.RawMessage(`{}`), want: false},
		{name: "key absent among others",
			raw:  json.RawMessage(`{"strategySleeves":{"enabled":true}}`),
			want: false,
		},
		{name: "explicit false",
			raw:  json.RawMessage(`{"pm_path_child_splitting":false}`),
			want: false,
		},
		{name: "explicit true",
			raw:  json.RawMessage(`{"pm_path_child_splitting":true}`),
			want: true,
		},
		{name: "true coexists with other fields",
			raw: json.RawMessage(`{
				"strategySleeves": {"enabled": true},
				"pm_path_child_splitting": true,
				"some_other_flag": false
			}`),
			want: true,
		},
		// Malformed JSON must NOT silently flip the flag on.
		{name: "malformed json — truncated",
			raw:  json.RawMessage(`{"pm_path_child_splitting":`),
			want: false,
		},
		{name: "malformed json — wrong type",
			raw:  json.RawMessage(`"not an object"`),
			want: false,
		},
		// A type mismatch on the field itself — the field exists
		// but its value is a string, not a bool. Go's strict
		// unmarshal rejects this with an error; we fail safe to
		// false so a typo in the JSON can't accidentally turn the
		// splitter on.
		{name: "wrong field type — string",
			raw:  json.RawMessage(`{"pm_path_child_splitting":"true"}`),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pmPathChildSplittingEnabled(tc.raw)
			if got != tc.want {
				t.Errorf("pmPathChildSplittingEnabled(%s) = %v, want %v",
					string(tc.raw), got, tc.want)
			}
		})
	}
}

// TestFuturesCashLedgerV2Enabled_Matrix mirrors the splitter
// resolver's safety matrix for the T7 futures-cash-flow flag.
// Same critical assertion: missing / malformed config must NOT
// flip the v2 path on, because the flip changes funds.current_capital
// math on every futures fill and a silent regression would corrupt
// the journal for funds whose operator never opted in.
func TestFuturesCashLedgerV2Enabled_Matrix(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{name: "nil raw", raw: nil, want: false},
		{name: "empty object", raw: json.RawMessage(`{}`), want: false},
		{name: "explicit false",
			raw:  json.RawMessage(`{"futures_cash_ledger_v2":false}`),
			want: false,
		},
		{name: "explicit true",
			raw:  json.RawMessage(`{"futures_cash_ledger_v2":true}`),
			want: true,
		},
		{name: "coexists with splitter flag",
			raw: json.RawMessage(`{
				"pm_path_child_splitting": true,
				"futures_cash_ledger_v2": true
			}`),
			want: true,
		},
		{name: "malformed json fails safe",
			raw:  json.RawMessage(`{"futures_cash_ledger_v2":`),
			want: false,
		},
		{name: "wrong value type fails safe",
			raw:  json.RawMessage(`{"futures_cash_ledger_v2":"true"}`),
			want: false,
		},
		// Independence: the splitter flag being on must NOT
		// imply the futures v2 flag is on (different opt-in
		// surfaces, different scope of risk).
		{name: "splitter on but futures v2 absent stays off",
			raw:  json.RawMessage(`{"pm_path_child_splitting":true}`),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := futuresCashLedgerV2Enabled(tc.raw)
			if got != tc.want {
				t.Errorf("futuresCashLedgerV2Enabled(%s) = %v, want %v",
					string(tc.raw), got, tc.want)
			}
		})
	}
}
