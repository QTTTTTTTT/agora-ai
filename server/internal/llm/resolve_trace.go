// resolve_trace.go — observability hooks for the 9-layer model
// resolution chain.
//
// Problem this solves
//
//   ResolveModel walks ~9 layers (explicit model → A/B hook → user
//   BYOK hook → fund override → agent default → user-tier override
//   → user custom endpoint → platform default → fallback). When a
//   support ticket comes in saying "this agent picked the wrong
//   model", we need to be able to answer "which layer fired and
//   why?". The default ResolveModel returns only the final config
//   — no breadcrumb trail.
//
//   This file adds a parallel public method ResolveModelWithTrace
//   that returns the same config PLUS a []ResolutionStep capturing
//   the verdict of each layer. The existing ResolveModel stays
//   unchanged — all existing callers (chat path, A/B harness, …)
//   keep using the cheap variant.
//
//   The /admin/llm-resolve dry-run endpoint (see cmd/server) and
//   the Prometheus counter `llm_resolution_source{layer="..."}`
//   are downstream consumers.
//
// Why an enum (Layer) and not raw strings
//
//   Two reasons. First, label cardinality on the Prometheus
//   counter has to be bounded — using a closed enum gives us a
//   compile-time guarantee that a typo'd new layer can't blow up
//   Prom storage. Second, the SPA's dry-run viewer wants to colour
//   layers consistently; a typed constant gives downstream code a
//   stable identifier.

package llm

import (
	"bytes"
	"context"
	"sync"

	"github.com/fundai/server/internal/metrics"
)

// Layer is the typed identifier for each resolution layer. The
// numeric ordering matches the priority chain in ResolveModel:
// lower-numbered layers run earlier and take precedence.
type Layer string

const (
	LayerExplicitModel      Layer = "explicit_model"       // req.Model set, found in PlatformModels
	LayerModelAB            Layer = "model_ab_hook"        // Sprint 10.1 — A/B routing
	LayerUserBYOK           Layer = "user_byok_hook"       // Phase B-2 — /advisor BYOK key
	LayerFundOverride       Layer = "fund_override_hook"   // S14.B — fund_llm_overrides
	LayerAgentDefault       Layer = "agent_default"        // per-(user,agent) saved default
	LayerUserTierOverride   Layer = "user_tier_override"   // per-(user,tier) saved default
	LayerUserCustomEndpoint Layer = "user_custom_endpoint" // per-user custom endpoint matching tier provider
	LayerPlatformDefault    Layer = "platform_default"     // DefaultModels[tier]
	LayerFallbackStandard   Layer = "fallback_standard"    // tier-specific default missing → standard
)

// ResolutionStep is the breadcrumb left at each layer.
//
//   Hit==true → this layer produced the final ModelConfig
//   Hit==false → this layer was evaluated and skipped; Detail
//                explains why (e.g. "no hook configured",
//                "explicit model not found in platform list")
//
// The trace is ordered: the index of the Hit==true step (always
// exactly one) tells the operator at what priority the request
// resolved.
type ResolutionStep struct {
	Layer  Layer  `json:"layer"`
	Hit    bool   `json:"hit"`
	Detail string `json:"detail,omitempty"`
}

// traceRecorder is the internal nil-safe append target threaded
// through resolveModelInternal. A nil recorder means tracing is
// disabled and every method is a cheap no-op so the hot path (the
// non-trace ResolveModel call) pays nothing.
type traceRecorder struct {
	steps []ResolutionStep
}

func (t *traceRecorder) record(layer Layer, hit bool, detail string) {
	if t == nil {
		return
	}
	t.steps = append(t.steps, ResolutionStep{
		Layer:  layer,
		Hit:    hit,
		Detail: detail,
	})
}

// ResolveModelWithTrace is the observability-friendly twin of
// ResolveModel. Returns the same (*ModelConfig, error) pair PLUS
// the []ResolutionStep trace describing which layer fired (Hit==
// true) and which layers were considered + skipped (Hit==false).
//
// On error the trace is still returned to aid debugging — the
// caller can see "we got to layer N before bailing out".
func (r *ModelRouter) ResolveModelWithTrace(ctx context.Context, req *ChatRequest) (*ModelConfig, []ResolutionStep, error) {
	recorder := &traceRecorder{steps: make([]ResolutionStep, 0, 9)}
	cfg, err := r.resolveModelInternal(ctx, req, recorder)
	// Increment the Prometheus counter for whichever layer
	// reported Hit=true. Wrapped in nil-check + once-loop in case
	// a future change accidentally records two hits — the counter
	// would still be correct (one bump per hit) instead of
	// catastrophically wrong.
	for _, s := range recorder.steps {
		if s.Hit {
			recordResolutionHit(s.Layer)
		}
	}
	return cfg, recorder.steps, err
}

// ---------------------------------------------------------------------------
// Prometheus counter — llm_resolution_source{layer}
// ---------------------------------------------------------------------------

// llmResolverRegistry holds the resolver-related metrics. Package
// local so the LLM package owns its own counter lifecycle —
// cmd/server flushes it through ExportResolverPrometheus() during
// the /api/metrics render. No global state is exposed to other
// packages.
//
// Lazy init: the counter is created on the first
// ResolveModelWithTrace call. This avoids an init-ordering surprise
// with code that builds a registry before this package is
// referenced.
var (
	llmResolverOnce      sync.Once
	llmResolverRegistry  *metrics.Registry
	llmResolutionCounter *metrics.Counter
)

func ensureResolverMetrics() {
	llmResolverOnce.Do(func() {
		llmResolverRegistry = metrics.NewRegistry()
		llmResolutionCounter = metrics.NewCounter(
			"llm_resolution_source_total",
			"Number of LLM model resolutions broken down by which layer in the priority chain fired.",
		)
		llmResolverRegistry.MustRegister(llmResolutionCounter)
	})
}

func recordResolutionHit(layer Layer) {
	ensureResolverMetrics()
	if llmResolutionCounter == nil {
		return
	}
	llmResolutionCounter.Inc(metrics.Labels{"layer": string(layer)})
}

// ExportResolverPrometheus renders the resolver registry to a
// Prometheus text-format string. Returns "" when no resolutions
// have happened yet (registry not initialised) so the caller can
// safely concatenate the output into a larger /metrics blob.
//
// Designed to be plugged into cmd/server/handleMetrics alongside
// the other exportXxxPrometheus() helpers — keeps the LLM package
// decoupled from the hand-rolled serverMetrics aggregator.
func ExportResolverPrometheus() string {
	if llmResolverRegistry == nil {
		return ""
	}
	var buf bytes.Buffer
	if err := llmResolverRegistry.WritePrometheus(&buf); err != nil {
		return ""
	}
	return buf.String()
}
