// analyst_panel_test.go — covers AnalystPanel fan-out, fan-in,
// failure tolerance, and the aggregateReports blending logic.

package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// --- Stub analyst for panel tests -----------------------------------------

// stubAnalyst lets tests pin the AnalystReport an analyst will
// return, simulate slow / failing analysts, and observe call
// counts. It implements AnalystAgent.
type stubAnalyst struct {
	id       string
	category AnalystCategory
	report   AnalystReport
	err      error
	delay    time.Duration
	calls    int32
}

func (s *stubAnalyst) ID() string                  { return s.id }
func (s *stubAnalyst) Name() string                { return s.id + "-bot" }
func (s *stubAnalyst) Category() AnalystCategory   { return s.category }
func (s *stubAnalyst) Persona() string             { return "" }
func (s *stubAnalyst) Analyze(ctx context.Context, in AnalystInput) (AnalystReport, error) {
	atomic.AddInt32(&s.calls, 1)
	if s.delay > 0 {
		select {
		case <-ctx.Done():
			return AnalystReport{}, ctx.Err()
		case <-time.After(s.delay):
		}
	}
	if s.err != nil {
		return AnalystReport{}, s.err
	}
	out := s.report
	out.Symbol = in.Symbol
	out.Category = s.category
	out.AsOf = in.AsOf
	out.GeneratedAt = testClock
	return out, nil
}

func newStub(id string, cat AnalystCategory, dir Direction, conf int) *stubAnalyst {
	return &stubAnalyst{
		id:       id,
		category: cat,
		report: AnalystReport{
			AgentID:     id,
			AgentName:   id + "-bot",
			Direction:   dir,
			Confidence:  conf,
			Thesis:      "stub thesis",
			KeyFindings: []string{"a"},
		},
	}
}

// --- Smoke tests ----------------------------------------------------------

func TestAnalystPanel_RunSymbol_HappyPath(t *testing.T) {
	stubs := []AnalystAgent{
		newStub("f", CategoryFundamentals, DirectionBullish, 80),
		newStub("s", CategorySentiment, DirectionBullish, 60),
		newStub("n", CategoryNews, DirectionNeutral, 40),
		newStub("t", CategoryTechnical, DirectionBullish, 70),
	}
	p := NewAnalystPanel("fund-1", stubs, WithPanelClock(fixedClock(testClock)), WithPanelSerial())
	got, err := p.RunSymbol(context.Background(), AnalystInput{Symbol: "AAPL", AsOf: testClock})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reports) != 4 {
		t.Errorf("len(Reports) = %d, want 4", len(got.Reports))
	}
	if got.Aggregate.Direction != DirectionBullish {
		t.Errorf("aggregate dir = %v, want bullish (3 of 4 bullish)", got.Aggregate.Direction)
	}
	if got.Aggregate.CategoriesVoted != 3 {
		t.Errorf("CategoriesVoted = %d, want 3", got.Aggregate.CategoriesVoted)
	}
	// 3 of 4 voted: no boost, no dampen → average of (80, 60, 40, 70) = 62.5 → 62
	if got.Aggregate.Confidence < 50 || got.Aggregate.Confidence > 80 {
		t.Errorf("aggregate confidence = %d, expected ~62", got.Aggregate.Confidence)
	}
	if got.Aggregate.PerCategoryVotes[CategoryFundamentals] != 1 {
		t.Errorf("vote map wrong: %v", got.Aggregate.PerCategoryVotes)
	}
}

func TestAnalystPanel_RunSymbol_AllAgree_BoostsConfidence(t *testing.T) {
	stubs := []AnalystAgent{
		newStub("f", CategoryFundamentals, DirectionBullish, 60),
		newStub("s", CategorySentiment, DirectionBullish, 60),
		newStub("n", CategoryNews, DirectionBullish, 60),
		newStub("t", CategoryTechnical, DirectionBullish, 60),
	}
	p := NewAnalystPanel("fund-1", stubs, WithPanelClock(fixedClock(testClock)), WithPanelSerial())
	got, _ := p.RunSymbol(context.Background(), AnalystInput{Symbol: "AAPL", AsOf: testClock})
	// All four agree → confidence boost of +10 → 70.
	if got.Aggregate.Confidence != 70 {
		t.Errorf("aggregate confidence = %d, want 70 (avg 60 + boost 10)", got.Aggregate.Confidence)
	}
	if got.Aggregate.CategoriesVoted != 4 {
		t.Errorf("CategoriesVoted = %d, want 4", got.Aggregate.CategoriesVoted)
	}
}

func TestAnalystPanel_RunSymbol_OneAnalystVoted_Dampened(t *testing.T) {
	stubs := []AnalystAgent{
		newStub("f", CategoryFundamentals, DirectionBullish, 80),
		newStub("s", CategorySentiment, DirectionNeutral, 30),
		newStub("n", CategoryNews, DirectionNeutral, 25),
		newStub("t", CategoryTechnical, DirectionNeutral, 25),
	}
	p := NewAnalystPanel("fund-1", stubs, WithPanelClock(fixedClock(testClock)), WithPanelSerial())
	got, _ := p.RunSymbol(context.Background(), AnalystInput{Symbol: "AAPL", AsOf: testClock})
	if got.Aggregate.CategoriesVoted != 1 {
		t.Errorf("CategoriesVoted = %d, want 1", got.Aggregate.CategoriesVoted)
	}
	// avg of (80, 30, 25, 25) = 40, dampen -10 = 30
	if got.Aggregate.Confidence > 40 {
		t.Errorf("aggregate confidence = %d, want dampened (<=40)", got.Aggregate.Confidence)
	}
}

func TestAnalystPanel_RunSymbol_ConflictingDirections_Neutral(t *testing.T) {
	stubs := []AnalystAgent{
		newStub("f", CategoryFundamentals, DirectionBullish, 80),
		newStub("s", CategorySentiment, DirectionBearish, 80),
		newStub("n", CategoryNews, DirectionBullish, 50),
		newStub("t", CategoryTechnical, DirectionBearish, 50),
	}
	p := NewAnalystPanel("fund-1", stubs, WithPanelClock(fixedClock(testClock)), WithPanelSerial())
	got, _ := p.RunSymbol(context.Background(), AnalystInput{Symbol: "AAPL", AsOf: testClock})
	if got.Aggregate.Direction != DirectionNeutral {
		t.Errorf("aggregate dir = %v, want neutral (50-50 conflict)", got.Aggregate.Direction)
	}
}

func TestAnalystPanel_RunSymbol_AnalystFailure_PanelStillReturns(t *testing.T) {
	failing := newStub("f", CategoryFundamentals, DirectionBullish, 80)
	failing.err = errors.New("data feed down")
	stubs := []AnalystAgent{
		failing,
		newStub("s", CategorySentiment, DirectionBullish, 60),
		newStub("n", CategoryNews, DirectionBullish, 60),
		newStub("t", CategoryTechnical, DirectionBullish, 60),
	}
	p := NewAnalystPanel("fund-1", stubs, WithPanelClock(fixedClock(testClock)), WithPanelSerial())
	got, err := p.RunSymbol(context.Background(), AnalystInput{Symbol: "AAPL", AsOf: testClock})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reports) != 3 {
		t.Errorf("len(Reports) = %d, want 3 (1 analyst failed)", len(got.Reports))
	}
	if _, ok := got.Reports[CategoryFundamentals]; ok {
		t.Errorf("failed analyst should not be in Reports")
	}
}

func TestAnalystPanel_RunSymbol_AllFail_Errors(t *testing.T) {
	f := newStub("f", CategoryFundamentals, DirectionBullish, 80)
	f.err = errors.New("x")
	s := newStub("s", CategorySentiment, DirectionBullish, 80)
	s.err = errors.New("y")
	p := NewAnalystPanel("fund-1", []AnalystAgent{f, s},
		WithPanelClock(fixedClock(testClock)), WithPanelSerial())
	if _, err := p.RunSymbol(context.Background(), AnalystInput{Symbol: "AAPL", AsOf: testClock}); err == nil {
		t.Error("expected error when every analyst fails")
	}
}

func TestAnalystPanel_RunSymbol_Parallel_AllCalled(t *testing.T) {
	stubs := []*stubAnalyst{
		newStub("f", CategoryFundamentals, DirectionBullish, 60),
		newStub("s", CategorySentiment, DirectionBullish, 60),
		newStub("n", CategoryNews, DirectionBullish, 60),
		newStub("t", CategoryTechnical, DirectionBullish, 60),
	}
	for _, s := range stubs {
		s.delay = 20 * time.Millisecond
	}
	agents := []AnalystAgent{stubs[0], stubs[1], stubs[2], stubs[3]}
	p := NewAnalystPanel("fund-1", agents, WithPanelClock(fixedClock(testClock)))

	start := time.Now()
	if _, err := p.RunSymbol(context.Background(), AnalystInput{Symbol: "AAPL", AsOf: testClock}); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	// 4 analysts × 20ms sequential = 80ms; parallel should be < 60ms.
	if elapsed > 60*time.Millisecond {
		t.Errorf("parallel run took %v; should be < 60ms", elapsed)
	}
	for _, s := range stubs {
		if atomic.LoadInt32(&s.calls) != 1 {
			t.Errorf("stub %s calls = %d, want 1", s.id, atomic.LoadInt32(&s.calls))
		}
	}
}

func TestAnalystPanel_RunSymbol_PerAnalystTimeout(t *testing.T) {
	slow := newStub("f", CategoryFundamentals, DirectionBullish, 80)
	slow.delay = 100 * time.Millisecond
	fast := newStub("s", CategorySentiment, DirectionBullish, 60)
	p := NewAnalystPanel("fund-1", []AnalystAgent{slow, fast},
		WithPanelClock(fixedClock(testClock)),
		WithPanelTimeout(20*time.Millisecond),
	)
	got, err := p.RunSymbol(context.Background(), AnalystInput{Symbol: "AAPL", AsOf: testClock})
	if err != nil {
		t.Fatalf("panel itself shouldn't fail when one analyst times out: %v", err)
	}
	if _, ok := got.Reports[CategoryFundamentals]; ok {
		t.Errorf("timed-out analyst should not appear in Reports")
	}
	if _, ok := got.Reports[CategorySentiment]; !ok {
		t.Errorf("fast analyst should be in Reports")
	}
}

func TestAnalystPanel_RunSymbol_NoSymbol(t *testing.T) {
	p := NewAnalystPanel("fund-1", []AnalystAgent{newStub("f", CategoryFundamentals, DirectionBullish, 60)})
	if _, err := p.RunSymbol(context.Background(), AnalystInput{}); err == nil {
		t.Error("expected error when input.Symbol empty")
	}
}

func TestAnalystPanel_RunSymbol_NoAnalysts(t *testing.T) {
	p := NewAnalystPanel("fund-1", nil)
	if _, err := p.RunSymbol(context.Background(), AnalystInput{Symbol: "X"}); err == nil {
		t.Error("expected error when panel has no analysts")
	}
}

func TestAnalystPanel_Categories_StableOrder(t *testing.T) {
	p := NewAnalystPanel("fund-1", []AnalystAgent{
		newStub("t", CategoryTechnical, DirectionBullish, 60),
		newStub("f", CategoryFundamentals, DirectionBullish, 60),
		newStub("n", CategoryNews, DirectionBullish, 60),
		newStub("s", CategorySentiment, DirectionBullish, 60),
	})
	got := p.Categories()
	// Sorted alphabetically: fundamentals, news, sentiment, technical
	want := []AnalystCategory{CategoryFundamentals, CategoryNews, CategorySentiment, CategoryTechnical}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("position %d: got %s, want %s", i, got[i], want[i])
		}
	}
}
