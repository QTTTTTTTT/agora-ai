package metrics

// Standard metric definitions for AI-fund platform.
//
// This file curates the *official* set of metrics that business code
// should call into. Wiring layer (cmd/server) is expected to construct
// one Stdlib via NewStdlib and pass it down to subsystems via DI; tests
// can pass a freshly-built Stdlib without registering it globally so
// they don't pollute observed counts.
//
// All names follow Prometheus convention: snake_case, _total suffix on
// counters, _seconds for latency histograms.

// Stdlib is the bundle of canonical metrics for the platform. Each
// field's `Help` describes the semantic and label expectations.
type Stdlib struct {
	// HTTP
	HTTPRequestsTotal *Counter
	HTTPLatency       *Histogram

	// Workflow (daily orchestration)
	WorkflowStepsTotal *Counter
	WorkflowStepLatency *Histogram

	// Roundtable
	RoundtablesTotal     *Counter // labels: status (completed|timeout|aborted)
	RoundtableRoundsHist *Histogram

	// Plan
	PlansGeneratedTotal *Counter
	PlanActionsTotal    *Counter // labels: action (buy|sell|hold|watch), executed (true|false)

	// Orders / matching
	OrdersTotal     *Counter // labels: status (filled|partial|rejected)
	MatchingLatency *Histogram

	// Marketplace
	MarketplaceListingsTotal *Counter // labels: mode (buyout|subscribe), event (created|cancelled|sold)
	MarketplacePurchasesTotal *Counter // labels: mode, status (succeeded|failed)
	MarketplaceInferenceLatency *Histogram

	// LLM
	LLMRequestsTotal *Counter // labels: provider, model, step, status
	LLMLatency       *Histogram

	// Scheduler
	SchedulerLeaderState *Gauge // labels: lease (1=leader, 0=follower)
	SchedulerLeaseTransitionsTotal *Counter // labels: lease, kind (acquired|lost|released)

	// Panics
	PanicsTotal *Counter // labels: subsystem
}

// NewStdlib builds the canonical metric set and registers everything on
// the supplied registry. Pass `nil` to skip registration (useful in
// tests that want isolated metrics).
func NewStdlib(reg *Registry) *Stdlib {
	s := &Stdlib{
		HTTPRequestsTotal: NewCounter(
			"http_requests_total",
			"Total HTTP requests handled, labelled by method/route/status_class.",
		),
		HTTPLatency: NewHistogram(
			"http_request_duration_seconds",
			"HTTP request latency in seconds.",
			DefaultLatencyBuckets,
		),

		WorkflowStepsTotal: NewCounter(
			"workflow_steps_total",
			"Workflow step transitions, labelled by step and outcome.",
		),
		WorkflowStepLatency: NewHistogram(
			"workflow_step_duration_seconds",
			"Workflow step latency in seconds.",
			DefaultLatencyBuckets,
		),

		RoundtablesTotal: NewCounter(
			"roundtables_total",
			"Roundtable sessions completed, labelled by terminal status.",
		),
		RoundtableRoundsHist: NewHistogram(
			"roundtable_rounds",
			"Number of rounds in completed roundtable sessions.",
			[]float64{1, 2, 3, 4, 5, 6, 7, 8},
		),

		PlansGeneratedTotal: NewCounter(
			"plans_generated_total",
			"Investment plans generated, labelled by fund.",
		),
		PlanActionsTotal: NewCounter(
			"plan_actions_total",
			"Plan actions emitted, labelled by action kind and execution outcome.",
		),

		OrdersTotal: NewCounter(
			"orders_total",
			"Orders submitted, labelled by terminal status.",
		),
		MatchingLatency: NewHistogram(
			"matching_engine_duration_seconds",
			"Matching engine processing latency in seconds.",
			DefaultLatencyBuckets,
		),

		MarketplaceListingsTotal: NewCounter(
			"marketplace_listings_total",
			"Marketplace listing events, labelled by mode and event.",
		),
		MarketplacePurchasesTotal: NewCounter(
			"marketplace_purchases_total",
			"Marketplace purchase attempts, labelled by mode and final status.",
		),
		MarketplaceInferenceLatency: NewHistogram(
			"marketplace_inference_duration_seconds",
			"Black-box inference gateway latency in seconds.",
			DefaultLatencyBuckets,
		),

		LLMRequestsTotal: NewCounter(
			"llm_requests_total",
			"LLM completion calls, labelled by provider/model/step/status.",
		),
		LLMLatency: NewHistogram(
			"llm_request_duration_seconds",
			"LLM completion latency in seconds.",
			DefaultLatencyBuckets,
		),

		SchedulerLeaderState: NewGauge(
			"scheduler_leader_state",
			"1 if this process holds the named lease, 0 otherwise.",
		),
		SchedulerLeaseTransitionsTotal: NewCounter(
			"scheduler_lease_transitions_total",
			"Lease state transitions, labelled by lease and kind.",
		),

		PanicsTotal: NewCounter(
			"panics_total",
			"Recovered panics, labelled by subsystem.",
		),
	}

	if reg != nil {
		reg.MustRegister(
			s.HTTPRequestsTotal, s.HTTPLatency,
			s.WorkflowStepsTotal, s.WorkflowStepLatency,
			s.RoundtablesTotal, s.RoundtableRoundsHist,
			s.PlansGeneratedTotal, s.PlanActionsTotal,
			s.OrdersTotal, s.MatchingLatency,
			s.MarketplaceListingsTotal, s.MarketplacePurchasesTotal, s.MarketplaceInferenceLatency,
			s.LLMRequestsTotal, s.LLMLatency,
			s.SchedulerLeaderState, s.SchedulerLeaseTransitionsTotal,
			s.PanicsTotal,
		)
	}

	return s
}
