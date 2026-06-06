// fund_context_test.go — covers the W14-1 ctx propagation
// helpers. These tests are deliberately tiny: the helpers are
// pure, and the rule of thumb is that ctx-key utilities deserve
// just enough tests to lock in the public contract — anyone
// extending the file in the future should not have to reverse-
// engineer the empty-string semantics.

package recall

import (
	"context"
	"testing"
)

func TestWithFundID_RoundTrip(t *testing.T) {
	ctx := WithFundID(context.Background(), "fund-123")
	if got := FundIDFromContext(ctx); got != "fund-123" {
		t.Fatalf("expected fund-123, got %q", got)
	}
}

// Empty fundID must NOT plant a "" sentinel. The
// embedquotaobs.Recorder treats "" as "drop this observation",
// but only because we also guarantee here that "" never makes
// it onto the ctx in the first place — keeping the contract
// single-sourced rather than two ends each "trusting" the other
// to filter empties.
func TestWithFundID_EmptyReturnsParent(t *testing.T) {
	parent := context.Background()
	got := WithFundID(parent, "")
	if got != parent {
		t.Fatalf("expected parent ctx unchanged for empty fundID")
	}
}

func TestWithFundID_NilCtxIsSafe(t *testing.T) {
	if got := WithFundID(nil, "fund-x"); got != nil { //nolint:staticcheck
		t.Fatalf("expected nil ctx to flow through unchanged, got %T", got)
	}
}

func TestFundIDFromContext_MissingReturnsEmpty(t *testing.T) {
	if got := FundIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty fundID for bare ctx, got %q", got)
	}
}

func TestFundIDFromContext_NilCtxIsSafe(t *testing.T) {
	if got := FundIDFromContext(nil); got != "" { //nolint:staticcheck
		t.Fatalf("expected empty fundID for nil ctx, got %q", got)
	}
}
