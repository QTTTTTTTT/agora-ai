package intraday

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

type stubFetcher struct {
	bars map[string][]ohlc.Bar
	err  error
}

func (s *stubFetcher) Fetch(ctx context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	if s.err != nil {
		return nil, s.err
	}
	b, ok := s.bars[req.Symbol]
	if !ok {
		return nil, ohlc.ErrNoData
	}
	return b, nil
}

func makeBars(closes []float64, vols []float64) []ohlc.Bar {
	out := make([]ohlc.Bar, len(closes))
	t0 := time.Date(2026, 5, 27, 9, 30, 0, 0, time.UTC)
	for i, c := range closes {
		v := 0.0
		if i < len(vols) {
			v = vols[i]
		}
		out[i] = ohlc.Bar{
			Time:   t0.Add(time.Duration(i) * 5 * time.Minute),
			Open:   c * 0.999,
			High:   c * 1.005,
			Low:    c * 0.995,
			Close:  c,
			Volume: v,
		}
	}
	return out
}

func TestBuilderEmptyInputs(t *testing.T) {
	b := NewBuilder(nil, Interval5m)
	if got := b.Build(context.Background(), []string{"AAPL"}, "us_equity", time.Now()); got != nil {
		t.Errorf("nil fetcher should produce nil snapshots, got %v", got)
	}
	b2 := NewBuilder(&stubFetcher{bars: map[string][]ohlc.Bar{}}, Interval5m)
	if got := b2.Build(context.Background(), nil, "us_equity", time.Now()); got != nil {
		t.Errorf("nil symbols should produce nil, got %v", got)
	}
}

func TestBuilderSkipsSymbolsWithFewBars(t *testing.T) {
	bars := makeBars([]float64{100, 101, 102}, []float64{1000, 1000, 1000})
	b := NewBuilder(&stubFetcher{bars: map[string][]ohlc.Bar{"AAPL": bars}}, Interval5m)
	if got := b.Build(context.Background(), []string{"AAPL"}, "us_equity", time.Now()); len(got) != 0 {
		t.Errorf("expected 0 snapshots (only 3 bars), got %d", len(got))
	}
}

func TestBuilderClassifiesTrendUp(t *testing.T) {
	closes := []float64{100, 100.5, 101, 101.5, 102, 102.5, 103, 103.5, 104, 104.5}
	vols := []float64{1000, 900, 1100, 1200, 800, 1300, 950, 1050, 1100, 1500}
	bars := makeBars(closes, vols)
	b := NewBuilder(&stubFetcher{bars: map[string][]ohlc.Bar{"AAPL": bars}}, Interval5m)
	snaps := b.Build(context.Background(), []string{"AAPL"}, "us_equity", time.Now())
	if len(snaps) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(snaps))
	}
	if snaps[0].TrendDirection != "up" {
		t.Errorf("trend direction: want up, got %s", snaps[0].TrendDirection)
	}
	if snaps[0].LastClose != 104.5 {
		t.Errorf("last close: want 104.5, got %f", snaps[0].LastClose)
	}
}

func TestBuilderClassifiesTrendDown(t *testing.T) {
	closes := []float64{100, 99.5, 99, 98.5, 98, 97.5, 97, 96.5, 96, 95.5}
	vols := []float64{1000, 900, 1100, 1200, 800, 1300, 950, 1050, 1100, 1500}
	bars := makeBars(closes, vols)
	b := NewBuilder(&stubFetcher{bars: map[string][]ohlc.Bar{"AAPL": bars}}, Interval5m)
	snaps := b.Build(context.Background(), []string{"AAPL"}, "us_equity", time.Now())
	if len(snaps) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(snaps))
	}
	if snaps[0].TrendDirection != "down" {
		t.Errorf("trend direction: want down, got %s", snaps[0].TrendDirection)
	}
}

func TestBuilderSortsSymbolsAlphabetically(t *testing.T) {
	bars := makeBars(
		[]float64{100, 100, 100, 100, 100, 100, 100, 100, 100, 100},
		[]float64{1000, 1000, 1000, 1000, 1000, 1000, 1000, 1000, 1000, 1000},
	)
	b := NewBuilder(&stubFetcher{bars: map[string][]ohlc.Bar{
		"BBBB": bars, "AAAA": bars, "CCCC": bars,
	}}, Interval5m)
	snaps := b.Build(context.Background(), []string{"BBBB", "AAAA", "CCCC"}, "us_equity", time.Now())
	if len(snaps) != 3 {
		t.Fatalf("want 3 snapshots, got %d", len(snaps))
	}
	if snaps[0].Symbol != "AAAA" || snaps[1].Symbol != "BBBB" || snaps[2].Symbol != "CCCC" {
		t.Errorf("not alphabetically sorted: %v", snaps)
	}
}

func TestBuilderTolerantToFetchErrors(t *testing.T) {
	b := NewBuilder(&stubFetcher{err: errors.New("upstream down")}, Interval5m)
	snaps := b.Build(context.Background(), []string{"X"}, "us_equity", time.Now())
	if len(snaps) != 0 {
		t.Errorf("want 0 snapshots on error, got %d", len(snaps))
	}
}
