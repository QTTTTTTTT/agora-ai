package main

import (
	"context"
	"testing"
)

// TestBuildAgentTrackRecordWithNilReposReturnsEmpty covers the
// "no alpha-aware-memory wiring" path. The Sprint 9.1 block must
// degrade silently when either the reputation repo or the alpha
// lesson repo is unwired (very common in tests + legacy smoke
// builds) so the PM prompt simply omits the section.
func TestBuildAgentTrackRecordWithNilReposReturnsEmpty(t *testing.T) {
	agent := &runtimePMAgent{}
	if got := agent.buildAgentTrackRecord(context.Background(), "fund-x"); got != "" {
		t.Fatalf("expected empty track-record on nil repos, got %q", got)
	}
}

// TestBuildAgentTrackRecordWithNilAgentReturnsEmpty mirrors the
// scorecard / lesson-replay symmetric guards: a nil receiver must
// never panic on the daily critical path.
func TestBuildAgentTrackRecordWithNilAgentReturnsEmpty(t *testing.T) {
	var agent *runtimePMAgent
	if got := agent.buildAgentTrackRecord(context.Background(), "fund-x"); got != "" {
		t.Fatalf("expected empty track-record on nil receiver, got %q", got)
	}
}
