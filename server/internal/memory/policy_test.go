package memory

import "testing"

func TestDefaultPolicy(t *testing.T) {
	p := DefaultPolicy{}

	// 1. Owner matches
	if !p.CanRecall("user1", Item{OwnerUserID: "user1", Sensitivity: "secret"}) {
		t.Error("owner should be able to see their own secret memory")
	}

	// 2. No owner
	if !p.CanRecall("user2", Item{OwnerUserID: "", Sensitivity: "secret"}) {
		t.Error("memory with no owner should be visible to anyone (legacy compat)")
	}

	// 3. Secret but not owner
	if p.CanRecall("user2", Item{OwnerUserID: "user1", Sensitivity: "secret"}) {
		t.Error("non-owner should not see secret memory")
	}

	// 4. Marketplace visibility
	if !p.CanRecall("user2", Item{OwnerUserID: "user1", Sensitivity: "public", Visibility: "marketplace"}) {
		t.Error("marketplace visibility should allow access")
	}

	// 5. Fund visibility (default denies for now as fund membership isn't checked here)
	if p.CanRecall("user2", Item{OwnerUserID: "user1", Sensitivity: "internal", Visibility: "fund"}) {
		t.Error("fund visibility should deny by default in this basic implementation")
	}

	// 6. Private visibility
	if p.CanRecall("user2", Item{OwnerUserID: "user1", Sensitivity: "public", Visibility: "private"}) {
		t.Error("private visibility should deny access to non-owner")
	}
}
