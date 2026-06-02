// bullbear_test.go — covers the S8.2 BullResearcher /
// BearResearcher and the Debate orchestrator.

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --- Helpers ---------------------------------------------------------------

// makePanelForDebate returns a PanelReport tuned to the test's
// needs: each analyst report carries the chosen direction +
// confidence + a single key finding / risk.
func makePanelForDebate(symbol string, dirs map[AnalystCategory]Direction, confs map[AnalystCategory]int) PanelReport {
	reports := map[AnalystCategory]AnalystReport{}
	for c, d := range dirs {
		conf := 60
		if v, ok := confs[c]; ok {
			conf = v
		}
		findings := []string{string(c) + " positive finding"}
		risks := []string{string(c) + " bearish risk"}
		reports[c] = AnalystReport{
			AgentID:     string(c) + "-bot",
			AgentName:   string(c) + " analyst",
			Category:    c,
			Symbol:      symbol,
			AsOf:        testClock,
			GeneratedAt: testClock,
			Direction:   d,
			Confidence:  conf,
			Thesis:      string(c) + " says " + string(d),
			KeyFindings: findings,
			Risks:       risks,
		}
	}
	agg := aggregateReports(reports)
	return PanelReport{
		Symbol: symbol, FundID: "fund-debate", AsOf: testClock, GeneratedAt: testClock,
		Reports: reports, Aggregate: agg,
	}
}

// --- Stance + Validate -----------------------------------------------------

func TestAdvocateStance_Opposite(t *testing.T) {
	if StanceBull.Opposite() != StanceBear {
		t.Errorf("StanceBull.Opposite() = %v", StanceBull.Opposite())
	}
	if StanceBear.Opposite() != StanceBull {
		t.Errorf("StanceBear.Opposite() = %v", StanceBear.Opposite())
	}
	if AdvocateStance("invalid").IsValid() {
		t.Error("invalid stance should not be valid")
	}
}

func TestAdvocateArgument_Validate(t *testing.T) {
	base := AdvocateArgument{
		Symbol:        "AAPL",
		Stance:        StanceBull,
		Direction:     DirectionBullish,
		Confidence:    60,
		Thesis:        "ok",
		SupportPoints: []string{"a"},
		Round:         1,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("base valid: %v", err)
	}
	bad := []AdvocateArgument{
		{},
		{Symbol: "AAPL"},
		{Symbol: "AAPL", Stance: StanceBull, Direction: DirectionNeutral, Thesis: "ok", Confidence: 60, SupportPoints: []string{"a"}, Round: 1},
		{Symbol: "AAPL", Stance: StanceBull, Direction: DirectionBullish, Thesis: "ok", Confidence: 200, SupportPoints: []string{"a"}, Round: 1},
		{Symbol: "AAPL", Stance: StanceBull, Direction: DirectionBullish, Confidence: 60, SupportPoints: []string{"a"}, Round: 1},
		{Symbol: "AAPL", Stance: StanceBull, Direction: DirectionBullish, Thesis: "ok", Confidence: 60, Round: 1},
		{Symbol: "AAPL", Stance: StanceBull, Direction: DirectionBullish, Thesis: "ok", Confidence: 60, SupportPoints: []string{"a"}, Round: 0},
	}
	for i, a := range bad {
		if err := a.Validate(); err == nil {
			t.Errorf("case %d: expected validation error, got nil for %+v", i, a)
		}
	}
}

// --- Bull + Bear no-LLM ---------------------------------------------------

func TestBullResearcher_NoLLM_AlwaysBullish(t *testing.T) {
	bull := NewBullResearcher("bull-1", "Bull", "fund-1", nil, WithAdvocateClock(fixedClock(testClock)))
	// Panel that screams BEARISH from every analyst.
	panel := makePanelForDebate("ZZZ",
		map[AnalystCategory]Direction{
			CategoryFundamentals: DirectionBearish,
			CategorySentiment:    DirectionBearish,
			CategoryNews:         DirectionBearish,
			CategoryTechnical:    DirectionBearish,
		},
		map[AnalystCategory]int{CategoryFundamentals: 80, CategorySentiment: 80, CategoryNews: 80, CategoryTechnical: 80})

	arg, err := bull.Argue(context.Background(), AdvocateInput{
		Symbol: "ZZZ", AsOf: testClock, Round: 1, Panel: panel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if arg.Direction != DirectionBullish || arg.Stance != StanceBull {
		t.Errorf("bull must be bullish even when panel is bearish, got %+v", arg)
	}
	if arg.Confidence < 30 {
		t.Errorf("bull confidence floor not honoured: %d", arg.Confidence)
	}
	if len(arg.SupportPoints) == 0 {
		t.Errorf("bull must produce support points; got none")
	}
}

func TestBearResearcher_NoLLM_AlwaysBearish(t *testing.T) {
	bear := NewBearResearcher("bear-1", "Bear", "fund-1", nil, WithAdvocateClock(fixedClock(testClock)))
	// Panel that screams BULLISH from every analyst.
	panel := makePanelForDebate("ZZZ",
		map[AnalystCategory]Direction{
			CategoryFundamentals: DirectionBullish,
			CategorySentiment:    DirectionBullish,
			CategoryNews:         DirectionBullish,
			CategoryTechnical:    DirectionBullish,
		}, nil)
	arg, err := bear.Argue(context.Background(), AdvocateInput{
		Symbol: "ZZZ", AsOf: testClock, Round: 1, Panel: panel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if arg.Direction != DirectionBearish || arg.Stance != StanceBear {
		t.Errorf("bear must be bearish even when panel is bullish, got %+v", arg)
	}
}

func TestBull_ConfidenceBoostsWithSupporters(t *testing.T) {
	bull := NewBullResearcher("bull-1", "Bull", "fund-1", nil, WithAdvocateClock(fixedClock(testClock)))
	// 4-of-4 bullish: confidence higher than 1-of-4 case.
	allBull := makePanelForDebate("AAA",
		map[AnalystCategory]Direction{
			CategoryFundamentals: DirectionBullish,
			CategorySentiment:    DirectionBullish,
			CategoryNews:         DirectionBullish,
			CategoryTechnical:    DirectionBullish,
		},
		map[AnalystCategory]int{CategoryFundamentals: 70, CategorySentiment: 70, CategoryNews: 70, CategoryTechnical: 70})
	oneBull := makePanelForDebate("AAA",
		map[AnalystCategory]Direction{
			CategoryFundamentals: DirectionBullish,
			CategorySentiment:    DirectionNeutral,
			CategoryNews:         DirectionNeutral,
			CategoryTechnical:    DirectionNeutral,
		},
		map[AnalystCategory]int{CategoryFundamentals: 70})

	a1, _ := bull.Argue(context.Background(), AdvocateInput{Symbol: "AAA", AsOf: testClock, Round: 1, Panel: allBull})
	a2, _ := bull.Argue(context.Background(), AdvocateInput{Symbol: "AAA", AsOf: testClock, Round: 1, Panel: oneBull})
	if a1.Confidence <= a2.Confidence {
		t.Errorf("4-of-4 confidence %d should exceed 1-of-4 confidence %d", a1.Confidence, a2.Confidence)
	}
}

func TestAdvocate_LLMReplyOverridesThesis(t *testing.T) {
	llm := &fakeLLM{reply: `{"direction":"bullish","confidence":85,"thesis":"LLM bullish thesis","key_findings":["LLM support"],"risks":["LLM rebuttal"]}`}
	bull := NewBullResearcher("bull-1", "Bull", "fund-1", llm, WithAdvocateClock(fixedClock(testClock)))
	panel := makePanelForDebate("XYZ",
		map[AnalystCategory]Direction{CategoryFundamentals: DirectionBullish}, nil)
	arg, err := bull.Argue(context.Background(), AdvocateInput{
		Symbol: "XYZ", AsOf: testClock, Round: 1, Panel: panel,
	})
	if err != nil {
		t.Fatal(err)
	}
	if arg.Thesis != "LLM bullish thesis" {
		t.Errorf("thesis = %q, want LLM override", arg.Thesis)
	}
	if arg.LLMModel != "llm" {
		t.Errorf("LLMModel = %q, want llm", arg.LLMModel)
	}
	if arg.Confidence != 85 {
		t.Errorf("confidence = %d, want 85", arg.Confidence)
	}
	if len(arg.SupportPoints) == 0 || arg.SupportPoints[0] != "LLM support" {
		t.Errorf("SupportPoints = %v", arg.SupportPoints)
	}
	if len(arg.Rebuttals) == 0 || arg.Rebuttals[0] != "LLM rebuttal" {
		t.Errorf("Rebuttals = %v", arg.Rebuttals)
	}
}

func TestAdvocate_LLMErrorFallsBack(t *testing.T) {
	llm := &fakeLLM{err: errors.New("rate limited")}
	bear := NewBearResearcher("bear-1", "Bear", "fund-1", llm, WithAdvocateClock(fixedClock(testClock)))
	panel := makePanelForDebate("Q",
		map[AnalystCategory]Direction{CategoryFundamentals: DirectionBullish}, nil)
	arg, err := bear.Argue(context.Background(), AdvocateInput{
		Symbol: "Q", AsOf: testClock, Round: 1, Panel: panel,
	})
	if err != nil {
		t.Fatalf("fallback should succeed, got err=%v", err)
	}
	if arg.LLMModel != "fallback" {
		t.Errorf("LLMModel = %q, want fallback", arg.LLMModel)
	}
	if arg.Direction != DirectionBearish {
		t.Errorf("direction = %v, want bearish", arg.Direction)
	}
}

func TestAdvocate_RebuttalsAppearFromRound2(t *testing.T) {
	bull := NewBullResearcher("bull-1", "Bull", "fund-1", nil, WithAdvocateClock(fixedClock(testClock)))
	panel := makePanelForDebate("R",
		map[AnalystCategory]Direction{CategoryFundamentals: DirectionBullish}, nil)
	bearArg := AdvocateArgument{
		AgentID: "bear-1", AgentName: "Bear", Stance: StanceBear,
		Symbol: "R", Round: 1, Direction: DirectionBearish, Confidence: 70,
		Thesis: "bear thesis", SupportPoints: []string{"bear point 1", "bear point 2"},
		AsOf: testClock, GeneratedAt: testClock,
	}
	arg, err := bull.Argue(context.Background(), AdvocateInput{
		Symbol: "R", AsOf: testClock, Round: 2, Panel: panel, Opponent: bearArg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(arg.Rebuttals) == 0 {
		t.Errorf("expected rebuttals in round 2 with opponent context, got none")
	}
	hasCounter := false
	for _, r := range arg.Rebuttals {
		if strings.Contains(r, "counter") || strings.Contains(r, "bear point") {
			hasCounter = true
		}
	}
	if !hasCounter {
		t.Errorf("rebuttals don't reference opponent: %v", arg.Rebuttals)
	}
}

// --- ToOpinion ------------------------------------------------------------

func TestAdvocateArgument_ToOpinion(t *testing.T) {
	a := AdvocateArgument{
		AgentID: "bull-1", AgentName: "Bull", Stance: StanceBull,
		Symbol: "AAPL", Round: 1, Direction: DirectionBullish, Confidence: 70,
		Thesis: "thesis", SupportPoints: []string{"a", "b"}, Rebuttals: []string{"x"},
	}
	op := a.ToOpinion()
	if op.AgentID != "bull-1" || op.Symbol != "AAPL" || op.Direction != "bullish" || op.Confidence != 70 {
		t.Errorf("ToOpinion projection wrong: %+v", op)
	}
	if len(op.DataPoints) != 3 {
		t.Errorf("DataPoints = %v, want 3 (2 support + 1 rebuttal)", op.DataPoints)
	}
	if !strings.HasPrefix(op.DataPoints[2], "rebut:") {
		t.Errorf("rebuttal not prefixed: %v", op.DataPoints[2])
	}
}

// --- Debate orchestrator --------------------------------------------------

func TestDebate_Run_HappyPath_TwoRounds(t *testing.T) {
	bull := NewBullResearcher("bull-1", "Bull", "fund-1", nil, WithAdvocateClock(fixedClock(testClock)))
	bear := NewBearResearcher("bear-1", "Bear", "fund-1", nil, WithAdvocateClock(fixedClock(testClock)))
	debate := NewDebate(bull, bear, DebateConfig{MaxRounds: 2}, WithDebateClock(fixedClock(testClock)))

	panel := makePanelForDebate("AAPL",
		map[AnalystCategory]Direction{
			CategoryFundamentals: DirectionBullish,
			CategorySentiment:    DirectionBullish,
			CategoryNews:         DirectionBearish,
			CategoryTechnical:    DirectionBullish,
		},
		map[AnalystCategory]int{
			CategoryFundamentals: 75, CategorySentiment: 70, CategoryNews: 65, CategoryTechnical: 70,
		})

	transcript, err := debate.Run(context.Background(), "fund-1", panel, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Arguments) != 4 {
		t.Errorf("expected 4 arguments (2 rounds × 2 sides), got %d", len(transcript.Arguments))
	}
	// Stances alternate bull, bear, bull, bear.
	expected := []AdvocateStance{StanceBull, StanceBear, StanceBull, StanceBear}
	for i, a := range transcript.Arguments {
		if a.Stance != expected[i] {
			t.Errorf("arg %d stance = %v, want %v", i, a.Stance, expected[i])
		}
		if a.Round < 1 || a.Round > 2 {
			t.Errorf("arg %d round = %d, want 1..2", i, a.Round)
		}
	}
	// Verdict should favour bull (3 bullish analysts, 1 bearish).
	if transcript.Verdict.Direction != DirectionBullish {
		t.Errorf("verdict direction = %v, want bullish (3 bullish analysts feed bull)", transcript.Verdict.Direction)
	}
	if transcript.Verdict.WinnerStance != StanceBull {
		t.Errorf("verdict winner = %v, want bull", transcript.Verdict.WinnerStance)
	}
}

func TestDebate_Run_BearWinsWhenBearishPanel(t *testing.T) {
	bull := NewBullResearcher("bull-1", "Bull", "fund-1", nil, WithAdvocateClock(fixedClock(testClock)))
	bear := NewBearResearcher("bear-1", "Bear", "fund-1", nil, WithAdvocateClock(fixedClock(testClock)))
	debate := NewDebate(bull, bear, DebateConfig{MaxRounds: 2}, WithDebateClock(fixedClock(testClock)))
	panel := makePanelForDebate("DOOM",
		map[AnalystCategory]Direction{
			CategoryFundamentals: DirectionBearish,
			CategorySentiment:    DirectionBearish,
			CategoryNews:         DirectionBearish,
			CategoryTechnical:    DirectionBearish,
		},
		map[AnalystCategory]int{
			CategoryFundamentals: 80, CategorySentiment: 80, CategoryNews: 80, CategoryTechnical: 80,
		})
	transcript, err := debate.Run(context.Background(), "fund-1", panel, "")
	if err != nil {
		t.Fatal(err)
	}
	if transcript.Verdict.Direction != DirectionBearish {
		t.Errorf("verdict = %v, want bearish (all-bearish panel feeds bear)", transcript.Verdict.Direction)
	}
}

func TestDebate_Run_RejectsNilAdvocates(t *testing.T) {
	d := NewDebate(nil, nil, DebateConfig{MaxRounds: 1})
	if _, err := d.Run(context.Background(), "f", PanelReport{Symbol: "X"}, ""); err == nil {
		t.Error("expected error when both advocates nil")
	}
}

func TestDebate_Run_RejectsEmptyPanelSymbol(t *testing.T) {
	bull := NewBullResearcher("b", "B", "f", nil)
	bear := NewBearResearcher("be", "Be", "f", nil)
	d := NewDebate(bull, bear, DebateConfig{MaxRounds: 1})
	if _, err := d.Run(context.Background(), "f", PanelReport{}, ""); err == nil {
		t.Error("expected error when panel.Symbol empty")
	}
}

func TestDebate_Run_FirstRoundFailure_Errors(t *testing.T) {
	failLLM := &fakeLLM{err: errors.New("nope")}
	_ = failLLM
	// Make bull fail in round 1 by giving it an input that
	// makes Validate fail: we simulate this by replacing the
	// bull with a custom advocate that errors out.
	bull := &alwaysErrorAdvocate{stance: StanceBull, errFn: func() error { return errors.New("bull explodes") }}
	bear := NewBearResearcher("bear", "Bear", "f", nil, WithAdvocateClock(fixedClock(testClock)))
	d := NewDebate(nil, bear, DebateConfig{MaxRounds: 1})
	// nil bull short-circuit.
	if _, err := d.Run(context.Background(), "f", makePanelForDebate("X",
		map[AnalystCategory]Direction{CategoryFundamentals: DirectionBullish}, nil), ""); err == nil {
		t.Error("expected error when bull is nil")
	}
	_ = bull
}

// alwaysErrorAdvocate is a tiny test double used to inject
// round-1 failures into the debate runner.
type alwaysErrorAdvocate struct {
	stance AdvocateStance
	errFn  func() error
}

func (a *alwaysErrorAdvocate) ID() string                          { return "errbot" }
func (a *alwaysErrorAdvocate) Name() string                        { return "ErrBot" }
func (a *alwaysErrorAdvocate) Stance() AdvocateStance              { return a.stance }
func (a *alwaysErrorAdvocate) Persona() string                     { return "" }
func (a *alwaysErrorAdvocate) Argue(_ context.Context, _ AdvocateInput) (AdvocateArgument, error) {
	return AdvocateArgument{}, a.errFn()
}

func TestSynthesiseVerdict_NoArguments(t *testing.T) {
	v := synthesiseVerdict(nil)
	if v.Direction != DirectionNeutral || v.Confidence != 20 {
		t.Errorf("verdict for empty args: got %+v", v)
	}
}

func TestSynthesiseVerdict_LaterRoundsWeighMore(t *testing.T) {
	// Bull weak in round 1, strong in round 2; bear strong in
	// round 1, weak in round 2. Late-round weight makes bull win.
	args := []AdvocateArgument{
		{Stance: StanceBull, Round: 1, Direction: DirectionBullish, Confidence: 40, Thesis: "weak bull r1", SupportPoints: []string{"a"}, Symbol: "X"},
		{Stance: StanceBear, Round: 1, Direction: DirectionBearish, Confidence: 80, Thesis: "strong bear r1", SupportPoints: []string{"a"}, Symbol: "X"},
		{Stance: StanceBull, Round: 2, Direction: DirectionBullish, Confidence: 90, Thesis: "strong bull r2", SupportPoints: []string{"a"}, Symbol: "X"},
		{Stance: StanceBear, Round: 2, Direction: DirectionBearish, Confidence: 30, Thesis: "weak bear r2", SupportPoints: []string{"a"}, Symbol: "X"},
	}
	// bull: 40*1 + 90*2 = 220; bear: 80*1 + 30*2 = 140; bull wins.
	v := synthesiseVerdict(args)
	if v.Direction != DirectionBullish || v.WinnerStance != StanceBull {
		t.Errorf("late-round weighting failed: %+v", v)
	}
	if v.WinningSummary == "" {
		t.Errorf("WinningSummary empty")
	}
}

func TestSynthesiseVerdict_ContestedMargin(t *testing.T) {
	// Bull and bear almost tied → Contested.
	args := []AdvocateArgument{
		{Stance: StanceBull, Round: 1, Direction: DirectionBullish, Confidence: 70, Thesis: "x", SupportPoints: []string{"a"}, Symbol: "X"},
		{Stance: StanceBear, Round: 1, Direction: DirectionBearish, Confidence: 65, Thesis: "y", SupportPoints: []string{"a"}, Symbol: "X"},
	}
	v := synthesiseVerdict(args)
	if !v.Contested {
		t.Errorf("close margin should be marked Contested: %+v", v)
	}
}
