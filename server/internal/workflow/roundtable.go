package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Domain types (local mirrors)
// ---------------------------------------------------------------------------

// Roundtable captures the full lifecycle of a multi-agent discussion session.
type Roundtable struct {
	ID        string
	FundID    string
	Date      string
	Rounds    []RoundtableRound
	Consensus []ConsensusItem
	Status    string // "active", "completed", "timeout"
	StartedAt time.Time
	EndedAt   time.Time
}

// RoundtableRound holds the results of a single discussion round.
type RoundtableRound struct {
	RoundNumber int
	Opinions    []ResearcherOpinion
	Summary     string
	Unresolved  []string
}

// ResearcherOpinion is one researcher's position on a single symbol/topic.
type ResearcherOpinion struct {
	AgentID    string
	AgentName  string
	Focus      string // "stock", "fundamental", "macro"
	Symbol     string
	Direction  string // "bullish", "bearish", "neutral"
	Confidence int
	Reasoning  string
	DataPoints []string
}

// ConsensusItem records the final agreed-upon position for a symbol.
type ConsensusItem struct {
	Symbol     string
	Direction  string
	Confidence int
	Supporters []string
	Dissenters []string
	Action     string // "buy", "sell", "hold", "watch"
	Reasoning  string
}

// ---------------------------------------------------------------------------
// Dependency interfaces
// ---------------------------------------------------------------------------

// ResearcherAgent represents an AI researcher that can produce opinions.
type ResearcherAgent interface {
	// Metadata
	AgentID() string
	AgentName() string
	Focus() string // "stock", "fundamental", "macro"

	// ProduceOpinion generates an opinion for the given topic, optionally
	// informed by the previous round's opinions for context.
	ProduceOpinion(ctx context.Context, topic string, previousRound *RoundtableRound) (ResearcherOpinion, error)
}

// LLMSummarizer uses an LLM to summarise rounds and extract consensus.
type LLMSummarizer interface {
	// SummarizeRound produces a human-readable summary of a round and
	// returns the list of topics that remain unresolved.
	SummarizeRound(ctx context.Context, round RoundtableRound) (summary string, unresolved []string, err error)

	// ExtractConsensus inspects all completed rounds and returns the final
	// set of consensus items.
	ExtractConsensus(ctx context.Context, allRounds []RoundtableRound) ([]ConsensusItem, error)
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// RoundtableEventKind enumerates the event types emitted during a session.
type RoundtableEventKind int

const (
	EventRoundtableStarted RoundtableEventKind = iota
	EventRoundStarted
	EventOpinionReceived
	EventRoundCompleted
	EventConsensusReached
	EventRoundtableCompleted
	EventRoundtableTimeout
)

// RoundtableEvent is published on every notable state transition.
type RoundtableEvent struct {
	Kind        RoundtableEventKind
	RoundtableID string
	Round       int
	Payload     any // type depends on Kind
	Timestamp   time.Time
}

// EventListener receives events emitted by the engine. Implementations must
// not block; the engine calls listeners synchronously on the hot path.
type EventListener func(RoundtableEvent)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const (
	defaultMaxRounds       = 3
	defaultRoundTimeout    = 5 * time.Minute
	defaultMaxConcurrency  = 6
	consensusThreshold     = 2 // numerator of the 2/3 super-majority fraction
	consensusDivisor       = 3 // denominator
)

// RoundtableConfig holds tunables for the engine.
type RoundtableConfig struct {
	MaxRounds      int
	RoundTimeout   time.Duration
	MaxConcurrency int

	// DebateMode, when true, replaces the legacy direction-vote
	// algorithmic consensus with a structured DebateGraph: each
	// opinion becomes a claim, supports/rebuts edges are inferred
	// (within-round agreement/contradiction + cross-round self
	// rebuttals), and the verdict is derived from net argument
	// strength rather than raw majority. Has no effect on the LLM
	// path — if the summarizer succeeds, its consensus still wins.
	DebateMode bool
}

func (c RoundtableConfig) maxRounds() int {
	if c.MaxRounds > 0 {
		return c.MaxRounds
	}
	return defaultMaxRounds
}

func (c RoundtableConfig) roundTimeout() time.Duration {
	if c.RoundTimeout > 0 {
		return c.RoundTimeout
	}
	return defaultRoundTimeout
}

func (c RoundtableConfig) maxConcurrency() int {
	if c.MaxConcurrency > 0 {
		return c.MaxConcurrency
	}
	return defaultMaxConcurrency
}

// ---------------------------------------------------------------------------
// RoundtableEngine
// ---------------------------------------------------------------------------

// RoundtableEngine orchestrates multi-round researcher discussions moderated
// by an LLM-backed PM agent.
type RoundtableEngine struct {
	researchers []ResearcherAgent
	summarizer  LLMSummarizer
	cfg         RoundtableConfig
	logger      *slog.Logger

	mu        sync.RWMutex
	listeners []EventListener
}

// NewRoundtableEngine creates a ready-to-use engine. At least one researcher
// and a summarizer are required.
func NewRoundtableEngine(
	researchers []ResearcherAgent,
	summarizer LLMSummarizer,
	cfg RoundtableConfig,
	logger *slog.Logger,
) (*RoundtableEngine, error) {
	if len(researchers) == 0 {
		return nil, fmt.Errorf("roundtable: at least one researcher agent is required")
	}
	if summarizer == nil {
		return nil, fmt.Errorf("roundtable: summarizer must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RoundtableEngine{
		researchers: researchers,
		summarizer:  summarizer,
		cfg:         cfg,
		logger:      logger,
	}, nil
}

// OnEvent registers a listener that will be called for every event.
func (e *RoundtableEngine) OnEvent(fn EventListener) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.listeners = append(e.listeners, fn)
}

// emit dispatches an event to all registered listeners.
func (e *RoundtableEngine) emit(evt RoundtableEvent) {
	e.mu.RLock()
	listeners := append([]EventListener(nil), e.listeners...)
	e.mu.RUnlock()
	for _, fn := range listeners {
		go fn(evt)
	}
}

// ---------------------------------------------------------------------------
// Run — main entry point
// ---------------------------------------------------------------------------

// Run executes the full roundtable discussion and returns the completed
// Roundtable struct. The supplied context controls overall cancellation;
// per-round timeouts are layered on top.
func (e *RoundtableEngine) Run(ctx context.Context, id, fundID, date string, initialTopics []string) (*Roundtable, error) {
	rt := &Roundtable{
		ID:        id,
		FundID:    fundID,
		Date:      date,
		Status:    "active",
		StartedAt: time.Now(),
	}

	e.logger.Info("roundtable started",
		slog.String("id", id),
		slog.String("fund", fundID),
		slog.String("date", date),
		slog.Int("researchers", len(e.researchers)),
	)
	e.emit(RoundtableEvent{
		Kind:         EventRoundtableStarted,
		RoundtableID: id,
		Timestamp:    rt.StartedAt,
	})

	maxRounds := e.cfg.maxRounds()
	topics := make([]string, len(initialTopics))
	copy(topics, initialTopics)

	for roundNum := 1; roundNum <= maxRounds; roundNum++ {
		select {
		case <-ctx.Done():
			rt.Status = "timeout"
			rt.EndedAt = time.Now()
			e.emit(RoundtableEvent{
				Kind:         EventRoundtableTimeout,
				RoundtableID: id,
				Timestamp:    rt.EndedAt,
			})
			e.logger.Warn("roundtable cancelled by context", slog.String("id", id))
			return rt, ctx.Err()
		default:
		}

		round, err := e.executeRound(ctx, rt, roundNum, topics)
		if err != nil {
			// If the context was cancelled during the round treat it as
			// a timeout rather than a hard failure.
			if ctx.Err() != nil {
				rt.Status = "timeout"
				rt.EndedAt = time.Now()
				e.emit(RoundtableEvent{
					Kind:         EventRoundtableTimeout,
					RoundtableID: id,
					Timestamp:    rt.EndedAt,
				})
				return rt, ctx.Err()
			}
			return rt, fmt.Errorf("roundtable %s: round %d failed: %w", id, roundNum, err)
		}

		rt.Rounds = append(rt.Rounds, *round)

		// After the first round, extract topics from opinions so that
		// subsequent rounds can narrow focus to unresolved symbols.
		if roundNum == 1 && len(topics) == 0 {
			topics = extractTopics(round.Opinions)
			e.logger.Info("topics extracted from first round",
				slog.String("id", id),
				slog.Any("topics", topics),
			)
		}

		// Check algorithmic consensus before LLM extraction so we can
		// exit early when the numbers already agree.
		if e.allTopicsResolved(round) {
			e.logger.Info("all topics resolved by algorithmic check",
				slog.String("id", id),
				slog.Int("round", roundNum),
			)
			break
		}

		// Narrow topics to unresolved items for the next round.
		if len(round.Unresolved) > 0 {
			topics = round.Unresolved
		}
	}

	// --- Final consensus extraction via LLM ---
	consensus, err := e.extractFinalConsensus(ctx, rt)
	if err != nil {
		if e.cfg.DebateMode {
			e.logger.Error("consensus extraction failed, falling back to debate graph",
				slog.String("id", id),
				slog.String("err", err.Error()),
			)
			consensus = BuildDebateGraph(rt).ToConsensus()
		} else {
			e.logger.Error("consensus extraction failed, falling back to algorithmic",
				slog.String("id", id),
				slog.String("err", err.Error()),
			)
			consensus = e.algorithmicConsensus(rt)
		}
	}
	rt.Consensus = consensus

	if rt.Status == "active" {
		rt.Status = "completed"
	}
	rt.EndedAt = time.Now()

	e.emit(RoundtableEvent{
		Kind:         EventRoundtableCompleted,
		RoundtableID: id,
		Payload:      consensus,
		Timestamp:    rt.EndedAt,
	})
	e.logger.Info("roundtable completed",
		slog.String("id", id),
		slog.Int("rounds", len(rt.Rounds)),
		slog.Int("consensus_items", len(consensus)),
	)

	return rt, nil
}

// ---------------------------------------------------------------------------
// Round execution
// ---------------------------------------------------------------------------

func (e *RoundtableEngine) executeRound(
	ctx context.Context,
	rt *Roundtable,
	roundNum int,
	topics []string,
) (*RoundtableRound, error) {
	roundCtx, cancel := context.WithTimeout(ctx, e.cfg.roundTimeout())
	defer cancel()

	e.logger.Info("round started",
		slog.String("id", rt.ID),
		slog.Int("round", roundNum),
		slog.Any("topics", topics),
	)
	e.emit(RoundtableEvent{
		Kind:         EventRoundStarted,
		RoundtableID: rt.ID,
		Round:        roundNum,
		Timestamp:    time.Now(),
	})

	// Determine the previous round (may be nil for round 1).
	var prevRound *RoundtableRound
	if len(rt.Rounds) > 0 {
		prevRound = &rt.Rounds[len(rt.Rounds)-1]
	}

	// Collect opinions from all researchers concurrently.
	opinions, err := e.collectOpinions(roundCtx, rt.ID, roundNum, topics, prevRound)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(roundCtx.Err(), context.DeadlineExceeded) {
		rt.Status = "timeout"
	}
	if err != nil {
		return nil, err
	}

	round := &RoundtableRound{
		RoundNumber: roundNum,
		Opinions:    opinions,
	}

	// Summarise via LLM.
	summary, unresolved, err := e.summarizer.SummarizeRound(roundCtx, *round)
	if err != nil {
		e.logger.Warn("LLM summarization failed, using algorithmic fallback",
			slog.String("id", rt.ID),
			slog.Int("round", roundNum),
			slog.String("err", err.Error()),
		)
		summary, unresolved = e.fallbackSummarize(round)
	}
	round.Summary = summary
	round.Unresolved = unresolved

	e.emit(RoundtableEvent{
		Kind:         EventRoundCompleted,
		RoundtableID: rt.ID,
		Round:        roundNum,
		Payload:      round,
		Timestamp:    time.Now(),
	})
	e.logger.Info("round completed",
		slog.String("id", rt.ID),
		slog.Int("round", roundNum),
		slog.Int("opinions", len(opinions)),
		slog.Int("unresolved", len(unresolved)),
	)

	return round, nil
}

// ---------------------------------------------------------------------------
// Opinion collection (concurrent, per-topic fan-out)
// ---------------------------------------------------------------------------

type opinionResult struct {
	opinion ResearcherOpinion
	err     error
}

func (e *RoundtableEngine) collectOpinions(
	ctx context.Context,
	rtID string,
	roundNum int,
	topics []string,
	prevRound *RoundtableRound,
) ([]ResearcherOpinion, error) {
	// If no explicit topics, send an empty topic so each researcher can
	// surface whatever they consider relevant.
	if len(topics) == 0 {
		topics = []string{""}
	}

	totalCalls := len(e.researchers) * len(topics)
	results := make(chan opinionResult, totalCalls)
	semaphore := make(chan struct{}, e.cfg.maxConcurrency())

	var wg sync.WaitGroup
	wg.Add(totalCalls)

	for _, r := range e.researchers {
		for _, topic := range topics {
			go func(agent ResearcherAgent, t string) {
				defer wg.Done()
				select {
				case <-ctx.Done():
					results <- opinionResult{err: ctx.Err()}
					return
				case semaphore <- struct{}{}:
				}
				defer func() { <-semaphore }()

				op, err := agent.ProduceOpinion(ctx, t, prevRound)
				if err != nil {
					e.logger.Warn("researcher opinion failed",
						slog.String("roundtable", rtID),
						slog.Int("round", roundNum),
						slog.String("agent", agent.AgentID()),
						slog.String("topic", t),
						slog.String("err", err.Error()),
					)
				}
				results <- opinionResult{opinion: op, err: err}
			}(r, topic)
		}
	}

	// Close the channel once every goroutine has finished.
	go func() {
		wg.Wait()
		close(results)
	}()

	var opinions []ResearcherOpinion
	var firstErr error
	received := 0

	for res := range results {
		received++
		if res.err != nil {
			if firstErr == nil {
				firstErr = res.err
			}
			continue
		}
		// Clamp confidence to [0, 100].
		if res.opinion.Confidence < 0 {
			res.opinion.Confidence = 0
		} else if res.opinion.Confidence > 100 {
			res.opinion.Confidence = 100
		}
		opinions = append(opinions, res.opinion)

		e.emit(RoundtableEvent{
			Kind:         EventOpinionReceived,
			RoundtableID: rtID,
			Round:        roundNum,
			Payload:      res.opinion,
			Timestamp:    time.Now(),
		})
	}

	// We require at least one valid opinion to proceed.
	if len(opinions) == 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("roundtable %s round %d: all opinions failed, first error: %w", rtID, roundNum, firstErr)
		}
		return nil, fmt.Errorf("roundtable %s round %d: no opinions collected", rtID, roundNum)
	}

	return opinions, nil
}

// ---------------------------------------------------------------------------
// Topic extraction
// ---------------------------------------------------------------------------

// extractTopics derives unique topics (symbols) from the first round's
// opinions so that follow-up rounds can be narrowly focused.
func extractTopics(opinions []ResearcherOpinion) []string {
	seen := make(map[string]struct{}, len(opinions))
	var topics []string
	for _, o := range opinions {
		if o.Symbol == "" {
			continue
		}
		if _, ok := seen[o.Symbol]; ok {
			continue
		}
		seen[o.Symbol] = struct{}{}
		topics = append(topics, o.Symbol)
	}
	return topics
}

// ---------------------------------------------------------------------------
// Consensus detection (algorithmic, 2/3 threshold)
// ---------------------------------------------------------------------------

// directionTally tracks votes per direction for a single symbol.
type directionTally struct {
	votes map[string][]string // direction → list of agent IDs
	total int
}

// tallyBySymbol groups opinions in a round by symbol and direction.
func tallyBySymbol(opinions []ResearcherOpinion) map[string]*directionTally {
	m := make(map[string]*directionTally)
	for _, o := range opinions {
		if o.Symbol == "" {
			continue
		}
		t, ok := m[o.Symbol]
		if !ok {
			t = &directionTally{votes: make(map[string][]string)}
			m[o.Symbol] = t
		}
		t.votes[o.Direction] = append(t.votes[o.Direction], o.AgentID)
		t.total++
	}
	return m
}

// hasSupermajority returns true when at least 2/3 of voters agree on a
// single direction for the given symbol.
func hasSupermajority(t *directionTally) (direction string, ok bool) {
	threshold := (t.total*consensusThreshold + consensusDivisor - 1) / consensusDivisor // ceil
	for dir, agents := range t.votes {
		if len(agents) >= threshold {
			return dir, true
		}
	}
	return "", false
}

// allTopicsResolved returns true when every symbol discussed in the round
// has a 2/3 super-majority on direction.
func (e *RoundtableEngine) allTopicsResolved(round *RoundtableRound) bool {
	tallies := tallyBySymbol(round.Opinions)
	if len(tallies) == 0 {
		return false
	}
	for sym, t := range tallies {
		if _, ok := hasSupermajority(t); !ok {
			e.logger.Debug("symbol still unresolved",
				slog.String("symbol", sym),
				slog.Int("voters", t.total),
			)
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Algorithmic consensus builder (fallback when LLM fails)
// ---------------------------------------------------------------------------

func (e *RoundtableEngine) algorithmicConsensus(rt *Roundtable) []ConsensusItem {
	if len(rt.Rounds) == 0 {
		return nil
	}

	// Use the last round as the source of truth.
	lastRound := rt.Rounds[len(rt.Rounds)-1]
	tallies := tallyBySymbol(lastRound.Opinions)

	var items []ConsensusItem
	for sym, t := range tallies {
		dir, ok := hasSupermajority(t)
		if !ok {
			// Pick the plurality direction.
			dir = pluralityDirection(t)
		}

		supporters, dissenters := partitionAgents(t, dir)
		avgConf := averageConfidence(lastRound.Opinions, sym, dir)
		action := directionToAction(dir, avgConf)
		reasoning := buildAlgorithmicReasoning(lastRound.Opinions, sym, dir)

		items = append(items, ConsensusItem{
			Symbol:     sym,
			Direction:  dir,
			Confidence: avgConf,
			Supporters: supporters,
			Dissenters: dissenters,
			Action:     action,
			Reasoning:  reasoning,
		})

		e.emit(RoundtableEvent{
			Kind:         EventConsensusReached,
			RoundtableID: rt.ID,
			Payload: map[string]any{
				"symbol":    sym,
				"direction": dir,
				"action":    action,
			},
			Timestamp: time.Now(),
		})
	}
	return items
}

func pluralityDirection(t *directionTally) string {
	best, bestCount := "", 0
	for dir, agents := range t.votes {
		if len(agents) > bestCount {
			best = dir
			bestCount = len(agents)
		}
	}
	return best
}

func partitionAgents(t *directionTally, winDir string) (supporters, dissenters []string) {
	for dir, agents := range t.votes {
		if dir == winDir {
			supporters = append(supporters, agents...)
		} else {
			dissenters = append(dissenters, agents...)
		}
	}
	return
}

func averageConfidence(opinions []ResearcherOpinion, symbol, direction string) int {
	sum, count := 0, 0
	for _, o := range opinions {
		if o.Symbol == symbol && o.Direction == direction {
			sum += o.Confidence
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / count
}

func directionToAction(direction string, confidence int) string {
	switch direction {
	case "bullish":
		if confidence >= 70 {
			return "buy"
		}
		return "watch"
	case "bearish":
		if confidence >= 70 {
			return "sell"
		}
		return "watch"
	default:
		return "hold"
	}
}

func buildAlgorithmicReasoning(opinions []ResearcherOpinion, symbol, direction string) string {
	reasoning := fmt.Sprintf("Algorithmic consensus for %s (%s): ", symbol, direction)
	first := true
	for _, o := range opinions {
		if o.Symbol != symbol {
			continue
		}
		if !first {
			reasoning += "; "
		}
		first = false
		tag := "agrees"
		if o.Direction != direction {
			tag = "disagrees"
		}
		reasoning += fmt.Sprintf("%s (%s, confidence %d%%) %s", o.AgentName, o.Focus, o.Confidence, tag)
	}
	return reasoning
}

// ---------------------------------------------------------------------------
// Fallback summarization (when LLM is unavailable)
// ---------------------------------------------------------------------------

func (e *RoundtableEngine) fallbackSummarize(round *RoundtableRound) (summary string, unresolved []string) {
	tallies := tallyBySymbol(round.Opinions)

	summary = fmt.Sprintf("Round %d: %d opinions across %d symbols. ",
		round.RoundNumber, len(round.Opinions), len(tallies))

	for sym, t := range tallies {
		dir, ok := hasSupermajority(t)
		if ok {
			summary += fmt.Sprintf("%s: consensus %s. ", sym, dir)
		} else {
			summary += fmt.Sprintf("%s: no consensus. ", sym)
			unresolved = append(unresolved, sym)
		}
	}

	return summary, unresolved
}

// ---------------------------------------------------------------------------
// LLM consensus extraction wrapper
// ---------------------------------------------------------------------------

func (e *RoundtableEngine) extractFinalConsensus(ctx context.Context, rt *Roundtable) ([]ConsensusItem, error) {
	extractCtx, cancel := context.WithTimeout(ctx, e.cfg.roundTimeout())
	defer cancel()

	items, err := e.summarizer.ExtractConsensus(extractCtx, rt.Rounds)
	if err != nil {
		return nil, err
	}

	// Emit individual consensus events.
	for _, item := range items {
		e.emit(RoundtableEvent{
			Kind:         EventConsensusReached,
			RoundtableID: rt.ID,
			Payload:      item,
			Timestamp:    time.Now(),
		})
	}

	return items, nil
}
