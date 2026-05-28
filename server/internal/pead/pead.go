// Package pead computes Post-Earnings Announcement Drift signals
// for the PM prompt.
//
// Sprint F #3. Bernard & Thomas 1989 documented that stocks
// continue to drift in the direction of their earnings surprise
// for roughly 60 trading days after the print — one of the most
// replicated anomalies in academic finance and a staple of every
// institutional event-driven book (AQR, Renaissance, Citadel
// pods all run a PEAD overlay).
//
// The forward `earnings.Snapshot` (Sprint E #2) tells the PM
// what catalysts are coming. PEAD answers the complementary
// question: of recent catalysts that ALREADY HIT, which ones
// still have drift left to run? For long-only LLM PMs this
// becomes a "tilt" overlay:
//
//   - Strong positive surprise (>= +3%) + recent (< 30 days) +
//     drift incomplete (drift < 1.5× surprise in same direction)
//     → the PEAD continuation thesis is intact; bullish tilt.
//
//   - Strong negative surprise + recent + drift incomplete in the
//     SAME direction → the drift is grinding lower; for long-only,
//     this is a "reduce" or "watch" signal, never a buy.
//
//   - Surprise + drift in OPPOSITE directions → the "gap fade"
//     case (stock beat but sold off). For positive surprise this
//     is the classic deep-value setup (drift inverted, but the
//     fundamental was strong); the PM is allowed to add. For
//     negative surprise + positive drift it's a "dead-cat bounce"
//     and a long-only PM should be cautious.
//
//   - Drift complete (|drift| >= 1.5× |surprise|) → the alpha is
//     mostly priced in; signal is neutral.
//
// Numerical recipe per symbol:
//
//     event_date    = most recent earnings announcement in trailing
//                     LookbackDays (default 60)
//     surprise_pct  = HistoricalEvent.SurprisePercent
//                     (decimal — 0.05 = 5% beat)
//     entry_close   = close on event_date (anchor; if event_date
//                     was a non-trading day we use the next
//                     available trading-day close)
//     current_close = most recent close
//     drift_pct     = (current_close - entry_close) / entry_close
//     drift_state:
//                  continuing  → sign(surprise)=sign(drift) AND
//                                |drift| < 1.5 × |surprise|
//                  complete    → sign(surprise)=sign(drift) AND
//                                |drift| >= 1.5 × |surprise|
//                  faded       → sign(surprise) != sign(drift)
//                  neutral     → |surprise| < MinSurprisePct OR
//                                drift missing (e.g. listing gap)
//
// Architecture mirrors earnings / quality / value:
//   - SymbolRequest names a (symbol, market) pair.
//   - Options carries thresholds + lookbacks.
//   - Service holds the earnings.HistoryFetcher + ohlc.Fetcher.
//   - BuildSignals returns one Signal per symbol with a usable
//     recent catalyst, sorted by |SurprisePercent| desc.
//
// Graceful degradation: a nil history fetcher OR a nil ohlc
// fetcher returns nil from BuildSignals (the prompt omits the
// block). Same "feature off when dependencies are missing"
// contract as the rest of the signal pipeline.
package pead

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/earnings"
	"github.com/fundai/server/internal/ohlc"
)

// DriftState classifies the PEAD signal direction.
type DriftState string

const (
	// DriftStateContinuing: surprise and drift have the same
	// sign AND |drift| < 1.5× |surprise|. The continuation
	// thesis is intact — the drift has room to run.
	DriftStateContinuing DriftState = "continuing"
	// DriftStateComplete: surprise and drift in the same
	// direction AND |drift| >= 1.5× |surprise|. The alpha is
	// mostly priced in; neutral signal going forward.
	DriftStateComplete DriftState = "complete"
	// DriftStateFaded: surprise and drift in OPPOSITE
	// directions. Positive surprise + negative drift = "good
	// print, bad reaction" (deep value); negative surprise +
	// positive drift = dead-cat bounce.
	DriftStateFaded DriftState = "faded"
	// DriftStateNeutral: surprise too small to count OR drift
	// undefined (insufficient bars between event and now).
	DriftStateNeutral DriftState = "neutral"
)

// Signal is one symbol's PEAD reading.
type Signal struct {
	// Symbol is the upper-cased ticker.
	Symbol string
	// EventDate is the calendar day the earnings print landed.
	EventDate time.Time
	// DaysSinceEvent is the integer calendar-day count from
	// EventDate to the AsOf time. >= 1 always (we filter out
	// same-day prints upstream — drift is undefined that day).
	DaysSinceEvent int
	// SurprisePercent is the earnings beat / miss (decimal —
	// 0.087 = 8.7% beat). Positive = beat, negative = miss.
	SurprisePercent float64
	// EntryClose is the close on EventDate (or the next
	// available trading-day close). Anchor for drift math.
	EntryClose float64
	// CurrentClose is the most recent close in the OHLC window.
	CurrentClose float64
	// DriftPercent is (CurrentClose - EntryClose) / EntryClose.
	// Positive = price rose since the print; negative = fell.
	DriftPercent float64
	// State is the classified drift state — the LLM's single
	// decision-critical field.
	State DriftState
}

// SymbolRequest names a (symbol, market) pair. Same shape as
// the rest of the sleeve services.
type SymbolRequest struct {
	Symbol string
	Market string
}

// Snapshot is the prompt-facing aggregate. One row per
// symbol with a usable recent catalyst, sorted by
// |SurprisePercent| descending (so the strongest catalysts top
// the block).
type Snapshot struct {
	// AsOf is the wall-clock the snapshot was assembled at.
	AsOf time.Time
	// LookbackDays is the trailing window covered (echoed back
	// from Options.LookbackDays for the prompt).
	LookbackDays int
	// MinSurprisePct is the threshold below which a print is
	// classified as DriftStateNeutral. Echoed for transparency.
	MinSurprisePct float64
	// Signals is the actionable per-symbol slice sorted by
	// |SurprisePercent| descending.
	Signals []Signal
}

// HasSignal reports whether the snapshot carries any non-neutral
// row. The wiring layer drops the prompt block when this is
// false so the LLM doesn't waste context on noise.
func (s *Snapshot) HasSignal() bool {
	if s == nil {
		return false
	}
	for _, sig := range s.Signals {
		if sig.State != DriftStateNeutral {
			return true
		}
	}
	return false
}

// Options tunes the drift window, the surprise threshold, and
// the OHLC bar window. Zero-value Options yields production
// defaults (60-day lookback, 0.03 surprise floor, 90-bar OHLC
// pull).
type Options struct {
	// LookbackDays caps how far back we look for the most
	// recent earnings event. Default 60 — matches the canonical
	// PEAD literature horizon.
	LookbackDays int
	// MinSurprisePct floors the |surprise| at which a print is
	// considered actionable. Default 0.03 (3% beat / miss).
	// Smaller surprises don't reliably drift in the data; they
	// land in DriftStateNeutral.
	MinSurprisePct float64
	// CompleteMultiplier sets the drift-vs-surprise ratio above
	// which the drift is "complete". Default 1.5 (drift has
	// already moved 1.5× the surprise size).
	CompleteMultiplier float64
	// OHLCLookbackN is the number of trailing daily bars to
	// request per symbol. Default = LookbackDays + 30 so we
	// always have at least a month of pre-event padding to
	// resolve the EntryClose for events at the start of the
	// window.
	OHLCLookbackN int
	// Now is the clock function. Default time.Now (UTC).
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.LookbackDays <= 0 {
		o.LookbackDays = 60
	}
	if o.MinSurprisePct <= 0 {
		o.MinSurprisePct = 0.03
	}
	if o.CompleteMultiplier <= 0 {
		o.CompleteMultiplier = 1.5
	}
	if o.OHLCLookbackN <= 0 {
		o.OHLCLookbackN = o.LookbackDays + 30
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	return o
}

// Service is the per-process orchestrator.
type Service struct {
	history *earnings.HistoryService
	ohlc    ohlc.Fetcher
	opts    Options
}

// NewService wires the Service. nil history OR nil ohlc fetcher
// returns nil from BuildSnapshot (block omitted). Matches the
// rest of the sleeve services.
func NewService(history *earnings.HistoryService, ohlcFetcher ohlc.Fetcher, opts Options) *Service {
	return &Service{
		history: history,
		ohlc:    ohlcFetcher,
		opts:    opts.withDefaults(),
	}
}

// Options exposes the resolved tuning struct (tests + introspection).
func (s *Service) Options() Options {
	if s == nil {
		return Options{}.withDefaults()
	}
	return s.opts
}

// BuildSnapshot runs the per-fund PEAD pass: pulls recent
// earnings history per symbol, fetches the price drift, and
// classifies each row. Returns nil on every failure path.
func (s *Service) BuildSnapshot(ctx context.Context, requests []SymbolRequest) *Snapshot {
	if s == nil || s.history == nil || s.ohlc == nil {
		return nil
	}
	now := s.opts.Now().UTC()
	symbols := uniqueSymbols(requests)
	if len(symbols) == 0 {
		return nil
	}
	market := dominantMarket(requests)
	historySnap := s.history.Build(ctx, symbols, market)
	if !historySnap.HasSignal() {
		return nil
	}
	signals := make([]Signal, 0, len(historySnap.PerSymbol))
	for sym, event := range historySnap.PerSymbol {
		mkt := event.Market
		if mkt == "" {
			mkt = market
		}
		sig := s.buildOne(ctx, sym, mkt, event, now)
		if sig != nil {
			signals = append(signals, *sig)
		}
	}
	if len(signals) == 0 {
		return nil
	}
	sort.Slice(signals, func(i, j int) bool {
		ai := math.Abs(signals[i].SurprisePercent)
		aj := math.Abs(signals[j].SurprisePercent)
		if ai != aj {
			return ai > aj
		}
		return signals[i].Symbol < signals[j].Symbol
	})
	return &Snapshot{
		AsOf:           now,
		LookbackDays:   s.opts.LookbackDays,
		MinSurprisePct: s.opts.MinSurprisePct,
		Signals:        signals,
	}
}

// buildOne computes a single Signal from a recent earnings
// event + an OHLC fetch. Returns nil when the bar window can't
// resolve EntryClose (e.g. provider returned only post-event
// bars) OR when SurprisePercent is exactly zero AND we can't
// derive it (no EPS data either).
func (s *Service) buildOne(ctx context.Context, symbol, market string, event earnings.HistoricalEvent, now time.Time) *Signal {
	closes := s.fetchCloses(ctx, symbol, market)
	if len(closes) == 0 {
		return nil
	}
	entryClose, currentClose := alignBars(closes, event.EventDate)
	if entryClose <= 0 || currentClose <= 0 {
		return nil
	}
	drift := (currentClose - entryClose) / entryClose
	surprise := event.SurprisePercent
	state := classifyDrift(surprise, drift, s.opts.MinSurprisePct, s.opts.CompleteMultiplier)
	days := int(now.Sub(event.EventDate).Hours() / 24)
	if days < 1 {
		// Same-day or future event date (shouldn't happen because
		// HistoryService filters strictly past, but defensive).
		return nil
	}
	return &Signal{
		Symbol:          symbol,
		EventDate:       event.EventDate,
		DaysSinceEvent:  days,
		SurprisePercent: surprise,
		EntryClose:      entryClose,
		CurrentClose:    currentClose,
		DriftPercent:    drift,
		State:           state,
	}
}

// fetchCloses pulls the OHLC bars for one symbol and returns a
// time-ordered slice of (time, close) pairs. Empty when the
// fetch fails OR every bar's close is non-positive.
func (s *Service) fetchCloses(ctx context.Context, symbol, market string) []closeBar {
	bars, err := s.ohlc.Fetch(ctx, ohlc.FetchRequest{
		Symbol:    symbol,
		Market:    market,
		Interval:  ohlc.IntervalDay,
		LookbackN: s.opts.OHLCLookbackN,
	})
	if err != nil || len(bars) == 0 {
		return nil
	}
	out := make([]closeBar, 0, len(bars))
	for _, b := range bars {
		if b.Close <= 0 || math.IsNaN(b.Close) || math.IsInf(b.Close, 0) {
			continue
		}
		out = append(out, closeBar{Time: b.Time.UTC(), Close: b.Close})
	}
	// Defensive sort — most providers already return ascending
	// by time but the contract isn't enforced cross-provider.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Time.Before(out[j].Time)
	})
	return out
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

type closeBar struct {
	Time  time.Time
	Close float64
}

// alignBars finds the close on the EventDate (or the next
// available trading-day close after it) and the most recent
// close in the slice. Returns (0, 0) when the slice has no bar
// at or after EventDate.
func alignBars(closes []closeBar, eventDate time.Time) (entry, current float64) {
	if len(closes) == 0 {
		return 0, 0
	}
	// EventDate strips to day granularity; we compare on the
	// CALENDAR day of the bar.timestamp.
	eventDay := eventDate.UTC().Truncate(24 * time.Hour)
	for _, b := range closes {
		barDay := b.Time.UTC().Truncate(24 * time.Hour)
		if !barDay.Before(eventDay) {
			entry = b.Close
			break
		}
	}
	if entry <= 0 {
		return 0, 0
	}
	current = closes[len(closes)-1].Close
	return entry, current
}

// classifyDrift implements the four-state PEAD signal classifier.
func classifyDrift(surprise, drift, minSurprise, completeMult float64) DriftState {
	if math.Abs(surprise) < minSurprise {
		return DriftStateNeutral
	}
	if surprise == 0 || drift == 0 {
		return DriftStateNeutral
	}
	sameSign := (surprise > 0 && drift > 0) || (surprise < 0 && drift < 0)
	if !sameSign {
		return DriftStateFaded
	}
	if math.Abs(drift) >= completeMult*math.Abs(surprise) {
		return DriftStateComplete
	}
	return DriftStateContinuing
}

// uniqueSymbols dedups + uppercase-normalises the request
// symbols. Reused from earnings package's normaliseSymbols
// (called via Build) but we also need a standalone version here
// for early-bail.
func uniqueSymbols(requests []SymbolRequest) []string {
	seen := make(map[string]struct{}, len(requests))
	out := make([]string, 0, len(requests))
	for _, req := range requests {
		s := strings.ToUpper(strings.TrimSpace(req.Symbol))
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// dominantMarket picks the most-common market tag in the request
// list. Used for the single-shot history.Build call. Single-fund
// requests almost always converge on one market; cross-market
// funds will see a slight bias but the per-event Market hint on
// the resulting HistoricalEvent rows overrides it downstream.
func dominantMarket(requests []SymbolRequest) string {
	counts := make(map[string]int, len(requests))
	for _, req := range requests {
		m := strings.ToLower(strings.TrimSpace(req.Market))
		counts[m]++
	}
	best := ""
	bestN := -1
	for m, n := range counts {
		if n > bestN {
			best = m
			bestN = n
		}
	}
	return best
}
