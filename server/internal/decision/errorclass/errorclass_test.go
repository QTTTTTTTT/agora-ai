package errorclass

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/fundai/server/internal/llm"
)

func TestClassify_NilError(t *testing.T) {
	d := Classify(nil)
	if d.Category != CategoryUnknown {
		t.Fatalf("nil → expected unknown, got %s", d.Category)
	}
	if d.Summary != "" {
		t.Fatalf("expected empty summary for nil error, got %q", d.Summary)
	}
}

func TestClassify_Sentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Category
	}{
		{"context_canceled", context.Canceled, CategoryCancelled},
		{"deadline_exceeded", context.DeadlineExceeded, CategoryNetworkTimeout},
		{"missing_creds", llm.ErrMissingCredentials, CategoryAuthFailed},
		{"budget_exceeded", llm.ErrCallBudgetExceeded, CategoryBudgetExceeded},
		{"circuit_open", llm.ErrCircuitOpen, CategoryServiceUnavailable},
		{"rate_limited", llm.ErrRateLimited, CategoryRateLimited},
		// Wrapped sentinels — errors.Is must still recognise.
		{"wrapped_missing_creds", fmt.Errorf("llm: %w", llm.ErrMissingCredentials), CategoryAuthFailed},
		{"wrapped_circuit_open", fmt.Errorf("provider X: %w", llm.ErrCircuitOpen), CategoryServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(c.err).Category
			if got != c.want {
				t.Fatalf("expected %s, got %s", c.want, got)
			}
		})
	}
}

func TestClassify_RequestError_ReasonField(t *testing.T) {
	cases := []struct {
		reason string
		want   Category
	}{
		{"rate_limited", CategoryRateLimited},
		{"server_error", CategoryServiceUnavailable},
		{"timeout", CategoryNetworkTimeout},
		{"cancelled", CategoryCancelled},
		{"empty_choices", CategoryEmptyResponse},
		{"empty_content", CategoryEmptyResponse},
		{"empty_candidates", CategoryEmptyResponse},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			rerr := &llm.RequestError{
				Provider: llm.ProviderOpenAI,
				Model:    "gpt-4o",
				Reason:   c.reason,
				Message:  "stub message",
			}
			d := Classify(rerr)
			if d.Category != c.want {
				t.Fatalf("reason=%s → expected %s, got %s", c.reason, c.want, d.Category)
			}
			if d.Provider != "openai" || d.Model != "gpt-4o" {
				t.Fatalf("expected provider/model populated, got provider=%q model=%q", d.Provider, d.Model)
			}
		})
	}
}

func TestClassify_RequestError_StatusFallback(t *testing.T) {
	cases := []struct {
		status int
		want   Category
	}{
		{http.StatusTooManyRequests, CategoryRateLimited},
		{http.StatusUnauthorized, CategoryAuthFailed},
		{http.StatusForbidden, CategoryAuthFailed},
		{http.StatusBadGateway, CategoryServiceUnavailable},
		{http.StatusServiceUnavailable, CategoryServiceUnavailable},
		{http.StatusBadRequest, CategoryInvalidRequest},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("status_%d", c.status), func(t *testing.T) {
			rerr := &llm.RequestError{
				Provider:   llm.ProviderClaude,
				Model:      "claude-opus-4",
				StatusCode: c.status,
				Reason:     "provider_error",
				Message:    "stub",
			}
			if got := Classify(rerr).Category; got != c.want {
				t.Fatalf("status=%d → expected %s, got %s", c.status, c.want, got)
			}
		})
	}
}

func TestClassify_RequestError_ContextLengthInBody(t *testing.T) {
	// The provider message wins over the status-code fallback when
	// it contains a recognised keyword.
	rerr := &llm.RequestError{
		Provider:   llm.ProviderOpenAI,
		Model:      "gpt-4o",
		StatusCode: http.StatusBadRequest,
		Reason:     "provider_error",
		Message:    "This model's maximum context length is 8192 tokens",
	}
	d := Classify(rerr)
	if d.Category != CategoryContextLengthExceeded {
		t.Fatalf("expected context_length_exceeded, got %s", d.Category)
	}
}

func TestClassify_SubstringFallthroughs(t *testing.T) {
	cases := []struct {
		msg  string
		want Category
	}{
		{"connection unauthorized", CategoryAuthFailed},
		{"context length exceeded for this model", CategoryContextLengthExceeded},
		{"request timed out", CategoryNetworkTimeout},
		{"503 service unavailable", CategoryServiceUnavailable},
		{"too many requests on provider", CategoryRateLimited},
	}
	for _, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			got := Classify(errors.New(c.msg)).Category
			if got != c.want {
				t.Fatalf("msg=%q → expected %s, got %s", c.msg, c.want, got)
			}
		})
	}
}

func TestClassify_SchemaValidation(t *testing.T) {
	cases := []string{
		"response schema validation failed: missing required field",
		"failed to unmarshal schema response",
		"invalid json against schema",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			got := Classify(errors.New(msg)).Category
			if got != CategorySchemaValidationFailed {
				t.Fatalf("msg=%q → expected schema_validation_failed, got %s", msg, got)
			}
		})
	}
}

func TestClassify_FallsThroughToUnknown(t *testing.T) {
	d := Classify(errors.New("totally novel transport hiccup the classifier can't recognise"))
	if d.Category != CategoryUnknown {
		t.Fatalf("expected unknown, got %s", d.Category)
	}
	if d.Summary == "" {
		t.Fatalf("expected summary for unknown error, got empty")
	}
}

func TestDetail_ErrorString(t *testing.T) {
	d := Detail{
		Category: CategoryRateLimited,
		Provider: "openai",
		Model:    "gpt-4o",
		Summary:  "boom",
	}
	got := d.ErrorString()
	want := "openai/gpt-4o (rate_limited): boom"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
	d2 := Detail{Category: CategoryUnknown, Summary: "x"}
	if got := d2.ErrorString(); got != "unknown: x" {
		t.Fatalf("expected 'unknown: x', got %q", got)
	}
}

func TestTruncateSummary(t *testing.T) {
	cases := []struct {
		in    string
		limit int
		want  string
	}{
		{"abcde", 10, "abcde"},
		{"abcdefghij", 5, "ab..."},
		{"", 5, ""},
		{"abcde", 3, "abc"},
		{"  abc  ", 10, "abc"},
	}
	for _, c := range cases {
		if got := truncateSummary(c.in, c.limit); got != c.want {
			t.Fatalf("truncateSummary(%q, %d) = %q, want %q", c.in, c.limit, got, c.want)
		}
	}
}

func TestCategory_IsKnown(t *testing.T) {
	if !CategoryRateLimited.IsKnown() {
		t.Fatalf("rate_limited should be known")
	}
	if Category("brand_new").IsKnown() {
		t.Fatalf("unknown category string should not be IsKnown")
	}
}
