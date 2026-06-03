package main

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestCostRollupLoop_Unwired_DoesNothing(t *testing.T) {
	l := newLLMCostRollupLoop(nil, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	l.Start(ctx)
	<-ctx.Done()
	if l.Ticks() != 0 {
		t.Fatalf("expected 0 ticks when unwired, got %d", l.Ticks())
	}
}

func TestCostRollupLoop_Defaults(t *testing.T) {
	l := newLLMCostRollupLoop(nil, slog.Default())
	if l.interval != 1*time.Hour {
		t.Fatalf("default interval should be 1h, got %s", l.interval)
	}
	if l.startupBack < 24*time.Hour {
		t.Fatalf("startupBack should cover at least 24h, got %s", l.startupBack)
	}
	if l.tickBack < l.interval {
		t.Fatalf("tickBack should overlap interval to avoid gaps (%s < %s)", l.tickBack, l.interval)
	}
}
