// Package errorclass maps the heterogeneous error surface produced
// by the LLM stack — provider HTTP errors, retry-policy circuit
// breakers, schema-validation failures, missing-credentials — into
// a bounded set of user-facing categories.
//
// The categorisation MUST be deterministic and exhaustive: every
// non-nil error returned by decision.DecisionEngine.Decide() resolves
// to exactly one Category. Unknown shapes fall into CategoryUnknown
// AND are simultaneously surfaced via Prometheus (S11.4) so the
// operator team can extend the enum instead of letting category
// drift go silent.
//
// Two important invariants:
//
//  1. The Category values are persisted in `investment_plans.fallback_reason`
//     and consumed by the React layer to look up i18n strings. Renaming
//     a constant therefore requires both a migration AND a frontend
//     dictionary update — keep the set small and stable.
//
//  2. The Detail.Summary field carries the raw provider message and
//     MUST NOT be exposed to non-admin users verbatim. The Detail
//     struct is what gets stored; the API layer is responsible for
//     stripping Summary when serving end-user (non-admin) requests.
package errorclass

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/llm"
)

// Category is the bounded set of user-facing reasons we surface when
// the LLM path fails and the fallback heuristic fires. Each value maps
// 1:1 to an i18n string the frontend renders.
type Category string

const (
	// CategoryRateLimited — provider returned HTTP 429 or our own
	// owner-level rate limiter blocked the call. User action: wait
	// a few minutes and retry.
	CategoryRateLimited Category = "rate_limited"

	// CategoryServiceUnavailable — provider returned HTTP 5xx or
	// the circuit breaker is open. User action: retry later /
	// contact support if persistent.
	CategoryServiceUnavailable Category = "service_unavailable"

	// CategoryAuthFailed — missing API key, expired key, or
	// HTTP 401/403 from the provider. User action: re-link the
	// provider in fund settings.
	CategoryAuthFailed Category = "auth_failed"

	// CategoryContextLengthExceeded — provider rejected the prompt
	// for exceeding its max context. User action: shrink the
	// universe / prompt, or switch to a longer-context tier.
	CategoryContextLengthExceeded Category = "context_length_exceeded"

	// CategoryInvalidRequest — generic HTTP 4xx that isn't a
	// rate-limit or auth issue. User action: review fund
	// configuration; admins see the raw body.
	CategoryInvalidRequest Category = "invalid_request"

	// CategorySchemaValidationFailed — LLM returned a syntactically
	// invalid JSON output or the output failed our JSON-schema
	// validator. User action: usually transient, retry.
	CategorySchemaValidationFailed Category = "schema_validation_failed"

	// CategoryNetworkTimeout — request timed out or upstream
	// connection error. User action: retry; if persistent the
	// platform may be misconfigured.
	CategoryNetworkTimeout Category = "network_timeout"

	// CategoryBudgetExceeded — call budget guard tripped before
	// the request even left the platform. User action: increase
	// budget or wait for the window to reset.
	CategoryBudgetExceeded Category = "budget_exceeded"

	// CategoryEmptyResponse — provider returned 200 but the
	// content was empty / blank. User action: retry; persistent
	// blanks usually mean a prompt incompatibility.
	CategoryEmptyResponse Category = "empty_response"

	// CategoryCancelled — call cancelled by the caller (usually a
	// workflow timeout / shutdown). Not a real error in the user
	// sense; we still record it so the admin board sees the
	// cancellation rate.
	CategoryCancelled Category = "cancelled"

	// CategoryUnknown — error didn't match any known shape. Triggers
	// a warning log + Prometheus counter so the operator team can
	// extend the classifier. The user-facing string for this
	// category is intentionally generic ("AI service error").
	CategoryUnknown Category = "unknown"
)

// IsKnown returns true when c is one of the explicit categories the
// classifier ships with. Used by callers (S11.4) that want to alert
// on category drift.
func (c Category) IsKnown() bool {
	switch c {
	case CategoryRateLimited, CategoryServiceUnavailable, CategoryAuthFailed,
		CategoryContextLengthExceeded, CategoryInvalidRequest,
		CategorySchemaValidationFailed, CategoryNetworkTimeout,
		CategoryBudgetExceeded, CategoryEmptyResponse, CategoryCancelled,
		CategoryUnknown:
		return true
	default:
		return false
	}
}

// Detail is the structured shape we persist into
// investment_plans.fallback_reason (JSONB). The frontend renders
// Category + Provider + Model directly; Summary is the raw provider
// message and MUST be stripped by the API layer before being served
// to non-admin users.
type Detail struct {
	Category Category  `json:"category"`
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`
	Attempt  int       `json:"attempt,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	At       time.Time `json:"at"`
}

// Classify maps a raw error from the LLM call chain to a Detail. The
// returned Detail is always non-nil; nil errors map to
// CategoryUnknown with an empty summary (callers should guard against
// this and not call Classify with a nil error in normal flow).
func Classify(err error) Detail {
	d := Detail{
		Category: CategoryUnknown,
		At:       time.Now().UTC(),
	}
	if err == nil {
		return d
	}
	d.Summary = truncateSummary(err.Error(), 240)

	// 1. Sentinel errors win — these are unambiguous and don't
	//    depend on the underlying transport detail.
	switch {
	case errors.Is(err, context.Canceled):
		d.Category = CategoryCancelled
		return d
	case errors.Is(err, context.DeadlineExceeded):
		d.Category = CategoryNetworkTimeout
		return d
	case errors.Is(err, llm.ErrMissingCredentials):
		d.Category = CategoryAuthFailed
		return d
	case errors.Is(err, llm.ErrCallBudgetExceeded):
		d.Category = CategoryBudgetExceeded
		return d
	case errors.Is(err, llm.ErrCircuitOpen):
		d.Category = CategoryServiceUnavailable
		return d
	case errors.Is(err, llm.ErrRateLimited):
		d.Category = CategoryRateLimited
		return d
	}

	// 2. Provider-typed error — extract Provider / Model and
	//    classify by Reason + StatusCode.
	var rerr *llm.RequestError
	if errors.As(err, &rerr) {
		d.Provider = string(rerr.Provider)
		d.Model = rerr.Model
		d.Category = classifyRequestError(rerr)
		return d
	}

	// 3. Schema-validation messages from llm/structured_output.go
	//    have a stable substring fingerprint we can recognise.
	if isSchemaValidationError(err) {
		d.Category = CategorySchemaValidationFailed
		return d
	}

	// 4. Final substring sweep — catches a handful of common
	//    transport-layer phrases we can confidently bucket.
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "no api key"), strings.Contains(lower, "missing credential"),
		strings.Contains(lower, "unauthorized"):
		d.Category = CategoryAuthFailed
	case strings.Contains(lower, "context length"), strings.Contains(lower, "maximum context length"),
		strings.Contains(lower, "context_length_exceeded"):
		d.Category = CategoryContextLengthExceeded
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"):
		d.Category = CategoryNetworkTimeout
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "too many requests"):
		d.Category = CategoryRateLimited
	case strings.Contains(lower, "service unavailable"), strings.Contains(lower, "bad gateway"),
		strings.Contains(lower, "502"), strings.Contains(lower, "503"):
		d.Category = CategoryServiceUnavailable
	}
	return d
}

func classifyRequestError(rerr *llm.RequestError) Category {
	// Reason field is set by the LLM client and is more reliable
	// than the wire-level StatusCode for soft failures (empty
	// content etc.).
	switch rerr.Reason {
	case "rate_limited":
		return CategoryRateLimited
	case "server_error":
		return CategoryServiceUnavailable
	case "timeout":
		return CategoryNetworkTimeout
	case "cancelled":
		return CategoryCancelled
	case "empty_choices", "empty_content", "empty_candidates":
		return CategoryEmptyResponse
	}

	// Provider error bodies sometimes leak a useful keyword we can
	// pull out — context_length_exceeded in particular is very
	// common and not always reflected in the HTTP status code.
	lowerMsg := strings.ToLower(rerr.Message)
	switch {
	case strings.Contains(lowerMsg, "context length"),
		strings.Contains(lowerMsg, "context_length_exceeded"),
		strings.Contains(lowerMsg, "maximum context"):
		return CategoryContextLengthExceeded
	case strings.Contains(lowerMsg, "invalid_api_key"),
		strings.Contains(lowerMsg, "authentication"),
		strings.Contains(lowerMsg, "unauthorized"):
		return CategoryAuthFailed
	}

	// Status-code fallthrough.
	switch {
	case rerr.StatusCode == http.StatusTooManyRequests:
		return CategoryRateLimited
	case rerr.StatusCode == http.StatusUnauthorized, rerr.StatusCode == http.StatusForbidden:
		return CategoryAuthFailed
	case rerr.StatusCode >= 500:
		return CategoryServiceUnavailable
	case rerr.StatusCode >= 400:
		return CategoryInvalidRequest
	}
	return CategoryUnknown
}

// isSchemaValidationError matches the error shape emitted by
// llm/structured_output.go and agent/llm_adapter.go when the
// LLM-generated JSON fails to parse or fails the response_schema
// validator. We look for a fingerprint substring rather than an
// exported sentinel because the structured-output package is small
// enough that adding a public Err* would inflate its public surface
// for one consumer.
func isSchemaValidationError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "schema") &&
		(strings.Contains(lower, "validation") ||
			strings.Contains(lower, "unmarshal") ||
			strings.Contains(lower, "invalid json"))
}

func truncateSummary(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	if limit <= 3 {
		return s[:limit]
	}
	return s[:limit-3] + "..."
}

// String makes Category satisfy fmt.Stringer so it shows up in slog
// records with no extra coaxing.
func (c Category) String() string { return string(c) }

// ErrorString is the technical, admin-facing summary of one Detail.
// User-facing rendering happens in the frontend via i18n; this helper
// exists for slog / audit log lines where a single string is more
// convenient than a JSON blob.
func (d Detail) ErrorString() string {
	if d.Provider != "" && d.Model != "" {
		return fmt.Sprintf("%s/%s (%s): %s", d.Provider, d.Model, d.Category, d.Summary)
	}
	return fmt.Sprintf("%s: %s", d.Category, d.Summary)
}
