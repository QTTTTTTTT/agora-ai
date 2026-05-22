package main

import (
	"context"
	"errors"
	"testing"

	"github.com/fundai/server/internal/subscription"
	"github.com/fundai/server/internal/workflow"
)

// TestBudgetExceededErrorBridgesBothSentinels is the crucial bridge
// test: when subscription.BudgetService returns ErrLLMBudgetExceeded,
// our adapter must wrap it so errors.Is matches BOTH
// subscription.ErrLLMBudgetExceeded (admin / billing UI) AND
// workflow.ErrLLMBudgetExceeded (orchestrator pause path).
//
// If this bridge breaks, the workflow will treat budget exhaustion as a
// generic step failure and keep retrying until call-count limits hit —
// burning more budget that won't actually be charged but pollutes the
// usage_entries table.
func TestBudgetExceededErrorBridgesBothSentinels(t *testing.T) {
	original := subscription.ErrLLMBudgetExceeded
	wrapped := &budgetExceededError{wrapped: original}

	if !errors.Is(wrapped, workflow.ErrLLMBudgetExceeded) {
		t.Fatal("bridge must satisfy workflow.ErrLLMBudgetExceeded")
	}
	if !errors.Is(wrapped, subscription.ErrLLMBudgetExceeded) {
		t.Fatal("bridge must satisfy subscription.ErrLLMBudgetExceeded via Unwrap")
	}
	if !subscription.IsLLMBudgetExceeded(wrapped) {
		t.Fatal("subscription.IsLLMBudgetExceeded must recognise the bridge")
	}
	if wrapped.Error() != original.Error() {
		t.Errorf("wrapped error message must echo the inner error, got %q", wrapped.Error())
	}
}

// TestBudgetExceededErrorNilSafe protects against nil-pointer bugs in
// the bridge — if anyone constructs &budgetExceededError{} by accident,
// the methods must not panic.
func TestBudgetExceededErrorNilSafe(t *testing.T) {
	var bridge *budgetExceededError
	_ = bridge.Error() // should not panic
	_ = bridge.Unwrap()
	_ = bridge.Is(workflow.ErrLLMBudgetExceeded)
}

// TestDollarBudgetGateAdapterPassesThroughOtherErrors ensures the
// adapter doesn't accidentally treat unrelated errors as budget
// exhaustion. A DB outage during the budget check should bubble up as
// itself, not as "you're out of budget" (which would silently pause
// every workflow).
func TestDollarBudgetGateAdapterPassesThroughOtherErrors(t *testing.T) {
	notBudget := errors.New("database is on fire")
	bridge := &budgetExceededError{wrapped: notBudget}

	if errors.Is(bridge, workflow.ErrLLMBudgetExceeded) {
		t.Fatal("non-budget errors must NOT be classified as budget exceeded")
	}
}

// TestDollarBudgetGateAdapterNilService proves the adapter no-ops on
// nil — used by tests that build the LLM client without a DB.
func TestDollarBudgetGateAdapterNilService(t *testing.T) {
	a := newDollarBudgetGateAdapter(nil)
	if err := a.Check(context.Background(), "u", "f"); err != nil {
		t.Fatalf("nil service must allow, got %v", err)
	}
}
