package agentablation

import (
	"math"
	"testing"
	"time"
)

func TestScheduleDeterministic(t *testing.T) {
	s1 := NewSchedule("seed-1", DefaultConfig())
	s2 := NewSchedule("seed-1", DefaultConfig())
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"bull", "bear", "quant"} {
		if s1.IsOffThisWeek(id, now) != s2.IsOffThisWeek(id, now) {
			t.Errorf("schedule should be deterministic across instances")
		}
	}
}

func TestScheduleDifferentSeedsDifferentOffWeeks(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	a := NewSchedule("alpha", DefaultConfig())
	b := NewSchedule("beta", DefaultConfig())
	differs := false
	for _, id := range []string{"bull", "bear", "quant", "fundamentals"} {
		if a.IsOffThisWeek(id, now) != b.IsOffThisWeek(id, now) {
			differs = true
		}
	}
	if !differs {
		t.Errorf("at least one agent should differ across seeds")
	}
}

func TestScheduleEachAgentExactlyOneOffWeekPerCycle(t *testing.T) {
	s := NewSchedule("seed-1", DefaultConfig())
	cycle := DefaultConfig().CycleWeeks
	weekZero := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	offCount := 0
	for w := 0; w < cycle; w++ {
		when := weekZero.Add(time.Duration(w*7*24) * time.Hour)
		if s.IsOffThisWeek("bull", when) {
			offCount++
		}
	}
	if offCount != 1 {
		t.Errorf("agent off-week count over cycle: got %d, want 1", offCount)
	}
}

func TestRecordSeparatesOnAndOff(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	for i := 0; i < 10; i++ {
		tr.Record(AlphaSample{AgentID: "a", IsOff: false, Alpha: 0.05})
		tr.Record(AlphaSample{AgentID: "a", IsOff: true, Alpha: 0.02})
	}
	rep := tr.Snapshot()
	if len(rep) != 1 {
		t.Fatalf("snap: got %d, want 1", len(rep))
	}
	if rep[0].OnSamples != 10 || rep[0].OffSamples != 10 {
		t.Errorf("samples: on=%d off=%d", rep[0].OnSamples, rep[0].OffSamples)
	}
	if math.Abs(rep[0].OnMeanAlpha-0.05) > 1e-9 || math.Abs(rep[0].OffMeanAlpha-0.02) > 1e-9 {
		t.Errorf("means: on=%v off=%v", rep[0].OnMeanAlpha, rep[0].OffMeanAlpha)
	}
}

func TestReportRespectsMinSamples(t *testing.T) {
	tr := NewTracker(Config{MinSamplesForReport: 5})
	for i := 0; i < 4; i++ {
		tr.Record(AlphaSample{AgentID: "a", IsOff: false, Alpha: 0.05})
		tr.Record(AlphaSample{AgentID: "a", IsOff: true, Alpha: 0.02})
	}
	if got := len(tr.Report()); got != 0 {
		t.Errorf("under-sampled agent should be excluded, got %d rows", got)
	}
	for i := 0; i < 5; i++ {
		tr.Record(AlphaSample{AgentID: "a", IsOff: false, Alpha: 0.05})
		tr.Record(AlphaSample{AgentID: "a", IsOff: true, Alpha: 0.02})
	}
	if got := len(tr.Report()); got != 1 {
		t.Errorf("after sufficient samples: got %d, want 1", got)
	}
}

func TestReportSortedByAbsDelta(t *testing.T) {
	tr := NewTracker(Config{MinSamplesForReport: 1})
	for i := 0; i < 5; i++ {
		tr.Record(AlphaSample{AgentID: "small", IsOff: false, Alpha: 0.01})
		tr.Record(AlphaSample{AgentID: "small", IsOff: true, Alpha: 0.005})
		tr.Record(AlphaSample{AgentID: "big", IsOff: false, Alpha: 0.10})
		tr.Record(AlphaSample{AgentID: "big", IsOff: true, Alpha: 0.0})
	}
	rep := tr.Report()
	if len(rep) != 2 {
		t.Fatalf("len: got %d, want 2", len(rep))
	}
	if rep[0].AgentID != "big" {
		t.Errorf("biggest delta first: got %q", rep[0].AgentID)
	}
}

func TestReportFlagsMaterialDelta(t *testing.T) {
	tr := NewTracker(Config{MinSamplesForReport: 5, SignificanceThreshold: 0.005})
	for i := 0; i < 10; i++ {
		tr.Record(AlphaSample{AgentID: "small", IsOff: false, Alpha: 0.001})
		tr.Record(AlphaSample{AgentID: "small", IsOff: true, Alpha: 0})
		tr.Record(AlphaSample{AgentID: "big", IsOff: false, Alpha: 0.05})
		tr.Record(AlphaSample{AgentID: "big", IsOff: true, Alpha: 0.0})
	}
	rep := tr.Report()
	for _, r := range rep {
		switch r.AgentID {
		case "small":
			if r.IsMaterial {
				t.Errorf("small delta should not be material: %v", r.Delta)
			}
		case "big":
			if !r.IsMaterial {
				t.Errorf("big delta should be material: %v", r.Delta)
			}
		}
	}
}

func TestRecordIgnoresEmptyAgentID(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	tr.Record(AlphaSample{AgentID: "", IsOff: false, Alpha: 0.1})
	if got := tr.Snapshot(); len(got) != 0 {
		t.Errorf("empty agent id should be ignored")
	}
}

func TestNilTrackerIsNoOp(t *testing.T) {
	var tr *Tracker
	tr.Record(AlphaSample{AgentID: "a"})
	if tr.Report() != nil {
		t.Errorf("nil tracker Report should be nil")
	}
}

func TestStddevComputed(t *testing.T) {
	tr := NewTracker(Config{MinSamplesForReport: 1})
	for _, v := range []float64{0.01, 0.03, 0.05, 0.07, 0.09} {
		tr.Record(AlphaSample{AgentID: "a", IsOff: false, Alpha: v})
	}
	for _, v := range []float64{0.0, 0.0, 0.0, 0.0, 0.0} {
		tr.Record(AlphaSample{AgentID: "a", IsOff: true, Alpha: v})
	}
	rep := tr.Report()
	if len(rep) != 1 {
		t.Fatalf("len: got %d, want 1", len(rep))
	}
	if rep[0].OnStdDev <= 0 {
		t.Errorf("OnStdDev: got %v, want > 0", rep[0].OnStdDev)
	}
	if rep[0].OffStdDev != 0 {
		t.Errorf("OffStdDev: got %v, want 0 (no variance)", rep[0].OffStdDev)
	}
}

func TestConfigNormalisation(t *testing.T) {
	tr := NewTracker(Config{}) // all zero
	for i := 0; i < 10; i++ {
		tr.Record(AlphaSample{AgentID: "a", IsOff: false, Alpha: 0.05})
		tr.Record(AlphaSample{AgentID: "a", IsOff: true, Alpha: 0.0})
	}
	if rep := tr.Report(); len(rep) != 1 {
		t.Errorf("normalised config should still produce report, got %d rows", len(rep))
	}
}
