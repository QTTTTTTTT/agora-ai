package main

import (
	"context"
	"testing"
)

// TestBuildLessonReplayWithNilMemoryRepoReturnsEmpty covers the
// "no memory store wired" path. The replay must never crash on
// the daily critical workflow; a missing dep degrades to an
// empty section in the prompt.
func TestBuildLessonReplayWithNilMemoryRepoReturnsEmpty(t *testing.T) {
	agent := &runtimePMAgent{}
	if got := agent.buildLessonReplay(context.Background(), "fund-x"); got != "" {
		t.Fatalf("expected empty replay on nil memoryRepo, got %q", got)
	}
}

// TestBuildLessonReplayWithNilAgentReturnsEmpty mirrors the
// scorecard wiring's symmetric guard. A nil receiver must not
// panic — this is the (extremely rare) code path where the
// wiring layer hands the engine a sentinel zero value.
func TestBuildLessonReplayWithNilAgentReturnsEmpty(t *testing.T) {
	var agent *runtimePMAgent
	if got := agent.buildLessonReplay(context.Background(), "fund-x"); got != "" {
		t.Fatalf("expected empty replay on nil receiver, got %q", got)
	}
}
