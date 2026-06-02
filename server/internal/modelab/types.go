// Package modelab is the Sprint 10 model-level A/B experiment
// engine. It sits BETWEEN the workflow / business code and the
// llm.MultiProviderClient so that "use a different model for
// this fund/agent/step" becomes a configuration change instead
// of a code change.
//
// Why a separate package: the existing internal/abtest engine
// runs by cloning the entire fund and letting both copies trade
// independently. That works for strategy changes ("swap PM
// persona", "raise the risk cap") but is too heavy for model
// changes — the answer to "is Claude better than GPT here?" can
// be obtained without forking the portfolio if we just run BOTH
// models on the SAME prompts and compare outputs side by side.
//
// Architecture (high level):
//
//	business code  →  llm.LLMClient (interface)
//	                    ▲
//	                    │   (in production, wraps as below)
//	                    │
//	                modelab.ShadowDispatcher  ──┐
//	                    │                       │ parallel
//	                    ├── primary  ─→ real LLM (steers execution)
//	                    └── shadow_n ─→ real LLM (observed only)
//
// Each call's "which model gets to be primary" decision lives in
// the ModelRouter (which arm wins the consistent hash). The
// dispatcher just executes all arms in parallel, returns the
// primary's response, and records the rest.
//
// Sticky-arm guarantee: a single workflow_run × step × agent
// tuple is always assigned to the SAME arm across multiple LLM
// calls inside that step. The whole point of the experiment is
// to make A and B comparable, and we'd contaminate the
// comparison if a 3-call debate were split across two models.
package modelab

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/fundai/server/internal/llm"
)

// ExperimentStatus is the lifecycle state of a model A/B
// experiment. Only "running" experiments are looked up by the
// router on the hot path; the other states are persisted for
// audit and report generation.
type ExperimentStatus string

const (
	StatusDraft     ExperimentStatus = "draft"
	StatusRunning   ExperimentStatus = "running"
	StatusPaused    ExperimentStatus = "paused"
	StatusCompleted ExperimentStatus = "completed"
	StatusArchived  ExperimentStatus = "archived"
)

// Scope is the dimension on which an experiment matches incoming
// LLM calls. Picked from the request's (fund_id, agent_id, step)
// — see Match below.
type Scope string

const (
	// ScopeGlobal: apply to every LLM call the platform makes.
	// scope_target is empty.
	ScopeGlobal Scope = "global"
	// ScopeFund: apply only to the LLM calls of one fund.
	// scope_target = fund_id.
	ScopeFund Scope = "fund"
	// ScopeAgentRole: apply to every agent of a given role
	// (e.g. "pm", "risk"). scope_target = role string.
	ScopeAgentRole Scope = "agent_role"
	// ScopeAgentID: apply to one specific agent row.
	// scope_target = agent_id.
	ScopeAgentID Scope = "agent_id"
)

// ArmConfig is one row in the experiment's arms[] JSONB array.
// It mirrors a subset of llm.ModelConfig so the router can apply
// it without re-importing the full config schema (API keys are
// resolved separately, at hot-path time, against the system or
// user-provider key stores — they are never persisted on the
// experiment row).
type ArmConfig struct {
	Name        string        `json:"name"`
	Provider    llm.Provider  `json:"provider"`
	ModelName   string        `json:"model_name"`
	BaseURL     string        `json:"base_url"`
	ModelTier   llm.ModelTier `json:"model_tier"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

// Validate enforces the basic well-formedness of a single arm.
// Returns an error a CRUD handler can surface verbatim to the
// operator. Empty Name is allowed only when the arm carries a
// non-empty ModelName (the report will fall back to the model
// name as the column header).
func (a ArmConfig) Validate() error {
	if strings.TrimSpace(string(a.Provider)) == "" {
		return errors.New("modelab: ArmConfig.Provider is required")
	}
	if strings.TrimSpace(a.ModelName) == "" {
		return errors.New("modelab: ArmConfig.ModelName is required")
	}
	return nil
}

// Label returns a stable single-string identifier for the arm,
// used as the column header on the report and as the "arm_model"
// value in shadow_responses. Format: "<provider>/<model>".
func (a ArmConfig) Label() string {
	return string(a.Provider) + "/" + a.ModelName
}

// Experiment is the in-memory mirror of one model_ab_experiments
// row. Field names follow the SQL column names — no abbreviation
// — so a printf of the struct reads identically to the DB row.
type Experiment struct {
	ID             string
	Name           string
	Description    string
	Scope          Scope
	ScopeTarget    string
	StepFilter     []string
	Arms           []ArmConfig
	TrafficSplit   []float64
	Status         ExperimentStatus
	StartAt        time.Time
	EndAt          time.Time
	MaxTotalTokens int64 // 0 = no cap
	TokensUsed     int64
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Validate enforces shape invariants the rest of the package
// assumes. Called by the CRUD handler before persisting, and by
// the loader after deserialising the JSONB columns.
func (e *Experiment) Validate() error {
	if e == nil {
		return errors.New("modelab: nil experiment")
	}
	if strings.TrimSpace(e.Name) == "" {
		return errors.New("modelab: experiment name required")
	}
	switch e.Scope {
	case ScopeGlobal, ScopeFund, ScopeAgentRole, ScopeAgentID:
	default:
		return errors.New("modelab: invalid scope " + string(e.Scope))
	}
	if e.Scope != ScopeGlobal && strings.TrimSpace(e.ScopeTarget) == "" {
		return errors.New("modelab: scope_target required for scope " + string(e.Scope))
	}
	if len(e.Arms) < 2 {
		return errors.New("modelab: experiment needs at least 2 arms (control + 1 treatment)")
	}
	if len(e.Arms) > 8 {
		return errors.New("modelab: experiment limited to 8 arms")
	}
	if len(e.TrafficSplit) != len(e.Arms) {
		return errors.New("modelab: traffic_split length must match arms length")
	}
	var sum float64
	for i, p := range e.TrafficSplit {
		if p < 0 {
			return errors.New("modelab: traffic_split must be non-negative")
		}
		sum += p
		if err := e.Arms[i].Validate(); err != nil {
			return err
		}
	}
	// Allow a small float tolerance — operators frequently enter
	// 0.33/0.33/0.33 by hand.
	if sum < 0.99 || sum > 1.01 {
		return errors.New("modelab: traffic_split must sum to ~1.0")
	}
	return nil
}

// Match reports whether this experiment applies to a request
// whose (fund, agent, role, step) tuple is supplied. The router
// calls this on the hot path so it stays branch-light: nothing
// inside Match touches the DB.
//
// A nil receiver or non-running status returns false — callers
// can therefore loop over `[]*Experiment` and rely on this
// method to filter.
func (e *Experiment) Match(fundID, agentID, agentRole, step string) bool {
	if e == nil || e.Status != StatusRunning {
		return false
	}
	if len(e.StepFilter) > 0 {
		hit := false
		for _, s := range e.StepFilter {
			if strings.EqualFold(s, step) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	switch e.Scope {
	case ScopeGlobal:
		return true
	case ScopeFund:
		return strings.EqualFold(strings.TrimSpace(e.ScopeTarget), strings.TrimSpace(fundID))
	case ScopeAgentRole:
		return strings.EqualFold(strings.TrimSpace(e.ScopeTarget), strings.TrimSpace(agentRole))
	case ScopeAgentID:
		return strings.EqualFold(strings.TrimSpace(e.ScopeTarget), strings.TrimSpace(agentID))
	default:
		return false
	}
}

// BudgetExhausted reports whether the cumulative tokens spent on
// shadow arms have crossed MaxTotalTokens. Zero cap means "no
// cap" → never exhausted.
func (e *Experiment) BudgetExhausted() bool {
	if e == nil || e.MaxTotalTokens <= 0 {
		return false
	}
	return e.TokensUsed >= e.MaxTotalTokens
}

// MarshalArms / UnmarshalArms are tiny helpers the repo uses to
// move the arms array in/out of the JSONB column without leaking
// json.Marshal into the call sites.
func MarshalArms(arms []ArmConfig) ([]byte, error) { return json.Marshal(arms) }
func UnmarshalArms(b []byte) ([]ArmConfig, error) {
	var out []ArmConfig
	if len(b) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Assignment is the sticky-arm row written on the first call of
// (experiment, run, step, agent). Subsequent calls inside the
// same tuple read it back instead of re-hashing, so the router
// is deterministic in face of weight changes mid-day.
type Assignment struct {
	ID           string
	ExperimentID string
	RunID        string
	Step         string
	AgentID      string
	FundID       string
	ArmIndex     int
	ArmName      string
	AssignedAt   time.Time
}

// ShadowResponse is one row of model_ab_shadow_responses.
type ShadowResponse struct {
	ID            string
	ExperimentID  string
	AssignmentID  string
	RunID         string
	Step          string
	AgentID       string
	FundID        string
	ArmIndex      int
	ArmName       string
	ArmModel      string
	RawOutput     string
	ParsedOutput  json.RawMessage
	ParseError    string
	InputTokens   int
	OutputTokens  int
	LatencyMs     int
	CostMicro     int64
	ErrorText     string
	FinishedAt    time.Time
}
