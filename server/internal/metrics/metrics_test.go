package metrics

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestCounter_AddInc(t *testing.T) {
	c := NewCounter("foo_total", "test")
	c.Inc(Labels{"a": "x"})
	c.Inc(Labels{"a": "x"})
	c.Add(Labels{"a": "y"}, 5)

	reg := NewRegistry()
	reg.MustRegister(c)
	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `foo_total{a="x"} 2`) {
		t.Errorf("missing foo_total{a=x}: %s", out)
	}
	if !strings.Contains(out, `foo_total{a="y"} 5`) {
		t.Errorf("missing foo_total{a=y}: %s", out)
	}
	if !strings.Contains(out, "# TYPE foo_total counter") {
		t.Errorf("missing TYPE header: %s", out)
	}
}

func TestCounter_NegativeIgnored(t *testing.T) {
	c := NewCounter("c_total", "")
	c.Inc(nil)
	c.Add(nil, -10)
	c.Add(nil, 3)

	reg := NewRegistry()
	reg.MustRegister(c)
	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "c_total 4") {
		t.Errorf("expected c_total 4 (1+3, -10 dropped): %s", buf.String())
	}
}

func TestGauge_SetAndAdd(t *testing.T) {
	g := NewGauge("inflight", "")
	g.Set(Labels{"q": "a"}, 5)
	g.Add(Labels{"q": "a"}, 3)
	g.Add(Labels{"q": "a"}, -1)
	g.Set(Labels{"q": "b"}, 1)

	reg := NewRegistry()
	reg.MustRegister(g)
	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `inflight{q="a"} 7`) {
		t.Errorf("expected gauge a=7: %s", out)
	}
	if !strings.Contains(out, `inflight{q="b"} 1`) {
		t.Errorf("expected gauge b=1: %s", out)
	}
}

func TestHistogram_Observation(t *testing.T) {
	h := NewHistogram("lat_seconds", "", []float64{0.1, 1, 10})
	h.Observe(nil, 0.05) // bucket 0.1, 1, 10, +Inf
	h.Observe(nil, 0.5)  // bucket 1, 10, +Inf
	h.Observe(nil, 5)    // bucket 10, +Inf
	h.Observe(nil, 100)  // bucket +Inf

	reg := NewRegistry()
	reg.MustRegister(h)
	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	// bucket le=0.1 should be 1 (only 0.05).
	if !strings.Contains(out, `lat_seconds_bucket{le="0.1"} 1`) {
		t.Errorf("le=0.1 wrong: %s", out)
	}
	// bucket le=1 should be 2 (0.05 + 0.5).
	if !strings.Contains(out, `lat_seconds_bucket{le="1"} 2`) {
		t.Errorf("le=1 wrong: %s", out)
	}
	// bucket le=10 should be 3 (0.05 + 0.5 + 5).
	if !strings.Contains(out, `lat_seconds_bucket{le="10"} 3`) {
		t.Errorf("le=10 wrong: %s", out)
	}
	// +Inf bucket should be 4.
	if !strings.Contains(out, `lat_seconds_bucket{le="+Inf"} 4`) {
		t.Errorf("le=+Inf wrong: %s", out)
	}
	// Sum = 0.05 + 0.5 + 5 + 100 = 105.55
	if !strings.Contains(out, "lat_seconds_sum 105.55") {
		t.Errorf("sum wrong: %s", out)
	}
	// Count = 4
	if !strings.Contains(out, "lat_seconds_count 4") {
		t.Errorf("count wrong: %s", out)
	}
}

func TestRegistry_DuplicatePanics(t *testing.T) {
	reg := NewRegistry()
	c := NewCounter("dup_total", "")
	reg.MustRegister(c)
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on duplicate")
		}
	}()
	c2 := NewCounter("dup_total", "")
	reg.MustRegister(c2)
}

func TestLabels_Canonicalisation(t *testing.T) {
	a := Labels{"x": "1", "y": "2"}
	b := Labels{"y": "2", "x": "1"}
	if a.canonicalKey() != b.canonicalKey() {
		t.Errorf("canonical keys must match regardless of insertion order")
	}
}

func TestEscapeLabelValue(t *testing.T) {
	c := NewCounter("c_total", "")
	c.Inc(Labels{"k": `a"b\c` + "\n" + "d"})

	reg := NewRegistry()
	reg.MustRegister(c)
	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `c_total{k="a\"b\\c\nd"} 1`) {
		t.Errorf("escape failed: %s", buf.String())
	}
}

func TestStdlib_Registration(t *testing.T) {
	reg := NewRegistry()
	s := NewStdlib(reg)
	s.HTTPRequestsTotal.Inc(Labels{"method": "GET", "status": "2xx"})
	s.LLMLatency.Observe(Labels{"provider": "openai"}, 0.42)
	s.SchedulerLeaderState.Set(Labels{"lease": "scheduler"}, 1)

	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"http_requests_total",
		"llm_request_duration_seconds_bucket",
		"scheduler_leader_state",
		"plans_generated_total",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Stdlib output missing %q", want)
		}
	}
}

func TestCounter_Concurrency(t *testing.T) {
	c := NewCounter("cc_total", "")
	const N = 1000
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc(Labels{"x": "y"})
		}()
	}
	wg.Wait()

	reg := NewRegistry()
	reg.MustRegister(c)
	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "cc_total{x=\"y\"} 1000") {
		t.Errorf("concurrent inc lost updates: %s", buf.String())
	}
}
