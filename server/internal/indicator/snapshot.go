package indicator

import (
	"fmt"
	"math"
	"strings"

	"github.com/fundai/server/internal/ohlc"
)

// Snapshot is the analyst-friendly aggregation of every indicator
// the quant role uses. Each field carries the value at the latest
// bar (the rightmost element of the input bars slice). Bool flags
// are derived; the quant role uses them directly when writing the
// "facts I see" portion of its debate prompt.
//
// A zero-valued Snapshot means "not enough bars to compute" — the
// caller should explicitly check len(bars) >= MinBarsForFullSnapshot
// to know whether the snapshot is fully populated.
type Snapshot struct {
	BarsUsed int

	LastClose  float64
	LastVolume float64

	// Trend / momentum
	SMA20     float64
	SMA50     float64
	SMA200    float64
	EMA12     float64
	EMA26     float64
	MACDLine  float64
	MACDSig   float64
	MACDHist  float64
	MACDCross string // "bullish" | "bearish" | "" (no fresh cross)
	RSI14     float64
	RSI14Tag  string // "overbought" | "oversold" | ""

	// Volatility / bands
	ATR14         float64
	ATR14PctOfPx  float64 // ATR / lastClose, as a fraction (0.02 = 2%)
	BBUpper       float64
	BBMid         float64
	BBLower       float64
	BBPctPosition float64 // (close - lower) / (upper - lower); 0.5 = at SMA

	// Chinese-broker style
	KDJK float64
	KDJD float64
	KDJJ float64

	// Volume
	VolMA20       float64
	RelativeVolume float64 // last bar / VolMA20; >1 == above average

	// Support / resistance over the prior SRLookback bars (excluding
	// the current bar so the "close above resistance" / "close below
	// support" semantics make sense — the current bar can break out
	// of its own range only against history). When fewer than
	// SRLookback+1 bars are available, all S/R fields stay zero.
	SupportLevel          float64
	ResistanceLevel       float64
	SRWindow              int     // SRLookback when computed; 0 otherwise
	SupportDistancePct    float64 // (close - support) / close; negative = closed below support
	ResistanceDistancePct float64 // (resistance - close) / close; negative = closed above resistance
	// BreakoutState describes price's relation to the prior range:
	//   "above_resistance" — close strictly above the prior 20-bar high
	//   "below_support"    — close strictly below the prior 20-bar low
	//   "near_resistance"  — close within 1×ATR (or 0.5% fallback) of resistance
	//   "near_support"     — close within 1×ATR (or 0.5% fallback) of support
	//   ""                 — comfortably inside the prior range
	BreakoutState string

	// Summary tags the prompt can render as bullets.
	Tags []string
}

// MinBarsForFullSnapshot is the lookback the analyst really wants;
// when fewer bars are passed, some fields will stay zero.
const MinBarsForFullSnapshot = 60

// SRLookback is the rolling window the support/resistance fields
// scan over. 20 bars matches the de facto "20-day high/low" the
// retail trading platforms display and the existing Donchian
// strategy sleeve.
const SRLookback = 20

// Compute produces a Snapshot for the latest bar. Returns an empty
// Snapshot when bars is too short to compute anything useful (<5
// bars) so callers can early-skip a degenerate symbol.
func Compute(bars []ohlc.Bar) Snapshot {
	snap := Snapshot{BarsUsed: len(bars)}
	if len(bars) < 5 {
		return snap
	}
	closes := Closes(bars)
	highs := Highs(bars)
	lows := Lows(bars)

	last := len(bars) - 1
	snap.LastClose = closes[last]
	snap.LastVolume = bars[last].Volume

	sma20 := SMA(closes, 20)
	sma50 := SMA(closes, 50)
	sma200 := SMA(closes, 200)
	snap.SMA20 = sma20[last]
	snap.SMA50 = sma50[last]
	snap.SMA200 = sma200[last]

	ema12 := EMA(closes, 12)
	ema26 := EMA(closes, 26)
	snap.EMA12 = ema12[last]
	snap.EMA26 = ema26[last]

	macd := MACD(closes, 12, 26, 9)
	snap.MACDLine = macd.Line[last]
	snap.MACDSig = macd.Signal[last]
	snap.MACDHist = macd.Histogram[last]
	snap.MACDCross = detectMACDCross(macd, last)

	rsi := RSI(closes, 14)
	snap.RSI14 = rsi[last]
	switch {
	case rsi[last] >= 70:
		snap.RSI14Tag = "overbought"
	case rsi[last] > 0 && rsi[last] <= 30:
		snap.RSI14Tag = "oversold"
	}

	atr := ATR(highs, lows, closes, 14)
	snap.ATR14 = atr[last]
	if snap.LastClose != 0 {
		snap.ATR14PctOfPx = atr[last] / snap.LastClose
	}

	upper, mid, lower := BollingerBands(closes, 20, 2)
	snap.BBUpper = upper[last]
	snap.BBMid = mid[last]
	snap.BBLower = lower[last]
	if upper[last] > lower[last] {
		snap.BBPctPosition = (snap.LastClose - lower[last]) / (upper[last] - lower[last])
	}

	kdj := KDJ(highs, lows, closes, 9, 3, 3)
	snap.KDJK = kdj.K[last]
	snap.KDJD = kdj.D[last]
	snap.KDJJ = kdj.J[last]

	volMA := VolumeMA(bars, 20)
	snap.VolMA20 = volMA[last]
	if volMA[last] > 0 {
		snap.RelativeVolume = bars[last].Volume / volMA[last]
	}

	// Support / resistance: scan the prior SRLookback bars
	// (NOT including the current bar) for the min low and max
	// high. Excluding the current bar is what lets us classify
	// the latest close as a breakout / breakdown — a Donchian-
	// style INclusive window would always have the current bar
	// touching one extreme.
	if last >= SRLookback {
		start := last - SRLookback
		support := lows[start]
		resistance := highs[start]
		for i := start; i < last; i++ {
			if lows[i] < support {
				support = lows[i]
			}
			if highs[i] > resistance {
				resistance = highs[i]
			}
		}
		snap.SupportLevel = support
		snap.ResistanceLevel = resistance
		snap.SRWindow = SRLookback
		if snap.LastClose != 0 {
			snap.SupportDistancePct = (snap.LastClose - support) / snap.LastClose
			snap.ResistanceDistancePct = (resistance - snap.LastClose) / snap.LastClose
		}
		snap.BreakoutState = classifyBreakoutState(snap.LastClose, support, resistance, snap.ATR14)
	}

	snap.Tags = buildTags(snap, closes)
	return snap
}

// classifyBreakoutState labels the current close against the prior
// support / resistance band. "near" is defined as within 1×ATR14
// when ATR is available, otherwise within 0.5% of price — that
// fallback keeps the classifier useful on synthetic / low-vol test
// fixtures where ATR may not yet be valid.
func classifyBreakoutState(lastClose, support, resistance, atr14 float64) string {
	if support <= 0 || resistance <= 0 || lastClose <= 0 {
		return ""
	}
	switch {
	case lastClose > resistance:
		return "above_resistance"
	case lastClose < support:
		return "below_support"
	}
	near := atr14
	if near <= 0 {
		near = 0.005 * lastClose
	}
	if near <= 0 {
		return ""
	}
	if (resistance - lastClose) <= near {
		return "near_resistance"
	}
	if (lastClose - support) <= near {
		return "near_support"
	}
	return ""
}

// detectMACDCross checks the last two bars for a sign change of
// Histogram — that's the canonical "fresh cross" indication every
// platform highlights. Returns "" if the latest cross is more than
// one bar old, so the quant role doesn't keep claiming "MACD just
// crossed" three days after the fact.
func detectMACDCross(m MACDResult, last int) string {
	if last < 1 {
		return ""
	}
	prev := m.Histogram[last-1]
	cur := m.Histogram[last]
	if prev < 0 && cur > 0 {
		return "bullish"
	}
	if prev > 0 && cur < 0 {
		return "bearish"
	}
	return ""
}

// buildTags is the human-readable layer the quant role embeds in
// its debate prompt. Each tag is a short fact ("RSI 76: overbought",
// "MA20 > MA50 > MA200 (uptrend)") so the model can reuse them as
// keyPoints verbatim.
func buildTags(s Snapshot, closes []float64) []string {
	tags := []string{}
	// Trend stack
	if s.SMA20 > 0 && s.SMA50 > 0 && s.SMA200 > 0 {
		if s.SMA20 > s.SMA50 && s.SMA50 > s.SMA200 {
			tags = append(tags, "trend: SMA20 > SMA50 > SMA200 (multi-timeframe uptrend)")
		}
		if s.SMA20 < s.SMA50 && s.SMA50 < s.SMA200 {
			tags = append(tags, "trend: SMA20 < SMA50 < SMA200 (multi-timeframe downtrend)")
		}
	}
	// MACD
	if s.MACDCross == "bullish" {
		tags = append(tags, "MACD: bullish cross at latest bar")
	} else if s.MACDCross == "bearish" {
		tags = append(tags, "MACD: bearish cross at latest bar")
	}
	if s.MACDHist != 0 {
		tags = append(tags, fmt.Sprintf("MACD hist: %.3f (%s)", s.MACDHist, signLabel(s.MACDHist)))
	}
	// RSI
	if s.RSI14 > 0 {
		tag := fmt.Sprintf("RSI14: %.1f", s.RSI14)
		if s.RSI14Tag != "" {
			tag += " (" + s.RSI14Tag + ")"
		}
		tags = append(tags, tag)
	}
	// KDJ
	if s.KDJK != 0 || s.KDJD != 0 {
		extra := ""
		if s.KDJJ > 90 {
			extra = " (J>90 hot)"
		} else if s.KDJJ < 10 && s.KDJJ != 0 {
			extra = " (J<10 cool)"
		}
		tags = append(tags, fmt.Sprintf("KDJ: K=%.1f D=%.1f J=%.1f%s", s.KDJK, s.KDJD, s.KDJJ, extra))
	}
	// Bollinger position
	if s.BBUpper > 0 && s.BBLower > 0 {
		switch {
		case s.LastClose >= s.BBUpper:
			tags = append(tags, fmt.Sprintf("BB: price %.2f at/above upper band %.2f", s.LastClose, s.BBUpper))
		case s.LastClose <= s.BBLower:
			tags = append(tags, fmt.Sprintf("BB: price %.2f at/below lower band %.2f", s.LastClose, s.BBLower))
		}
	}
	// Volatility
	if s.ATR14PctOfPx > 0 {
		tags = append(tags, fmt.Sprintf("ATR14 ≈ %.2f%% of price", s.ATR14PctOfPx*100))
	}
	// Volume
	if s.RelativeVolume > 0 {
		desc := "below avg"
		if s.RelativeVolume >= 1.5 {
			desc = "volume surge"
		} else if s.RelativeVolume >= 1 {
			desc = "above avg"
		}
		tags = append(tags, fmt.Sprintf("Volume %.2fx vs 20d avg (%s)", s.RelativeVolume, desc))
	}
	// Support / resistance
	if s.SupportLevel > 0 && s.ResistanceLevel > 0 {
		tags = append(tags, fmt.Sprintf(
			"S/R(%dd): support %.2f, resistance %.2f",
			s.SRWindow, s.SupportLevel, s.ResistanceLevel,
		))
		switch s.BreakoutState {
		case "above_resistance":
			tags = append(tags, fmt.Sprintf(
				"breakout: close %.2f > prior %dd high %.2f",
				s.LastClose, s.SRWindow, s.ResistanceLevel,
			))
		case "below_support":
			tags = append(tags, fmt.Sprintf(
				"breakdown: close %.2f < prior %dd low %.2f",
				s.LastClose, s.SRWindow, s.SupportLevel,
			))
		case "near_resistance":
			tags = append(tags, fmt.Sprintf(
				"price within 1×ATR of %dd resistance %.2f",
				s.SRWindow, s.ResistanceLevel,
			))
		case "near_support":
			tags = append(tags, fmt.Sprintf(
				"price within 1×ATR of %dd support %.2f",
				s.SRWindow, s.SupportLevel,
			))
		}
	}
	// Short-term momentum (5-bar return)
	if len(closes) >= 6 {
		ret := closes[len(closes)-1]/closes[len(closes)-6] - 1
		tags = append(tags, fmt.Sprintf("5-bar return: %.2f%%", ret*100))
	}
	return tags
}

func signLabel(v float64) string {
	switch {
	case v > 0:
		return "positive"
	case v < 0:
		return "negative"
	default:
		return "flat"
	}
}

// FormatForPrompt collapses a Snapshot into a single text line the
// debate / decision LLM prompts can splice into per-symbol notes.
// Returns empty string when the snapshot has no useful data so the
// caller can skip the bullet entirely.
func (s Snapshot) FormatForPrompt(symbol string) string {
	if len(s.Tags) == 0 || s.LastClose == 0 {
		return ""
	}
	header := fmt.Sprintf("%s @ %.4f", strings.ToUpper(strings.TrimSpace(symbol)), s.LastClose)
	if math.IsNaN(s.LastClose) {
		return ""
	}
	return header + " — " + strings.Join(s.Tags, "; ")
}

// AsAnalystSignals returns the well-known signal keys the
// agent.TechnicalAnalyst hard-votes on. Wiring layers that have an
// indicator.Snapshot in hand can use this to fill the
// agent.TechnicalBlock.Signals map without re-deriving the canonical
// key names.
//
// Returned keys (canonical, lower-snake-case):
//   - rsi14: raw RSI (0–100)
//   - macd_hist: raw MACD histogram value
//   - ma50_over_ma200: +1 / -1 — sign of (SMA50 − SMA200)
//   - breakout: +1 (close above prior 20-bar high), -1 (below prior
//     20-bar low), 0 omitted when range-bound
//   - relative_volume: latest bar volume / 20-bar average volume
//   - support_distance_pct: (close − support) / close
//   - resistance_distance_pct: (resistance − close) / close
//
// Keys for which the snapshot has no data (e.g. SMA200 not yet
// computed because <200 bars) are omitted. The TechnicalAnalyst
// skips unrecognised / missing keys, so a partial map is safe to
// pass through.
func (s Snapshot) AsAnalystSignals() map[string]float64 {
	out := make(map[string]float64, 7)
	if s.RSI14 > 0 {
		out["rsi14"] = s.RSI14
	}
	if s.MACDHist != 0 || s.MACDLine != 0 {
		out["macd_hist"] = s.MACDHist
	}
	if s.SMA50 > 0 && s.SMA200 > 0 {
		switch {
		case s.SMA50 > s.SMA200:
			out["ma50_over_ma200"] = 1
		case s.SMA50 < s.SMA200:
			out["ma50_over_ma200"] = -1
		}
	}
	switch s.BreakoutState {
	case "above_resistance":
		out["breakout"] = 1
	case "below_support":
		out["breakout"] = -1
	}
	if s.RelativeVolume > 0 {
		out["relative_volume"] = s.RelativeVolume
	}
	if s.SRWindow > 0 {
		out["support_distance_pct"] = s.SupportDistancePct
		out["resistance_distance_pct"] = s.ResistanceDistancePct
	}
	return out
}
