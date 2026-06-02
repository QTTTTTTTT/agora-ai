package modelab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fundai/server/internal/llm"
)

// ConfigChatClient is the narrow interface the dispatcher needs
// from the underlying LLM client to fire shadow arms with a
// pre-built ModelConfig. *llm.MultiProviderClient satisfies it;
// tests can supply a stub.
type ConfigChatClient interface {
	ChatWithConfig(ctx context.Context, req llm.ChatRequest, cfg *llm.ModelConfig) (*llm.ChatResponse, error)
}

// ShadowDispatcher is the Sprint 10.2 wrapper that turns the
// single-call llm.LLMClient into a multi-arm dispatcher. On every
// Chat call:
//
//  1. It asks the Resolver whether the call belongs to an
//     experiment. If not, it delegates to Inner unchanged — the
//     hot path is identical to pre-10.x behaviour.
//  2. If yes, it runs the WINNING arm (the one PickArm returned)
//     through Inner.Chat exactly as before — that response is
//     what gets returned to the caller, AND it's the only one
//     that steers production. The router's hook (installed in
//     S10.1) ensures the winning arm's model is the one Inner
//     uses.
//  3. In parallel, it fires the OTHER arms against a config
//     materialised from each arm's spec via BuildLLMConfig +
//     llm.MultiProviderClient.ChatWithConfig. These responses
//     are persisted to model_ab_shadow_responses but never
//     surface to the caller.
//
// Key safety properties:
//   - The primary call always returns to the caller, even if
//     all shadow arms fail.
//   - Shadow arms run with a bounded timeout (default 30s) and
//     a process-wide semaphore (default 8 concurrent) so a
//     misbehaving experiment can't drown the platform.
//   - Token-budget guard: experiments with MaxTotalTokens are
//     skipped once cumulative tokens used exceeds the cap.
//   - Inner is required to be a *llm.MultiProviderClient (NOT
//     a generic llm.LLMClient) because shadow arms need
//     ChatWithConfig to bypass the router. Constructing a
//     dispatcher with a generic client returns the original
//     client semantics — every call goes through Inner.Chat
//     and shadow fan-out is silently disabled.
//
// Concurrency model: the primary call runs SYNCHRONOUSLY on the
// calling goroutine. Shadow arms launch in goroutines, BUT we
// wait for them to finish before returning — this gives us
// predictable timing for both production traffic and unit
// tests. Operators who need fire-and-forget can wrap the
// dispatcher themselves; the in-band variant is the safer
// default.
type ShadowDispatcher struct {
	// Inner is the underlying LLM client. When it also satisfies
	// ConfigChatClient (typically *llm.MultiProviderClient) we
	// use ChatWithConfig for shadow arms; otherwise shadow fan-
	// out is disabled and every call delegates straight to
	// Inner.Chat — useful for tests and degraded environments.
	Inner llm.LLMClient

	// ConfigClient is the handle used for shadow arm dispatch.
	// Set automatically when NewShadowDispatcher is passed a
	// client that implements ConfigChatClient. nil disables
	// fan-out.
	ConfigClient ConfigChatClient

	// Resolver decides which experiment / arm a request maps
	// to. nil disables A/B (delegate everything to Inner).
	Resolver *Resolver

	// Repo persists shadow responses. nil disables persistence
	// (the dispatcher still fans out — useful for benchmarking
	// without DB pressure — but no rows are written).
	Repo *Repo

	// HookContext fills BuildLLMConfig defaults. Reuses the
	// router's system keys + tier defaults so shadow arms call
	// the same endpoints / cost the same as the primary path.
	HookContext HookContext

	// MaxConcurrentShadowCalls caps how many in-flight shadow
	// calls the dispatcher will allow across the whole process.
	// Defaults to 8. Set to 0 to use the default.
	MaxConcurrentShadowCalls int

	// ShadowTimeout bounds a single shadow arm's wall-clock.
	// Defaults to 30s.
	ShadowTimeout time.Duration

	// Logger receives structured events. nil → slog.Default.
	Logger *slog.Logger

	// sem is the lazy-initialised semaphore for shadow
	// concurrency. Owned by the dispatcher's first Chat call.
	semOnce sync.Once
	sem     chan struct{}
}

// NewShadowDispatcher constructs a dispatcher. Pass the
// production *MultiProviderClient as inner — passing a generic
// LLMClient still works but disables shadow fan-out, which is
// useful for tests that don't care about A/B.
func NewShadowDispatcher(inner llm.LLMClient, resolver *Resolver, repo *Repo, hc HookContext) *ShadowDispatcher {
	d := &ShadowDispatcher{
		Inner:                    inner,
		Resolver:                 resolver,
		Repo:                     repo,
		HookContext:              hc,
		MaxConcurrentShadowCalls: 8,
		ShadowTimeout:            30 * time.Second,
	}
	if cc, ok := inner.(ConfigChatClient); ok {
		d.ConfigClient = cc
	}
	return d
}

// Chat implements llm.LLMClient. See ShadowDispatcher godoc.
func (d *ShadowDispatcher) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if d == nil || d.Inner == nil {
		return nil, errors.New("modelab: ShadowDispatcher has no inner client")
	}
	// No resolver / config client → straight passthrough.
	if d.Resolver == nil || d.ConfigClient == nil {
		return d.Inner.Chat(ctx, req)
	}
	role := SanitizeAgentRole(req.AgentRole)
	decision := d.Resolver.Resolve(ctx, req.FundID, req.AgentID, role, req.StepName, req.RunID)
	if !decision.InExperiment || decision.Experiment == nil {
		return d.Inner.Chat(ctx, req)
	}

	// Fire shadow arms in parallel. The PRIMARY arm goes
	// through Inner.Chat normally (the router's hook will
	// route it to the right arm) — that response is what we
	// return to the caller.
	wg := &sync.WaitGroup{}
	for idx, arm := range decision.Experiment.Arms {
		if idx == decision.ArmIndex {
			continue // primary arm handled below
		}
		idx, arm := idx, arm
		wg.Add(1)
		d.acquire()
		go func() {
			defer wg.Done()
			defer d.release()
			d.runShadowArm(ctx, req, decision, idx, arm)
		}()
	}

	resp, err := d.Inner.Chat(ctx, req)
	wg.Wait()
	return resp, err
}

// ListModels implements llm.LLMClient by delegating to Inner.
func (d *ShadowDispatcher) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	if d == nil || d.Inner == nil {
		return nil, errors.New("modelab: ShadowDispatcher has no inner client")
	}
	return d.Inner.ListModels(ctx)
}

// runShadowArm performs one shadow call AND persists the result
// (or the error) into model_ab_shadow_responses. Errors are
// logged but never propagated — the primary call's correctness
// is independent of shadow execution.
func (d *ShadowDispatcher) runShadowArm(_ context.Context, req llm.ChatRequest, decision Decision, armIdx int, arm ArmConfig) {
	if d.ConfigClient == nil || arm.Validate() != nil {
		return
	}

	timeout := d.ShadowTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// Detach from the parent context's cancellation: when the
	// primary call returns to the caller, the parent ctx is
	// often cancelled immediately (e.g. handler returns) which
	// would abort the shadow call before we can persist
	// results. We use Background + our own timeout so the
	// shadow has a chance to finish AND its result reaches the
	// DB.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// Carry over relevant ctx values (request ID, trace) if any.
	// For now we don't — slog records its own field.

	cfg := BuildLLMConfig(arm, d.HookContext)
	shadowReq := req
	shadowReq.Model = arm.ModelName
	// Force the router to skip its own hook for the shadow call
	// (we already built the config). Even though ChatWithConfig
	// bypasses ResolveModel, setting Model here keeps any logs
	// / caches keyed on the shadow model so we don't poison the
	// primary's cache entries.

	startedAt := time.Now()
	resp, err := d.ConfigClient.ChatWithConfig(ctx, shadowReq, cfg)
	latency := time.Since(startedAt)

	row := &ShadowResponse{
		ExperimentID: decision.Experiment.ID,
		RunID:        req.RunID,
		Step:         req.StepName,
		AgentID:      req.AgentID,
		FundID:       req.FundID,
		ArmIndex:     armIdx,
		ArmName:      arm.Name,
		ArmModel:     arm.Label(),
		LatencyMs:    int(latency.Milliseconds()),
	}
	if decision.Assignment != nil {
		row.AssignmentID = decision.Assignment.ID
	}
	if err != nil {
		row.ErrorText = err.Error()
		d.logger().Warn("modelab.shadow_arm_failed",
			"experiment_id", decision.Experiment.ID,
			"arm_index", armIdx,
			"arm_label", arm.Label(),
			"err", err,
		)
	} else if resp != nil {
		row.RawOutput = resp.Content
		row.InputTokens = resp.InputTokens
		row.OutputTokens = resp.OutputTokens
		row.CostMicro = int64(resp.TotalCost * 1_000_000)
		if parsed, perr := tryParseJSONOutput(resp.Content); perr == nil {
			row.ParsedOutput = parsed
		} else {
			row.ParseError = perr.Error()
		}
	}

	if d.Repo == nil || row.AssignmentID == "" {
		// No persistence wired OR no sticky-arm row to bind to.
		// We log instead so the engineer can see the shadow ran.
		d.logger().Debug("modelab.shadow_response_dropped",
			"reason", "no_repo_or_assignment",
			"experiment_id", decision.Experiment.ID,
			"arm_label", arm.Label(),
		)
		return
	}
	if insErr := d.Repo.InsertShadowResponse(ctx, row); insErr != nil {
		d.logger().Warn("modelab.shadow_response_insert_failed",
			"experiment_id", decision.Experiment.ID,
			"arm_label", arm.Label(),
			"err", insErr,
		)
		return
	}
	// Best-effort token cap bookkeeping. We add OUTPUT tokens
	// only because input is shared across arms (the prompt is
	// identical) and counting input N times would inflate the
	// budget consumption.
	if resp != nil && resp.OutputTokens > 0 {
		if addErr := d.Repo.AddTokens(ctx, decision.Experiment.ID, int64(resp.OutputTokens)); addErr != nil {
			d.logger().Debug("modelab.add_tokens_failed",
				"experiment_id", decision.Experiment.ID,
				"err", addErr,
			)
		}
	}
}

func (d *ShadowDispatcher) acquire() {
	d.semOnce.Do(func() {
		n := d.MaxConcurrentShadowCalls
		if n <= 0 {
			n = 8
		}
		d.sem = make(chan struct{}, n)
	})
	if d.sem != nil {
		d.sem <- struct{}{}
	}
}

func (d *ShadowDispatcher) release() {
	if d.sem != nil {
		<-d.sem
	}
}

func (d *ShadowDispatcher) logger() *slog.Logger {
	if d == nil || d.Logger == nil {
		return slog.Default()
	}
	return d.Logger
}

// tryParseJSONOutput attempts to parse the LLM output as JSON
// so the report layer (S10.3) can do structural comparisons
// (e.g. "did arm A and arm B propose the same Stance?"). Non-
// JSON outputs are kept as raw text — the parser failure is
// recorded in ShadowResponse.ParseError.
func tryParseJSONOutput(raw string) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty output")
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}
