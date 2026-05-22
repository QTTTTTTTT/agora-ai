package sizing

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// buildBars constructs a synthetic OHLC series with a known
// daily range so the unit tests can reason about ATR
// deterministically. range = high - low = 2.0 (constant), and
// closes drift up by 0.1 per bar so volatility is essentially
// just the intraday range (TR ≈ 2.0 every bar).
func buildBars(n int) []ohlc.Bar {
	bars := make([]ohlc.Bar, n)
	base := 100.0
	for i := 0; i < n; i++ {
		mid := base + float64(i)*0.1
		bars[i] = ohlc.Bar{
			Time:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * 24 * time.Hour),
			Open:   mid,
			High:   mid + 1.0,
			Low:    mid - 1.0,
			Close:  mid,
			Volume: 1000,
		}
	}
	return bars
}

func TestSizeReturnsAppliedFalseWhenDisabled(t *testing.T) {
	r := Size(Policy{Enabled: false}, Input{NAV: 100_000, Price: 50, Bars: buildBars(20)})
	if r.Applied {
		t.Fatalf("disabled policy should not produce sizing; got %+v", r)
	}
	if !strings.Contains(r.Reason, "disabled") {
		t.Fatalf("reason should mention disabled, got %q", r.Reason)
	}
}

func TestSizeReturnsAppliedFalseWhenNAVZero(t *testing.T) {
	r := Size(Policy{Enabled: true}, Input{NAV: 0, Price: 50, Bars: buildBars(20)})
	if r.Applied {
		t.Fatal("should skip when NAV <= 0")
	}
}

func TestSizeReturnsAppliedFalseWhenPriceZero(t *testing.T) {
	r := Size(Policy{Enabled: true}, Input{NAV: 100_000, Price: 0, Bars: buildBars(20)})
	if r.Applied {
		t.Fatal("should skip when price <= 0")
	}
}

func TestSizeReturnsAppliedFalseWhenInsufficientBars(t *testing.T) {
	// ATR(14) requires 15 bars; provide 10.
	r := Size(Policy{Enabled: true}, Input{NAV: 100_000, Price: 50, Bars: buildBars(10)})
	if r.Applied {
		t.Fatal("should skip when bar history is too short for ATR")
	}
	if !strings.Contains(r.Reason, "ATR(14)") {
		t.Fatalf("reason should mention ATR period, got %q", r.Reason)
	}
}

func TestSizeUsesATRStopWhenNoSleeveHint(t *testing.T) {
	bars := buildBars(40)
	// Expected: ATR ≈ 2.0 (constant TR), so risk_per_share =
	// 2.0 * 2.0 = 4.0. NAV * 0.5% = 500. qty = 500/4 = 125.
	// Notional = 125 * 50 = 6250; cap = 100_000 * 0.10 = 10_000
	// → not clipped.
	r := Size(Policy{Enabled: true}, Input{NAV: 100_000, Price: 50, Bars: bars})
	if !r.Applied {
		t.Fatalf("expected applied=true, got %+v", r)
	}
	if math.Abs(r.Quantity-125) > 1 {
		t.Fatalf("expected qty ≈ 125, got %.4f", r.Quantity)
	}
	expectedStop := 50 - 2*2.0
	if math.Abs(r.StopPrice-expectedStop) > 0.01 {
		t.Fatalf("expected stop ≈ %.2f, got %.4f", expectedStop, r.StopPrice)
	}
	if math.Abs(r.ATR-2.0) > 0.05 {
		t.Fatalf("expected ATR ≈ 2.0, got %.4f", r.ATR)
	}
	if !strings.Contains(r.Reason, "from-ATR") {
		t.Fatalf("expected reason to mention ATR-derived stop, got %q", r.Reason)
	}
}

func TestSizePrefersSleeveStopWhenProvided(t *testing.T) {
	bars := buildBars(40)
	// Sleeve stop is at 48 (2 below entry of 50) → risk = 2.
	// qty = 500 / 2 = 250; notional = 250 * 50 = 12500; cap
	// = 10000 → clipped down to 10000/50 = 200.
	r := Size(Policy{Enabled: true}, Input{
		NAV:          100_000,
		Price:        50,
		Bars:         bars,
		ExistingStop: 48,
	})
	if !r.Applied {
		t.Fatalf("expected applied, got %+v", r)
	}
	if r.Quantity != 200 {
		t.Fatalf("expected qty = 200 (cap-clipped), got %.4f", r.Quantity)
	}
	if math.Abs(r.StopPrice-48) > 0.0001 {
		t.Fatalf("expected stop preserved at 48, got %.4f", r.StopPrice)
	}
	if !strings.Contains(r.Reason, "from-sleeve") {
		t.Fatalf("expected reason to mention sleeve stop, got %q", r.Reason)
	}
	if !strings.Contains(r.Reason, "clipped") {
		t.Fatalf("expected reason to mention notional clip, got %q", r.Reason)
	}
}

func TestSizeClampsTooTightSleeveStop(t *testing.T) {
	bars := buildBars(40)
	// ATR ≈ 2.0 → min risk = 1.0. Sleeve gives stop at 49.5
	// (only 0.5 below price) → sleeveRisk = 0.5 < 1.0 → clamp
	// to 1.0. Expected: risk_per_share = 1, stop = 49,
	// raw qty = 500, notional = 25000, cap = 10000 → qty = 200.
	r := Size(Policy{Enabled: true}, Input{
		NAV:          100_000,
		Price:        50,
		Bars:         bars,
		ExistingStop: 49.5,
	})
	if !r.Applied {
		t.Fatalf("expected applied, got %+v", r)
	}
	if math.Abs(r.StopPrice-49) > 0.05 {
		t.Fatalf("expected clamped stop ≈ 49, got %.4f", r.StopPrice)
	}
	if !strings.Contains(r.Reason, "clamped") {
		t.Fatalf("expected reason to flag clamp, got %q", r.Reason)
	}
}

func TestSizeIgnoresSleeveStopAboveEntry(t *testing.T) {
	// A sleeve mistakenly returning stop > entry (buy below
	// stop, makes no sense for a long) should be ignored —
	// sizer falls back to ATR.
	bars := buildBars(40)
	r := Size(Policy{Enabled: true}, Input{
		NAV:          100_000,
		Price:        50,
		Bars:         bars,
		ExistingStop: 52,
	})
	if !r.Applied {
		t.Fatalf("expected applied, got %+v", r)
	}
	if !strings.Contains(r.Reason, "from-ATR") {
		t.Fatalf("expected ATR fallback when sleeve stop is above entry, got %q", r.Reason)
	}
}

func TestSizeClipsNotionalAboveCap(t *testing.T) {
	// Force a runaway quantity by pumping NAV; without the
	// cap qty would land far above the 10% notional limit.
	bars := buildBars(40)
	r := Size(Policy{Enabled: true, MaxNotionalPctOfNAV: 0.05}, Input{NAV: 1_000_000, Price: 50, Bars: bars})
	maxNotional := 1_000_000 * 0.05
	if r.Quantity*50 > maxNotional+0.01 {
		t.Fatalf("quantity*price = %.2f exceeds 5%% NAV cap %.2f", r.Quantity*50, maxNotional)
	}
	if !strings.Contains(r.Reason, "clipped") {
		t.Fatalf("expected clip mention, got %q", r.Reason)
	}
}

func TestSizeRoundsQuantityDown(t *testing.T) {
	// Choose inputs that produce a non-integer raw qty
	// (e.g. NAV=10_000, risk_pct=0.005, risk_per_share=4
	// → raw = 12.5 → floor = 12).
	bars := buildBars(40)
	r := Size(Policy{Enabled: true}, Input{NAV: 10_000, Price: 50, Bars: bars})
	if !r.Applied {
		t.Fatalf("expected applied, got %+v", r)
	}
	if r.Quantity != 12 {
		t.Fatalf("expected floor(12.5)=12, got %.4f", r.Quantity)
	}
}

func TestSizeReturnsAppliedFalseWhenQuantityRoundsToZero(t *testing.T) {
	bars := buildBars(40)
	// NAV=100, risk_pct=0.005 → budget=0.5. risk_per_share=4
	// → raw qty 0.125 → floor 0 → not applied.
	r := Size(Policy{Enabled: true}, Input{NAV: 100, Price: 50, Bars: bars})
	if r.Applied {
		t.Fatalf("expected applied=false for sub-share budget, got %+v", r)
	}
	if !strings.Contains(r.Reason, "rounds to 0 shares") {
		t.Fatalf("reason should mention rounding to zero, got %q", r.Reason)
	}
}

func TestSizeIsAllocationFreeReason(t *testing.T) {
	// Sanity check: every disable path should produce a
	// non-empty Reason so the wiring layer can log it.
	cases := []Result{
		Size(Policy{}, Input{}),
		Size(Policy{Enabled: true}, Input{NAV: -1}),
		Size(Policy{Enabled: true}, Input{NAV: 1, Price: -1}),
		Size(Policy{Enabled: true}, Input{NAV: 1, Price: 1}),
	}
	for i, r := range cases {
		if r.Reason == "" {
			t.Fatalf("case %d: expected non-empty Reason", i)
		}
	}
}
