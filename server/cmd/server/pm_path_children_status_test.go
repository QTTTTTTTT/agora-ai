package main

import (
	"testing"
)

// TestAggregateChildrenStatus_Matrix nails down the precedence
// table from the file-level comment in pm_path_children_status.go.
// The two safety-critical assertions are:
//
//   * "100% filled but one row reports zero filled_qty" must NOT
//     collapse to "filled" — it has to surface as "partial:99"
//     so the operator notices that the qty math has drifted.
//   * Empty children returns "" (NOT "rejected" or "pending"),
//     a deliberate cue that the caller should fall back to its
//     own status decision instead of treating the result as
//     authoritative.
func TestAggregateChildrenStatus_Matrix(t *testing.T) {
	cases := []struct {
		name         string
		children     []ChildStatus
		parentQty    float64
		want         string
	}{
		// Happy path: every child filled at its target qty.
		{
			name: "all filled",
			children: []ChildStatus{
				{Status: "filled", FilledQty: 800},
				{Status: "filled", FilledQty: 800},
				{Status: "filled", FilledQty: 800},
				{Status: "filled", FilledQty: 800},
				{Status: "filled", FilledQty: 800},
			},
			parentQty: 4000,
			want:      "filled",
		},
		// Terminal unhappy: every child rejected or cancelled.
		{
			name: "all rejected",
			children: []ChildStatus{
				{Status: "rejected", FilledQty: 0},
				{Status: "rejected", FilledQty: 0},
			},
			parentQty: 1000,
			want:      "rejected",
		},
		{
			name: "all cancelled (treated as rejected family)",
			children: []ChildStatus{
				{Status: "cancelled", FilledQty: 0},
				{Status: "cancelled", FilledQty: 0},
			},
			parentQty: 1000,
			want:      "rejected",
		},
		// Mixed: 3 of 5 children filled, 2 still working → 60%.
		{
			name: "60% partial",
			children: []ChildStatus{
				{Status: "filled", FilledQty: 800},
				{Status: "filled", FilledQty: 800},
				{Status: "filled", FilledQty: 800},
				{Status: "working", FilledQty: 0},
				{Status: "working", FilledQty: 0},
			},
			parentQty: 4000,
			want:      "partial:60",
		},
		// Partial fill within a single child (live broker emits
		// 400 of 800 then halts) — totalFilled=3600, parent=4000
		// → 90%.
		{
			name: "fractional child fill rolls up to 90%",
			children: []ChildStatus{
				{Status: "filled", FilledQty: 800},
				{Status: "filled", FilledQty: 800},
				{Status: "filled", FilledQty: 800},
				{Status: "filled", FilledQty: 800},
				{Status: "partial", FilledQty: 400},
			},
			parentQty: 4000,
			want:      "partial:90",
		},
		// Edge case: math gives 100% but ONE child isn't filled —
		// must clamp to "partial:99" so "filled" stays reserved
		// for the all-filled terminal happy path.
		{
			name: "math 100% but a child is still working clamps to 99",
			children: []ChildStatus{
				{Status: "filled", FilledQty: 1000},
				{Status: "filled", FilledQty: 1000},
				{Status: "filled", FilledQty: 1000},
				{Status: "working", FilledQty: 1000}, // working but qty filled — odd, but possible on a live broker
			},
			parentQty: 4000,
			want:      "partial:99",
		},
		// All children pending with zero fill — strictly-zero pending.
		{
			name: "all pending zero fill returns pending",
			children: []ChildStatus{
				{Status: "pending", FilledQty: 0},
				{Status: "pending", FilledQty: 0},
			},
			parentQty: 2000,
			want:      "pending",
		},
		// Working + triggered mix with zero fills — still pending.
		{
			name: "mixed in-flight statuses zero fill returns pending",
			children: []ChildStatus{
				{Status: "working", FilledQty: 0},
				{Status: "triggered", FilledQty: 0},
			},
			parentQty: 2000,
			want:      "pending",
		},
		// Edge case: parentQty <= 0 degrades gracefully — every
		// child filled → "filled", otherwise "pending". Never
		// divides by zero.
		{
			name: "zero parent qty + all filled returns filled",
			children: []ChildStatus{
				{Status: "filled", FilledQty: 100},
			},
			parentQty: 0,
			want:      "filled",
		},
		{
			name: "zero parent qty + mixed returns pending",
			children: []ChildStatus{
				{Status: "filled", FilledQty: 100},
				{Status: "working", FilledQty: 0},
			},
			parentQty: 0,
			want:      "pending",
		},
		// Empty input — caller falls back to its own status.
		{
			name:      "empty children returns empty string",
			children:  nil,
			parentQty: 4000,
			want:      "",
		},
		// Case + whitespace normalisation.
		{
			name: "FILLED uppercase + leading space all filled",
			children: []ChildStatus{
				{Status: " FILLED ", FilledQty: 800},
				{Status: "filled", FilledQty: 800},
			},
			parentQty: 1600,
			want:      "filled",
		},
		// Very tiny fill — floor to 1 so a non-zero filled qty
		// never collapses to "partial:0" (which would read as
		// "haven't started" and confuse the operator).
		{
			name: "tiny fill floors to partial:1",
			children: []ChildStatus{
				{Status: "partial", FilledQty: 1},
				{Status: "working", FilledQty: 0},
			},
			parentQty: 10000,
			want:      "partial:1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := aggregateChildrenStatus(tc.children, tc.parentQty)
			if got != tc.want {
				t.Errorf("aggregateChildrenStatus(%v, parent=%v) = %q, want %q",
					tc.children, tc.parentQty, got, tc.want)
			}
		})
	}
}
