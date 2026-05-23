// Package exposure surfaces fund-level concentration risk to the PM
// prompt.
//
// Why this exists. Per-trade R-sizing (Sprint A #1) and dynamic
// risk-budget throttling (Sprint B #2) bound INDIVIDUAL trade risk
// but neither catches the classic concentration accidents:
//
//   1. Single-name concentration. A fund that's already 30% in
//      AAPL and adds another 5% block can become a de-facto AAPL
//      tracker without any single trade tripping the per-trade
//      cap.
//   2. Sector concentration. Five separate buys across NVDA / AMD
//      / TSMC / ASML / AVGO each pass the per-symbol gate but the
//      portfolio ends up 60% semis with zero diversification.
//   3. Cash drift. A fund that quietly leans toward 95% deployed
//      has no dry powder for the next drawdown or for the next
//      good idea — the post-Lehman risk-management literature is
//      unanimous on keeping a configurable cash floor.
//   4. Top-N concentration. The Bridgewater / AQR / Citadel
//      diversification convention is "no top-3 cluster > 60% of
//      book"; otherwise a single sector shock can blow the whole
//      portfolio.
//
// Sprint C #1 computes all four and surfaces them to the prompt
// as a structured Snapshot so the PM can downgrade sizing /
// reject the buy entirely when a candidate would push a bucket
// past its cap. The system prompt teaches the LLM to apply caps
// asymmetrically: "buy" can be blocked, "reduce" / "sell" never
// is.
//
// I/O contract. This is a pure-Go function over the existing
// decision.DecisionPosition slice + a few scalars (NAV,
// AvailableCash). No DB reads, no I/O, no clock — same shape as
// internal/sizing. The wiring layer hands in the values it
// already has and the package returns a deterministic Snapshot.
package exposure

import (
	"math"
	"sort"
	"strings"
)

// Position is the minimal view of one holding the exposure
// computation needs. The wiring layer constructs it from
// decision.DecisionPosition; the alias-free type lets the package
// stay dependency-free.
type Position struct {
	Symbol      string
	Sector      string  // e.g. "tech", "energy", "financials"; "" → "unclassified"
	MarketValue float64 // current MV in fund's base currency; > 0 expected
}

// SymbolWeight is a single-name concentration row. Sorted DESC by
// Weight in the prompt-facing slice so the heaviest names land
// first.
type SymbolWeight struct {
	Symbol string
	Weight float64 // [0, 1] as a fraction of TotalAssets
	Cap    float64 // applicable cap, e.g. 0.25
	Breach bool    // Weight > Cap
}

// SectorWeight is a sector-level rollup. Sectors are the values of
// the Position.Sector field, normalised to lower case + trimmed;
// blank sectors are collapsed into "unclassified".
type SectorWeight struct {
	Sector string
	Weight float64
	Cap    float64
	Breach bool
}

// Snapshot is the prompt-facing exposure read. Empty fields mean
// the corresponding view wasn't computable (e.g. no positions →
// no Top3Weight, but CashPct still meaningful). Breaches surface
// the prompt-facing one-liners the LLM should respect.
type Snapshot struct {
	// TotalAssets and AvailableCash echo the inputs so the
	// prompt can render the absolute dollars alongside the
	// percentages without re-computing.
	TotalAssets   float64
	AvailableCash float64

	// CashPct is AvailableCash / TotalAssets. < CashFloorPct
	// is flagged in Breaches but not in this field — we want
	// the PM to see the actual number.
	CashPct      float64
	CashFloorPct float64

	// PositionCount is the count of positions with MV > 0.
	PositionCount int

	// SinglesName is the per-symbol concentration view sorted
	// DESC by weight. Empty when there are no positions.
	SingleName []SymbolWeight
	SingleNameCap float64

	// SectorWeights is the per-sector rollup. Same DESC sort.
	// Empty when there are no positions.
	SectorWeights []SectorWeight
	SectorCap     float64

	// Top3Weight is the sum of the largest three position
	// weights. Zero when there are fewer than 3 positions.
	Top3Weight float64
	Top3Cap    float64

	// Breaches lists the human-readable one-liners the prompt
	// repeats to the LLM verbatim. Each entry maps to one
	// breach (single-name OR sector OR top-3 OR cash floor).
	// Empty when the portfolio is clean.
	Breaches []string
}

// HasSignal reports whether the snapshot carries enough data to be
// worth rendering. Used by the prompt builder to omit the block
// entirely on degenerate inputs.
func (s Snapshot) HasSignal() bool {
	return s.TotalAssets > 0 && (s.PositionCount > 0 || s.CashPct > 0)
}

// Options configures the caps. The zero value is fine; withDefaults
// installs the conventional limits used by AQR / Bridgewater /
// most modern multi-asset funds.
type Options struct {
	// SingleNameCap is the maximum any one position should
	// hold. Default 0.25 (25%); clamped to [0.05, 1.0].
	SingleNameCap float64

	// SectorCap is the maximum any one sector bucket should
	// hold. Default 0.50 (50%); clamped to [0.10, 1.0].
	SectorCap float64

	// Top3Cap is the maximum the largest three positions
	// combined should hold. Default 0.60 (60%); clamped to
	// [0.20, 1.0].
	Top3Cap float64

	// CashFloorPct is the minimum cash fraction. Default 0.05
	// (5%); clamped to [0.0, 0.50]. Below this the snapshot
	// emits a "cash drift" breach asking the PM to either
	// release a position or skip new buys.
	CashFloorPct float64
}

func (o Options) withDefaults() Options {
	if o.SingleNameCap <= 0 {
		o.SingleNameCap = 0.25
	}
	if o.SingleNameCap < 0.05 {
		o.SingleNameCap = 0.05
	}
	if o.SingleNameCap > 1.0 {
		o.SingleNameCap = 1.0
	}
	if o.SectorCap <= 0 {
		o.SectorCap = 0.50
	}
	if o.SectorCap < 0.10 {
		o.SectorCap = 0.10
	}
	if o.SectorCap > 1.0 {
		o.SectorCap = 1.0
	}
	if o.Top3Cap <= 0 {
		o.Top3Cap = 0.60
	}
	if o.Top3Cap < 0.20 {
		o.Top3Cap = 0.20
	}
	if o.Top3Cap > 1.0 {
		o.Top3Cap = 1.0
	}
	// CashFloorPct is special: 0 is a meaningful "don't enforce
	// a floor" config, so the only invariants are non-negative
	// and below the upper guard.
	if o.CashFloorPct < 0 {
		o.CashFloorPct = 0
	}
	if o.CashFloorPct > 0.50 {
		o.CashFloorPct = 0.50
	}
	return o
}

// Compute is the only public function. Pure: same inputs ↦ same
// outputs. The function is allocation-bounded by len(positions)
// so it's cheap to call on every PM run.
//
// totalAssets must be > 0; otherwise the snapshot is empty (the
// prompt builder will omit the block). availableCash may be 0.
func Compute(opts Options, totalAssets, availableCash float64, positions []Position) Snapshot {
	opts = opts.withDefaults()

	if totalAssets <= 0 {
		return Snapshot{}
	}

	cashPct := 0.0
	if availableCash > 0 {
		cashPct = availableCash / totalAssets
	}

	// Bucket positions by symbol (dedup) and sector, dropping
	// negative / zero MV rows (they don't carry exposure).
	type bySym struct {
		symbol string
		mv     float64
		sector string
	}
	bySymMap := make(map[string]*bySym, len(positions))
	for _, p := range positions {
		sym := strings.ToUpper(strings.TrimSpace(p.Symbol))
		if sym == "" || p.MarketValue <= 0 {
			continue
		}
		sector := strings.ToLower(strings.TrimSpace(p.Sector))
		if sector == "" {
			sector = "unclassified"
		}
		if existing, ok := bySymMap[sym]; ok {
			existing.mv += p.MarketValue
			continue
		}
		bySymMap[sym] = &bySym{symbol: sym, mv: p.MarketValue, sector: sector}
	}

	syms := make([]bySym, 0, len(bySymMap))
	for _, b := range bySymMap {
		syms = append(syms, *b)
	}

	sort.SliceStable(syms, func(i, j int) bool {
		if syms[i].mv == syms[j].mv {
			return syms[i].symbol < syms[j].symbol
		}
		return syms[i].mv > syms[j].mv
	})

	singleName := make([]SymbolWeight, 0, len(syms))
	breaches := make([]string, 0)
	for _, s := range syms {
		w := s.mv / totalAssets
		breach := w > opts.SingleNameCap
		singleName = append(singleName, SymbolWeight{
			Symbol: s.symbol,
			Weight: round4(w),
			Cap:    opts.SingleNameCap,
			Breach: breach,
		})
		if breach {
			breaches = append(breaches, formatBreach("single-name", s.symbol, w, opts.SingleNameCap))
		}
	}

	sectorMV := make(map[string]float64, len(syms))
	for _, s := range syms {
		sectorMV[s.sector] += s.mv
	}
	sectorWeights := make([]SectorWeight, 0, len(sectorMV))
	for sector, mv := range sectorMV {
		w := mv / totalAssets
		breach := w > opts.SectorCap
		sectorWeights = append(sectorWeights, SectorWeight{
			Sector: sector,
			Weight: round4(w),
			Cap:    opts.SectorCap,
			Breach: breach,
		})
		if breach {
			breaches = append(breaches, formatBreach("sector", sector, w, opts.SectorCap))
		}
	}
	sort.SliceStable(sectorWeights, func(i, j int) bool {
		if sectorWeights[i].Weight == sectorWeights[j].Weight {
			return sectorWeights[i].Sector < sectorWeights[j].Sector
		}
		return sectorWeights[i].Weight > sectorWeights[j].Weight
	})

	top3 := 0.0
	if len(syms) >= 3 {
		top3 = (syms[0].mv + syms[1].mv + syms[2].mv) / totalAssets
		if top3 > opts.Top3Cap {
			breaches = append(breaches, formatBreach("top-3", "cluster", top3, opts.Top3Cap))
		}
	}

	if opts.CashFloorPct > 0 && cashPct < opts.CashFloorPct {
		breaches = append(breaches, formatCashBreach(cashPct, opts.CashFloorPct))
	}

	// Determinism — breach order is otherwise tied to map
	// iteration order on the sector pass.
	sort.Strings(breaches)

	return Snapshot{
		TotalAssets:   totalAssets,
		AvailableCash: availableCash,
		CashPct:       round4(cashPct),
		CashFloorPct:  opts.CashFloorPct,
		PositionCount: len(syms),
		SingleName:    singleName,
		SingleNameCap: opts.SingleNameCap,
		SectorWeights: sectorWeights,
		SectorCap:     opts.SectorCap,
		Top3Weight:    round4(top3),
		Top3Cap:       opts.Top3Cap,
		Breaches:      breaches,
	}
}

// formatBreach renders one breach line for the prompt. The format
// is fixed so the LLM can pattern-match on it: "BREACH:
// <bucket>=<name> weight=<pct>% > cap=<pct>%".
func formatBreach(bucket, name string, weight, cap float64) string {
	return "BREACH: " + bucket + "=" + name +
		" weight=" + pctString(weight) +
		" > cap=" + pctString(cap)
}

func formatCashBreach(cashPct, floorPct float64) string {
	return "BREACH: cash=" + pctString(cashPct) +
		" < floor=" + pctString(floorPct) +
		" (consider releasing a position before any new buy)"
}

// pctString trims a fraction to one decimal percent. Floor at 0
// (we never want negative percent in the prompt).
func pctString(v float64) string {
	if v < 0 {
		v = 0
	}
	tenths := int64(math.Round(v * 1000))
	whole := tenths / 10
	frac := tenths % 10
	if frac < 0 {
		frac = -frac
	}
	if whole < 0 {
		whole = 0
	}
	out := make([]byte, 0, 8)
	out = appendInt(out, whole)
	out = append(out, '.')
	out = appendInt(out, frac)
	out = append(out, '%')
	return string(out)
}

func appendInt(b []byte, v int64) []byte {
	if v == 0 {
		return append(b, '0')
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var tmp [20]byte
	i := len(tmp)
	for v > 0 {
		i--
		tmp[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		b = append(b, '-')
	}
	return append(b, tmp[i:]...)
}

// round4 trims a fraction to 4 dp so the prompt JSON stays diff-
// friendly across runs.
func round4(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v < 0 {
		v = 0
	}
	const scale = 1e4
	return float64(int64(v*scale+0.5)) / scale
}
