package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/attribution"
	"github.com/fundai/server/internal/repository"
)

// scorecardLotStub satisfies attribution.LotStatsRepo with hand-
// crafted SleeveRegimeStat rows so the wiring test can check
// exactly which stats appear in the prompt summary. Empty struct
// returns empty slices for every method which is the "no closed
// lots yet" code path.
type scorecardLotStub struct {
	sleeve       []repository.SleeveStat
	regime       []repository.RegimeStat
	sleeveRegime []repository.SleeveRegimeStat
}

func (s *scorecardLotStub) StatsBySleeve(_ context.Context, _ string, _ time.Time) ([]repository.SleeveStat, error) {
	return s.sleeve, nil
}
func (s *scorecardLotStub) StatsByRegime(_ context.Context, _ string, _ time.Time) ([]repository.RegimeStat, error) {
	return s.regime, nil
}
func (s *scorecardLotStub) StatsBySleeveRegime(_ context.Context, _ string, _ time.Time) ([]repository.SleeveRegimeStat, error) {
	return s.sleeveRegime, nil
}
func (s *scorecardLotStub) OpenLotInventory(_ context.Context, _ string) (int, sql.NullTime, error) {
	return 0, sql.NullTime{}, nil
}

// scorecardMemoryStub stubs attribution.MemoryWriter; the
// scorecard path only reads, so Create/ListByFund are trivial.
type scorecardMemoryStub struct{}

func (m *scorecardMemoryStub) Create(_ context.Context, _ *repository.Memory) (string, error) {
	return "", nil
}
func (m *scorecardMemoryStub) ListByFund(_ context.Context, _ string, _ string, _ int) ([]repository.Memory, error) {
	return nil, nil
}

// TestBuildSleeveScorecardWithNilAttributionReturnsEmpty covers the
// legacy / smoke-build path. The runtimePMAgent without a wired
// attribution service must produce an empty scorecard rather than
// crash — the LLM prompt path is on the daily critical workflow.
func TestBuildSleeveScorecardWithNilAttributionReturnsEmpty(t *testing.T) {
	agent := &runtimePMAgent{}
	if got := agent.buildSleeveScorecard(context.Background(), "fund-x"); got != "" {
		t.Fatalf("expected empty scorecard on nil attribution, got %q", got)
	}
}

// TestBuildSleeveScorecardEmptyOnNoClosedLots: a fresh fund with no
// closed lots must not produce a scorecard. The prompt path will
// then omit the section entirely, mirroring the "no data, no
// noise" rule of the prompt builder.
func TestBuildSleeveScorecardEmptyOnNoClosedLots(t *testing.T) {
	svc := attribution.NewService(&scorecardLotStub{}, &scorecardMemoryStub{})
	agent := &runtimePMAgent{attribution: svc}
	if got := agent.buildSleeveScorecard(context.Background(), "fund-x"); got != "" {
		t.Fatalf("expected empty scorecard when no closed lots, got %q", got)
	}
}

// TestBuildSleeveScorecardRendersTopWinnersAndLosers exercises the
// happy path: closed lots populate the cross-tab, the prompt
// builder filters by sample size, and the wiring helper hands a
// non-empty markdown block to the LLM prompt assembler.
func TestBuildSleeveScorecardRendersTopWinnersAndLosers(t *testing.T) {
	stub := &scorecardLotStub{
		sleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "trend_up", TradeCount: 12, WinRate: 0.75, TotalPnL: 1850.20, AvgPnLPct: 0.052},
			{Sleeve: "mean_reversion", Regime: "chop", TradeCount: 7, WinRate: 0.22, TotalPnL: -820.10, AvgPnLPct: -0.043},
			{Sleeve: "trend", Regime: "range", TradeCount: 2, WinRate: 0.5, TotalPnL: 50, AvgPnLPct: 0.01}, // below min sample — dropped
		},
	}
	svc := attribution.NewService(stub, &scorecardMemoryStub{})
	agent := &runtimePMAgent{attribution: svc}
	out := agent.buildSleeveScorecard(context.Background(), "fund-x")
	if out == "" {
		t.Fatal("expected non-empty scorecard")
	}
	for _, want := range []string{
		"Strategy scorecard",
		"Winners",
		"Losers",
		"trend regime=trend_up",
		"mean_reversion regime=chop",
		"$+1850.20",
		"$-820.10",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scorecard missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "regime=range") {
		t.Fatalf("under-sampled row should be filtered, got:\n%s", out)
	}
}
