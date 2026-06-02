package modelab

import (
	"math"
	"strconv"
	"testing"
)

func TestPickArm_DeterministicForSameKey(t *testing.T) {
	split := []float64{0.5, 0.5}
	a := PickArm("run-1", "pm_decision", "agent-1", split)
	b := PickArm("run-1", "pm_decision", "agent-1", split)
	if a != b {
		t.Fatalf("expected same key to always pick same arm, got %d vs %d", a, b)
	}
}

func TestPickArm_DifferentAgentsCanLandInDifferentArms(t *testing.T) {
	split := []float64{0.5, 0.5}
	seen := map[int]int{}
	for i := 0; i < 200; i++ {
		idx := PickArm("run-1", "pm_decision", "agent-"+strconv.Itoa(i), split)
		seen[idx]++
	}
	if len(seen) < 2 {
		t.Fatalf("expected both arms to be picked across 200 agents, got %v", seen)
	}
}

func TestPickArm_RespectsTrafficSplitApproximately(t *testing.T) {
	split := []float64{0.9, 0.1}
	count := [2]int{}
	const N = 5000
	for i := 0; i < N; i++ {
		idx := PickArm("run-"+strconv.Itoa(i), "step", "agent", split)
		count[idx]++
	}
	// Loose bounds: at 5000 samples a 90/10 split should land at
	// roughly 4500/500. We accept ±200 absolute drift so the test
	// stays stable across SHA implementations (SHA-256 is fixed,
	// but the test should also survive a future hash swap).
	if math.Abs(float64(count[0])-4500) > 200 {
		t.Errorf("arm 0 count off: got %d want ~4500 (split 0.9/0.1)", count[0])
	}
	if math.Abs(float64(count[1])-500) > 200 {
		t.Errorf("arm 1 count off: got %d want ~500", count[1])
	}
}

func TestPickArm_EmptySplitReturnsZero(t *testing.T) {
	if got := PickArm("r", "s", "a", nil); got != 0 {
		t.Fatalf("empty split should return 0, got %d", got)
	}
	if got := PickArm("r", "s", "a", []float64{}); got != 0 {
		t.Fatalf("empty split should return 0, got %d", got)
	}
}

func TestPickArm_SingleArmAlwaysZero(t *testing.T) {
	if got := PickArm("r", "s", "a", []float64{1.0}); got != 0 {
		t.Fatalf("single arm should always be 0, got %d", got)
	}
}

func TestPickArm_ThreeWaySplit(t *testing.T) {
	split := []float64{0.33, 0.33, 0.34}
	count := [3]int{}
	for i := 0; i < 3000; i++ {
		idx := PickArm("run-"+strconv.Itoa(i), "step", "agent", split)
		if idx < 0 || idx >= 3 {
			t.Fatalf("out of range arm idx=%d", idx)
		}
		count[idx]++
	}
	for i, c := range count {
		if c < 700 || c > 1300 {
			t.Errorf("arm %d count %d outside [700,1300]", i, c)
		}
	}
}

func TestPickArm_ZeroWeightArmNeverPicked(t *testing.T) {
	split := []float64{1.0, 0.0}
	for i := 0; i < 200; i++ {
		idx := PickArm("run-"+strconv.Itoa(i), "step", "agent", split)
		if idx == 1 {
			t.Fatalf("zero-weight arm should never be picked, got idx=1 at iter %d", i)
		}
	}
}
