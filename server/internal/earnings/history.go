package earnings

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Sprint F #3. History support for the PEAD (Post-Earnings
// Announcement Drift) sleeve. The forward-looking Snapshot /
// Event / Fetcher above answers "what earnings are coming"; this
// file answers "what already happened, and by how much did the
// company beat / miss". PEAD is the Bernard-Thomas 1989 finding
// that stocks drift in the direction of their earnings surprise
// for ~60 days after the print — one of the most replicated
// (and slowest-decaying) anomalies in academic finance.

// HistoricalEvent is one recently-released earnings record. We
// intentionally model only the fields PEAD actually uses; the
// full vendor payloads carry dozens of extra fields (EBITDA, GAAP
// adjustments, segment breakdowns) that are out of scope here.
type HistoricalEvent struct {
	// Symbol is the upper-cased ticker the event applies to.
	Symbol string
	// Market is the lower-cased market hint, mirroring Event.
	Market string
	// EventDate is the calendar day the print landed. UTC.
	EventDate time.Time
	// EpsActual is the reported EPS for the quarter. Zero is
	// "not reported".
	EpsActual float64
	// EpsEstimate is the consensus / analyst-mean EPS prior to
	// the release. Zero is "not reported"; non-zero with
	// EpsActual = 0 is a partial datum (we keep the row but
	// SurprisePercent stays zero).
	EpsEstimate float64
	// SurprisePercent is the standard (actual - estimate) /
	// |estimate| metric, expressed as a decimal (0.05 = 5%
	// beat). Some vendors carry it pre-computed; we accept
	// either pre-computed OR derive from EpsActual / EpsEstimate
	// inside the PEAD service. Positive = beat, negative = miss.
	SurprisePercent float64
	// Source is the vendor tag (e.g. "yahoo") for the audit
	// log — mirrors the forward-calendar Event.Source.
	Source string
}

// HistoryFetcher is the per-source adapter for historical
// earnings rows. Mirrors the forward Fetcher above. Real-world
// implementations:
//
//   - NoopHistoryFetcher (this file)       — default; no events.
//   - StaticHistoryFetcher (this file)     — operator-seeded slice.
//   - YahooHistoryProvider                 — Yahoo Finance v10
//                                            earningsHistory module.
//
// Returns the complete recent-history slice the back-end knows
// about for the requested symbols; the per-call horizon filter is
// applied upstream so back-end authors don't have to remember.
type HistoryFetcher interface {
	FetchHistory(ctx context.Context, req HistoryRequest) ([]HistoricalEvent, error)
}

// HistoryRequest names the per-call query: which symbols, which
// market, and how far back. LookbackDays maps directly to the
// PEAD drift window we care about (≈ 60 days in the canonical
// literature; we let the service own the default so back-ends
// can ignore the field and let the upstream filter run).
type HistoryRequest struct {
	Symbols      []string
	Market       string
	LookbackDays int
}

// HistorySnapshot is the per-fund result keyed on upper-cased
// symbol. PEAD only ever needs the MOST RECENT print per symbol
// (older quarters' drift is fully realised); a snapshot therefore
// stores a single HistoricalEvent per symbol rather than a slice.
type HistorySnapshot struct {
	AsOf         time.Time
	LookbackDays int
	PerSymbol    map[string]HistoricalEvent
}

// HasSignal reports whether the snapshot carries any usable
// information. False on empty snapshots so the wiring layer can
// drop the block from the prompt entirely.
func (s *HistorySnapshot) HasSignal() bool {
	return s != nil && len(s.PerSymbol) > 0
}

// SortedEvents returns the snapshot's events sorted by
// (EventDate descending, Symbol). Useful for tests and for the
// prompt builder which wants newest-first.
func (s *HistorySnapshot) SortedEvents() []HistoricalEvent {
	if s == nil || len(s.PerSymbol) == 0 {
		return nil
	}
	out := make([]HistoricalEvent, 0, len(s.PerSymbol))
	for _, e := range s.PerSymbol {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EventDate.Equal(out[j].EventDate) {
			return out[i].EventDate.After(out[j].EventDate)
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out
}

// HistoryOptions configures HistoryService.
type HistoryOptions struct {
	// Now is the clock function. Default time.Now (UTC normalised
	// inside the service).
	Now func() time.Time
	// LookbackDays is the trailing window the service surfaces.
	// 60 days covers the canonical PEAD drift horizon (Bernard
	// & Thomas 1989; Sadka 2006); after ~60 days the drift is
	// fully realised in academic samples.
	LookbackDays int
}

// HistoryService is the per-process orchestrator for historical
// earnings. Mirrors Service exactly so the wiring layer can
// construct one alongside the forward calendar.
type HistoryService struct {
	fetcher      HistoryFetcher
	now          func() time.Time
	lookbackDays int
}

// NewHistoryService wires a HistoryService. A nil fetcher is
// tolerated and silently swapped to NoopHistoryFetcher so the
// rest of the wiring layer can always construct the service.
func NewHistoryService(fetcher HistoryFetcher, opts HistoryOptions) *HistoryService {
	s := &HistoryService{
		fetcher:      fetcher,
		now:          opts.Now,
		lookbackDays: opts.LookbackDays,
	}
	if s.fetcher == nil {
		s.fetcher = NoopHistoryFetcher{}
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.lookbackDays <= 0 {
		s.lookbackDays = 60
	}
	return s
}

// LookbackDays exposes the resolved horizon so callers can echo
// it back to the prompt for transparency.
func (s *HistoryService) LookbackDays() int {
	if s == nil {
		return 0
	}
	return s.lookbackDays
}

// Build runs the per-fund historical earnings query. Returns nil
// on every failure path — PEAD is a soft input; we'd rather omit
// the block than fail the decision.
func (s *HistoryService) Build(ctx context.Context, symbols []string, market string) *HistorySnapshot {
	if s == nil {
		return nil
	}
	now := s.now().UTC()
	req := HistoryRequest{
		Symbols:      normaliseSymbols(symbols),
		Market:       strings.ToLower(strings.TrimSpace(market)),
		LookbackDays: s.lookbackDays,
	}
	if len(req.Symbols) == 0 {
		return nil
	}
	events, err := s.fetcher.FetchHistory(ctx, req)
	if err != nil || len(events) == 0 {
		return nil
	}
	floor := now.AddDate(0, 0, -s.lookbackDays)
	perSymbol := make(map[string]HistoricalEvent, len(events))
	for _, e := range events {
		sym := strings.ToUpper(strings.TrimSpace(e.Symbol))
		if sym == "" {
			continue
		}
		// Filter: events in [now - lookback, now]. Strictly in
		// the past (today's announcement has no drift yet).
		if e.EventDate.IsZero() || e.EventDate.Before(floor) || e.EventDate.After(now) {
			continue
		}
		// Keep the MOST RECENT print per symbol (PEAD only cares
		// about the latest catalyst).
		existing, ok := perSymbol[sym]
		if !ok || e.EventDate.After(existing.EventDate) {
			normalised := e
			normalised.Symbol = sym
			normalised.Market = strings.ToLower(strings.TrimSpace(e.Market))
			perSymbol[sym] = normalised
		}
	}
	if len(perSymbol) == 0 {
		return nil
	}
	return &HistorySnapshot{
		AsOf:         now,
		LookbackDays: s.lookbackDays,
		PerSymbol:    perSymbol,
	}
}

// ---------------------------------------------------------------------------
// Built-in history fetchers
// ---------------------------------------------------------------------------

// NoopHistoryFetcher returns no events. Default for funds that
// haven't wired a history provider — the block is silently
// absent from the prompt.
type NoopHistoryFetcher struct{}

// FetchHistory implements HistoryFetcher.
func (NoopHistoryFetcher) FetchHistory(_ context.Context, _ HistoryRequest) ([]HistoricalEvent, error) {
	return nil, nil
}

// StaticHistoryFetcher returns a fixed slice of events. Useful
// for tests AND for hand-curated A-share / HK earnings overlays.
type StaticHistoryFetcher struct {
	Events []HistoricalEvent
}

// FetchHistory implements HistoryFetcher.
func (f StaticHistoryFetcher) FetchHistory(_ context.Context, _ HistoryRequest) ([]HistoricalEvent, error) {
	return f.Events, nil
}
