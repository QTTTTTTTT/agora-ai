package consistency

import (
	"strings"
	"testing"
)

func TestJaccardIdenticalIsOne(t *testing.T) {
	a := []Trade{{Symbol: "AAPL", Direction: "buy"}, {Symbol: "MSFT", Direction: "sell"}}
	b := []Trade{{Symbol: "AAPL", Direction: "buy"}, {Symbol: "MSFT", Direction: "sell"}}
	if got := JaccardOf(a, b); got != 1.0 {
		t.Errorf("Jaccard identical: got %v, want 1.0", got)
	}
}

func TestJaccardDisjointIsZero(t *testing.T) {
	a := []Trade{{Symbol: "AAPL", Direction: "buy"}}
	b := []Trade{{Symbol: "TSLA", Direction: "buy"}}
	if got := JaccardOf(a, b); got != 0 {
		t.Errorf("Jaccard disjoint: got %v, want 0", got)
	}
}

func TestJaccardCaseAndWhitespaceInsensitive(t *testing.T) {
	a := []Trade{{Symbol: " aapl ", Direction: "Buy"}}
	b := []Trade{{Symbol: "AAPL", Direction: "buy"}}
	if got := JaccardOf(a, b); got != 1.0 {
		t.Errorf("Jaccard normalisation: got %v, want 1.0", got)
	}
}

func TestJaccardBothEmptyIsOne(t *testing.T) {
	if got := JaccardOf(nil, nil); got != 1.0 {
		t.Errorf("Jaccard empty/empty: got %v, want 1.0", got)
	}
}

func TestJaccardOneEmptyIsZero(t *testing.T) {
	if got := JaccardOf(nil, []Trade{{Symbol: "X", Direction: "buy"}}); got != 0 {
		t.Errorf("Jaccard empty/non-empty: got %v, want 0", got)
	}
}

func TestJaccardDirectionMismatchCounted(t *testing.T) {
	a := []Trade{{Symbol: "AAPL", Direction: "buy"}}
	b := []Trade{{Symbol: "AAPL", Direction: "sell"}}
	if got := JaccardOf(a, b); got != 0 {
		t.Errorf("direction mismatch should miss: got %v, want 0", got)
	}
}

func TestCompareReportsMedianAndMin(t *testing.T) {
	runs := []Run{
		{Index: 1, Trades: []Trade{{Symbol: "AAPL", Direction: "buy"}, {Symbol: "MSFT", Direction: "buy"}}},
		{Index: 2, Trades: []Trade{{Symbol: "AAPL", Direction: "buy"}, {Symbol: "MSFT", Direction: "buy"}}},
		{Index: 3, Trades: []Trade{{Symbol: "AAPL", Direction: "buy"}}},
	}
	res := Compare(runs)
	if res.RunCount != 3 {
		t.Errorf("RunCount: got %d, want 3", res.RunCount)
	}
	if len(res.Pairs) != 3 {
		t.Errorf("Pairs: got %d, want 3", len(res.Pairs))
	}
	if res.MedianJaccard <= 0 {
		t.Errorf("MedianJaccard: got %v, want > 0", res.MedianJaccard)
	}
	if res.MinJaccard >= res.MedianJaccard && res.MinJaccard != res.MedianJaccard {
		t.Errorf("Min should be <= Median: min=%v median=%v",
			res.MinJaccard, res.MedianJaccard)
	}
}

func TestAssertPassesAboveFloor(t *testing.T) {
	runs := []Run{
		{Index: 1, Trades: []Trade{{Symbol: "AAPL", Direction: "buy"}}},
		{Index: 2, Trades: []Trade{{Symbol: "AAPL", Direction: "buy"}}},
	}
	res := Compare(runs)
	if err := res.Assert(0.8, 0); err != nil {
		t.Errorf("Assert should pass: %v", err)
	}
}

func TestAssertFailsBelowFloor(t *testing.T) {
	runs := []Run{
		{Index: 1, Trades: []Trade{{Symbol: "AAPL", Direction: "buy"}}},
		{Index: 2, Trades: []Trade{{Symbol: "TSLA", Direction: "buy"}}},
	}
	res := Compare(runs)
	err := res.Assert(0.8, 0)
	if err == nil {
		t.Fatalf("Assert should fail at jaccard=0")
	}
	if !strings.Contains(err.Error(), "median jaccard") {
		t.Errorf("error should mention jaccard: %v", err)
	}
}

func TestAssertFailsOnWeightDrift(t *testing.T) {
	runs := []Run{
		{Index: 1, Trades: []Trade{{Symbol: "AAPL", Direction: "buy", Weight: 0.05}}},
		{Index: 2, Trades: []Trade{{Symbol: "AAPL", Direction: "buy", Weight: 0.20}}},
	}
	res := Compare(runs)
	err := res.Assert(0.5, 0.1)
	if err == nil {
		t.Fatalf("Assert should fail on weight drift")
	}
	if !strings.Contains(err.Error(), "weight drift") {
		t.Errorf("error should mention weight drift: %v", err)
	}
}

func TestWeightDriftReportedSeparately(t *testing.T) {
	runs := []Run{
		{Index: 1, Trades: []Trade{{Symbol: "AAPL", Direction: "buy", Weight: 0.05, Confidence: 0.8}}},
		{Index: 2, Trades: []Trade{{Symbol: "AAPL", Direction: "buy", Weight: 0.07, Confidence: 0.85}}},
	}
	res := Compare(runs)
	if res.MedianJaccard != 1.0 {
		t.Errorf("MedianJaccard should be 1 (same trades): got %v", res.MedianJaccard)
	}
	if res.WeightDriftMax <= 0 {
		t.Errorf("WeightDriftMax should be > 0: got %v", res.WeightDriftMax)
	}
	if res.ConfDriftMax <= 0 {
		t.Errorf("ConfDriftMax should be > 0: got %v", res.ConfDriftMax)
	}
}

func TestSingleRunIsTrivialPass(t *testing.T) {
	res := Compare([]Run{{Index: 1, Trades: []Trade{{Symbol: "AAPL", Direction: "buy"}}}})
	if res.MedianJaccard != 1.0 {
		t.Errorf("single run: got %v, want 1.0", res.MedianJaccard)
	}
	if err := res.Assert(0.8, 0); err == nil {
		t.Errorf("Assert should fail on RunCount<2")
	}
}

func TestMedianPairwiseConvenience(t *testing.T) {
	runs := []Run{
		{Index: 1, Trades: []Trade{{Symbol: "A", Direction: "buy"}}},
		{Index: 2, Trades: []Trade{{Symbol: "A", Direction: "buy"}}},
		{Index: 3, Trades: []Trade{{Symbol: "A", Direction: "buy"}}},
	}
	if got := MedianPairwise(runs); got != 1.0 {
		t.Errorf("MedianPairwise: got %v, want 1.0", got)
	}
}

func TestDeterministicAcrossInvocations(t *testing.T) {
	runs := []Run{
		{Index: 1, Trades: []Trade{{Symbol: "A", Direction: "buy", Weight: 0.05}, {Symbol: "B", Direction: "sell", Weight: 0.03}}},
		{Index: 2, Trades: []Trade{{Symbol: "A", Direction: "buy", Weight: 0.06}, {Symbol: "C", Direction: "sell", Weight: 0.04}}},
		{Index: 3, Trades: []Trade{{Symbol: "A", Direction: "buy", Weight: 0.05}}},
	}
	a := Compare(runs)
	b := Compare(runs)
	if a.MedianJaccard != b.MedianJaccard {
		t.Errorf("non-deterministic: %v vs %v", a.MedianJaccard, b.MedianJaccard)
	}
}
