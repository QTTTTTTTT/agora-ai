package strategy

import (
	"fmt"
	"math"

	"github.com/fundai/server/internal/indicator"
	"github.com/fundai/server/internal/regime"
)

// ---------------------------------------------------------------------------
// Mean-reversion sleeve
// ---------------------------------------------------------------------------
//
// Signal: RSI extreme confirmed by Bollinger band touch.
//
//   LONG  (buy the dip) fires when:
//     close[t]   < lower_bb[t]    (price below 2σ band)
//     AND rsi[t] < rsiOversold    (default 30)
//
//   SHORT (sell into strength, on existing long) fires when:
//     close[t]   > upper_bb[t]
//     AND rsi[t] > rsiOverbought  (default 70)
//
// Regime gating: the sleeve ONLY fires in regime=range. This is
// the most important guard — mean reversion in regime=chop is
// the textbook recipe for catching falling knives, and in
// regime=trend_* it bucks the dominant flow.
//
// Confidence model: how extreme is the RSI? RSI=30 → 0.55 (just
// at the threshold), RSI=10 → 0.95 (saturated). Symmetric for
// the overbought side.

const (
	meanRevSignalSource = "rsi_bb_14_20"
)

// MeanReversionSleeve implements the Sleeve interface.
type MeanReversionSleeve struct {
	params MeanReversionParams
}

// NewMeanReversionSleeve builds a MeanReversionSleeve with the
// supplied params. Like NewTrendSleeve, zero values are NOT
// back-filled — caller passes an EffectivePolicy-normalised
// MeanReversionParams or defaultMeanReversion().
func NewMeanReversionSleeve(params MeanReversionParams) *MeanReversionSleeve {
	return &MeanReversionSleeve{params: params}
}

// Name implements Sleeve.
func (m *MeanReversionSleeve) Name() string { return "mean_reversion" }

// PreferredRegimes restricts the sleeve to range markets. The
// gate is deliberately narrower than trend (which fires in two
// regimes) because mean reversion's failure modes are wider.
func (m *MeanReversionSleeve) PreferredRegimes() []regime.Regime {
	return []regime.Regime{regime.Range}
}

// Evaluate implements Sleeve. Returns nil for "no opinion".
func (m *MeanReversionSleeve) Evaluate(b Bundle) *Proposal {
	p := m.params
	need := p.BBPeriod
	if p.RSIPeriod+1 > need {
		need = p.RSIPeriod + 1
	}
	// Add a small buffer so the SMA inside BB has stabilised.
	need += 5
	if len(b.Bars) < need {
		return nil
	}
	if !AllowsRegime(m.PreferredRegimes(), b.Regime) {
		return nil
	}

	closes := indicator.Closes(b.Bars)
	last := len(closes) - 1
	close := closes[last]
	if close <= 0 || math.IsNaN(close) {
		return nil
	}

	upper, _, lower := indicator.BollingerBands(closes, p.BBPeriod, p.BBMultiplier)
	rsi := indicator.RSI(closes, p.RSIPeriod)
	bbUpper := upper[last]
	bbLower := lower[last]
	rsiLast := rsi[last]
	if bbUpper <= 0 || bbLower <= 0 {
		return nil
	}
	// RSI can legitimately be 0 at the very first valid bar
	// (no prior gain). Skip on that boundary to avoid a false
	// "RSI=0, max oversold" signal.
	if rsiLast <= 0 {
		return nil
	}

	// ----- LONG: oversold mean-reversion --------------------------
	if close < bbLower && rsiLast < p.RSIOversold {
		conf := meanRevConfidenceOversold(rsiLast, p.RSIOversold)
		reasoning := fmt.Sprintf(
			"mean_reversion(rsi_bb): close %.4f below lower BB %.4f, RSI %.1f < %.1f (regime=%s)",
			close, bbLower, rsiLast, p.RSIOversold, b.Regime,
		)
		return &Proposal{
			Action:       ActionBuy,
			Confidence:   conf,
			Reasoning:    reasoning,
			StopLoss:     stopLossPrice(close, p.StopLossPct, ActionBuy),
			SignalSource: meanRevSignalSource,
		}
	}

	// ----- SHORT (= SELL existing long): overbought reversion -----
	if close > bbUpper && rsiLast > p.RSIOverbought {
		conf := meanRevConfidenceOverbought(rsiLast, p.RSIOverbought)
		reasoning := fmt.Sprintf(
			"mean_reversion(rsi_bb): close %.4f above upper BB %.4f, RSI %.1f > %.1f (regime=%s)",
			close, bbUpper, rsiLast, p.RSIOverbought, b.Regime,
		)
		return &Proposal{
			Action:       ActionSell,
			Confidence:   conf,
			Reasoning:    reasoning,
			StopLoss:     stopLossPrice(close, p.StopLossPct, ActionSell),
			SignalSource: meanRevSignalSource,
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Confidence model
// ---------------------------------------------------------------------------
//
// The RSI is bounded in [0, 100]. We translate distance below
// the oversold threshold (or above the overbought one) into the
// same [0.55, 0.95] band the trend sleeve uses, so attribution
// can compare apples to apples.

func meanRevConfidenceOversold(rsi, threshold float64) float64 {
	if rsi >= threshold {
		return 0.55
	}
	// Distance from threshold to 0 (the floor). RSI=30 → dist=0,
	// RSI=15 → dist=15 (≈ mid-band), RSI=0 → dist=threshold (sat).
	dist := threshold - rsi
	frac := dist / threshold
	if frac >= 1 {
		return 0.95
	}
	if frac <= 0 {
		return 0.55
	}
	return 0.55 + frac*(0.95-0.55)
}

func meanRevConfidenceOverbought(rsi, threshold float64) float64 {
	if rsi <= threshold {
		return 0.55
	}
	// Distance from threshold to 100 (the ceiling). RSI=70 → 0,
	// RSI=85 → dist=15 (≈ mid-band), RSI=100 → dist=30 (sat).
	dist := rsi - threshold
	frac := dist / (100 - threshold)
	if frac >= 1 {
		return 0.95
	}
	if frac <= 0 {
		return 0.55
	}
	return 0.55 + frac*(0.95-0.55)
}
