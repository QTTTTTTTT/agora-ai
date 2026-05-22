package marketdata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNewTranslatorReturnsNoopByDefault(t *testing.T) {
	tr := NewTranslator(TranslatorConfig{})
	if _, ok := tr.(noopTranslator); !ok {
		t.Fatalf("expected noopTranslator, got %T", tr)
	}
}

func TestNewTranslatorFallsBackToNoopOnMisconfiguration(t *testing.T) {
	// libretranslate without BaseURL → noop
	tr := NewTranslator(TranslatorConfig{Provider: "libretranslate"})
	if _, ok := tr.(noopTranslator); !ok {
		t.Fatalf("expected noopTranslator for misconfigured libretranslate, got %T", tr)
	}
	// openai-compat without API key → noop
	tr = NewTranslator(TranslatorConfig{Provider: "openai-compat", BaseURL: "http://example"})
	if _, ok := tr.(noopTranslator); !ok {
		t.Fatalf("expected noopTranslator for misconfigured openai-compat, got %T", tr)
	}
}

func TestLibreTranslateFillsMissingVariantAndCaches(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/translate" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		var payload struct {
			Q      string `json:"q"`
			Source string `json:"source"`
			Target string `json:"target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Echo the source text wrapped in [target] so we can assert.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"translatedText": "[" + payload.Target + "]" + payload.Q,
		})
	}))
	defer srv.Close()

	tr := NewTranslator(TranslatorConfig{Provider: "libretranslate", BaseURL: srv.URL}).(*libreTranslateTranslator)
	items := []NewsItem{
		{Title: "贵州茅台一季度净利增长", Summary: "公司公告称…", Language: NewsLanguageZH},
	}
	translated, err := tr.Translate(context.Background(), items)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if translated[0].TitleEn == "" {
		t.Fatalf("expected titleEn to be filled, got empty")
	}
	if !strings.HasPrefix(translated[0].TitleEn, "[en]") {
		t.Fatalf("expected titleEn prefixed with [en], got %q", translated[0].TitleEn)
	}
	if translated[0].TitleZh != "贵州茅台一季度净利增长" {
		t.Fatalf("expected titleZh preserved as source, got %q", translated[0].TitleZh)
	}

	before := calls.Load()
	// Second call should hit cache, not the server.
	_, _ = tr.Translate(context.Background(), translated)
	if calls.Load() != before {
		t.Fatalf("expected cache hit, but server was called again (%d -> %d)", before, calls.Load())
	}
}

func TestLibreTranslateSkipsWhenSourceEqualsTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("translator should not have been called for same-language items")
	}))
	defer srv.Close()

	tr := NewTranslator(TranslatorConfig{Provider: "libretranslate", BaseURL: srv.URL, Targets: []string{NewsLanguageZH}}).(*libreTranslateTranslator)
	items := []NewsItem{{Title: "贵州茅台", Language: NewsLanguageZH, TitleZh: "贵州茅台"}}
	_, _ = tr.Translate(context.Background(), items)
}

func TestOpenAICompatTranslatorFillsBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		respBody := map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content": `[{"index":0,"title":"Kweichow Moutai Q1 net profit grows","summary":"The company announced…"}]`,
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(respBody)
	}))
	defer srv.Close()

	tr := NewTranslator(TranslatorConfig{
		Provider: "openai-compat",
		BaseURL:  srv.URL,
		APIKey:   "test-key",
		Model:    "test-model",
	}).(*openAICompatTranslator)

	items := []NewsItem{
		{Title: "贵州茅台一季度净利增长", Summary: "公司公告称…", Language: NewsLanguageZH},
	}
	translated, err := tr.Translate(context.Background(), items)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if translated[0].TitleEn != "Kweichow Moutai Q1 net profit grows" {
		t.Fatalf("unexpected titleEn: %q", translated[0].TitleEn)
	}
	if translated[0].SummaryEn != "The company announced…" {
		t.Fatalf("unexpected summaryEn: %q", translated[0].SummaryEn)
	}
}

func TestOpenAICompatTranslatorReturnsErrorWhenModelMissing(t *testing.T) {
	tr := &openAICompatTranslator{cfg: TranslatorConfig{Provider: "openai-compat", BaseURL: "http://example", APIKey: "k"}.normalize()}
	_, err := tr.Translate(context.Background(), []NewsItem{{Title: "x"}})
	if err == nil {
		t.Fatalf("expected error when model is empty")
	}
}

