package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// NewsTranslator translates a batch of NewsItem headlines/summaries into the
// requested target language. Implementations should be:
//
//   - Idempotent: re-translating an already-translated item should be a no-op
//     so the service can call them on cached results without burning quota.
//   - Resilient: errors on individual items must not fail the whole batch;
//     return the items that were successfully translated.
//   - Cancel-aware: honour ctx cancellation so the caller can bound latency.
//
// The translator is invoked once per news fetch (after hybrid merge) and is
// expected to populate the *Zh / *En fields on items whose Language differs
// from the platform's two supported languages.
type NewsTranslator interface {
	Translate(ctx context.Context, items []NewsItem) ([]NewsItem, error)
}

// TranslatorConfig is parsed from environment variables at boot time.
type TranslatorConfig struct {
	Provider string        // "none" | "libretranslate" | "openai-compat"
	BaseURL  string        // e.g. https://libretranslate.com or https://api.deepseek.com/v1
	APIKey   string        // optional; required for openai-compat and recommended for libretranslate
	Model    string        // openai-compat only, e.g. "deepseek-chat"
	Timeout  time.Duration // per-batch HTTP timeout; defaults to 8s
	Targets  []string      // which languages to ensure exist; defaults to ["zh", "en"]
}

func (c TranslatorConfig) normalize() TranslatorConfig {
	c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.APIKey = strings.TrimSpace(c.APIKey)
	c.Model = strings.TrimSpace(c.Model)
	if c.Timeout <= 0 {
		c.Timeout = 8 * time.Second
	}
	if len(c.Targets) == 0 {
		c.Targets = []string{NewsLanguageZH, NewsLanguageEN}
	} else {
		normalized := make([]string, 0, len(c.Targets))
		seen := make(map[string]struct{}, len(c.Targets))
		for _, target := range c.Targets {
			lower := strings.ToLower(strings.TrimSpace(target))
			if lower == "" {
				continue
			}
			if _, ok := seen[lower]; ok {
				continue
			}
			seen[lower] = struct{}{}
			normalized = append(normalized, lower)
		}
		c.Targets = normalized
	}
	return c
}

// NewTranslator picks the implementation based on Provider. Returns a
// noopTranslator when Provider is empty/"none" or required fields are
// missing (so the rest of the service can call .Translate unconditionally).
func NewTranslator(cfg TranslatorConfig) NewsTranslator {
	normalized := cfg.normalize()
	switch normalized.Provider {
	case "libretranslate":
		if normalized.BaseURL == "" {
			slog.Warn("marketdata: translator libretranslate missing base url; falling back to noop")
			return noopTranslator{}
		}
		return newLibreTranslateTranslator(normalized)
	case "openai-compat":
		if normalized.BaseURL == "" || normalized.APIKey == "" {
			slog.Warn("marketdata: translator openai-compat missing base url or api key; falling back to noop")
			return noopTranslator{}
		}
		return newOpenAICompatTranslator(normalized)
	case "", "none":
		return noopTranslator{}
	default:
		slog.Warn("marketdata: unknown translator provider; falling back to noop", "provider", normalized.Provider)
		return noopTranslator{}
	}
}

// translateNewsItems applies the configured translator to a batch of news
// items. Returns the input slice as-is when no translator is configured.
// Errors are logged but not propagated so a translation outage cannot break
// the underlying news pipeline.
func (s *Service) translateNewsItems(ctx context.Context, items []NewsItem) []NewsItem {
	if s == nil || s.translator == nil || len(items) == 0 {
		return items
	}
	translated, err := s.translator.Translate(ctx, items)
	if err != nil {
		slog.Warn("marketdata: translator failed; returning untranslated items", "error", err, "count", len(items))
		return items
	}
	return translated
}

// noopTranslator is the default implementation used when no upstream
// translation service is configured. It returns the items unchanged.
type noopTranslator struct{}

func (noopTranslator) Translate(_ context.Context, items []NewsItem) ([]NewsItem, error) {
	return items, nil
}

// ----- LibreTranslate -----

type libreTranslateTranslator struct {
	cfg        TranslatorConfig
	httpClient *http.Client
	cache      *translationCache
}

func newLibreTranslateTranslator(cfg TranslatorConfig) *libreTranslateTranslator {
	return &libreTranslateTranslator{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: cfg.Timeout},
		cache:      newTranslationCache(),
	}
}

// Translate fills missing TitleZh/TitleEn (and matching Summary fields) by
// calling LibreTranslate's POST /translate endpoint per missing variant.
// Each (text, source, target) tuple is cached in-memory to avoid burning
// quota when the same article is re-fetched within the TTL window.
//
// LibreTranslate request:
//
//	POST /translate
//	{ "q": "...", "source": "zh", "target": "en", "format": "text", "api_key": "..." }
//
// LibreTranslate response:
//
//	{ "translatedText": "..." }
func (t *libreTranslateTranslator) Translate(ctx context.Context, items []NewsItem) ([]NewsItem, error) {
	if len(items) == 0 {
		return items, nil
	}
	// Backfill the source-language variant from Title/Summary so callers can
	// rely on titleZh/titleEn being present for the source language without
	// having to manually invoke tagNewsItemsWithLanguage first.
	items = tagNewsItemsWithLanguage(items, "")
	for i := range items {
		base := items[i].Language
		for _, target := range t.cfg.Targets {
			if target == base {
				continue
			}
			if target == NewsLanguageZH && strings.TrimSpace(items[i].TitleZh) != "" && strings.TrimSpace(items[i].SummaryZh) != "" {
				continue
			}
			if target == NewsLanguageEN && strings.TrimSpace(items[i].TitleEn) != "" && strings.TrimSpace(items[i].SummaryEn) != "" {
				continue
			}
			t.fillVariant(ctx, &items[i], base, target)
		}
	}
	return items, nil
}

func (t *libreTranslateTranslator) fillVariant(ctx context.Context, item *NewsItem, source, target string) {
	if title := strings.TrimSpace(item.Title); title != "" {
		if translated := t.translateOne(ctx, title, source, target); translated != "" {
			setLocalizedTitle(item, target, translated)
		}
	}
	if summary := strings.TrimSpace(item.Summary); summary != "" {
		if translated := t.translateOne(ctx, summary, source, target); translated != "" {
			setLocalizedSummary(item, target, translated)
		}
	}
}

func (t *libreTranslateTranslator) translateOne(ctx context.Context, text, source, target string) string {
	if cached, ok := t.cache.get(text, source, target); ok {
		return cached
	}
	payload := map[string]any{
		"q":      text,
		"source": orAuto(source),
		"target": target,
		"format": "text",
	}
	if t.cfg.APIKey != "" {
		payload["api_key"] = t.cfg.APIKey
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.BaseURL+"/translate", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return ""
	}
	var parsed struct {
		TranslatedText string `json:"translatedText"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ""
	}
	result := strings.TrimSpace(parsed.TranslatedText)
	if result != "" {
		t.cache.set(text, source, target, result)
	}
	return result
}

// ----- OpenAI-compatible (DeepSeek, OpenAI, local LLM) -----

type openAICompatTranslator struct {
	cfg        TranslatorConfig
	httpClient *http.Client
	cache      *translationCache
}

func newOpenAICompatTranslator(cfg TranslatorConfig) *openAICompatTranslator {
	return &openAICompatTranslator{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: cfg.Timeout},
		cache:      newTranslationCache(),
	}
}

// Translate uses an OpenAI-compatible /v1/chat/completions endpoint to
// generate translations. We send a very strict system prompt that requires
// JSON-only output so we can parse the result deterministically without
// regex hacks. The whole batch is sent in a single round-trip per missing
// variant pair to minimise latency.
func (t *openAICompatTranslator) Translate(ctx context.Context, items []NewsItem) ([]NewsItem, error) {
	if len(items) == 0 {
		return items, nil
	}
	if t.cfg.Model == "" {
		return items, errors.New("openai-compat translator: model not configured")
	}
	// Backfill source-language variants so callers don't have to tag first.
	items = tagNewsItemsWithLanguage(items, "")
	for _, target := range t.cfg.Targets {
		pending := collectPendingIndices(items, target)
		if len(pending) == 0 {
			continue
		}
		t.translateBatch(ctx, items, pending, target)
	}
	return items, nil
}

func collectPendingIndices(items []NewsItem, target string) []int {
	pending := make([]int, 0, len(items))
	for i := range items {
		base := items[i].Language
		if base == "" {
			base = detectNewsLanguage(items[i])
		}
		if base == target {
			continue
		}
		if target == NewsLanguageZH && strings.TrimSpace(items[i].TitleZh) != "" && strings.TrimSpace(items[i].SummaryZh) != "" {
			continue
		}
		if target == NewsLanguageEN && strings.TrimSpace(items[i].TitleEn) != "" && strings.TrimSpace(items[i].SummaryEn) != "" {
			continue
		}
		pending = append(pending, i)
	}
	return pending
}

func (t *openAICompatTranslator) translateBatch(ctx context.Context, items []NewsItem, pending []int, target string) {
	type entry struct {
		Index   int    `json:"index"`
		Title   string `json:"title"`
		Summary string `json:"summary"`
	}
	type entryOut struct {
		Index   int    `json:"index"`
		Title   string `json:"title"`
		Summary string `json:"summary"`
	}
	input := make([]entry, 0, len(pending))
	for _, idx := range pending {
		input = append(input, entry{
			Index:   idx,
			Title:   strings.TrimSpace(items[idx].Title),
			Summary: strings.TrimSpace(items[idx].Summary),
		})
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return
	}
	targetName := languageDisplayName(target)
	systemPrompt := fmt.Sprintf(
		"You are a financial news translator. Translate the provided JSON array of news items into %s. "+
			"Return ONLY a JSON array with the same indices and the translated `title` and `summary` fields. "+
			"Preserve ticker symbols, numbers, dates and proper nouns. Do not add commentary.",
		targetName,
	)
	body, err := json.Marshal(map[string]any{
		"model": t.cfg.Model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": string(inputJSON)},
		},
		"temperature":     0.1,
		"response_format": map[string]string{"type": "json_object"},
	})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return
	}
	if len(parsed.Choices) == 0 {
		return
	}
	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	// Models occasionally wrap arrays in {"items":[...]} when response_format
	// requests json_object. Accept both shapes by sniffing the first non-space
	// character.
	var outs []entryOut
	if strings.HasPrefix(content, "[") {
		_ = json.Unmarshal([]byte(content), &outs)
	} else {
		var wrapper struct {
			Items []entryOut `json:"items"`
		}
		if err := json.Unmarshal([]byte(content), &wrapper); err == nil {
			outs = wrapper.Items
		}
	}
	for _, out := range outs {
		if out.Index < 0 || out.Index >= len(items) {
			continue
		}
		if strings.TrimSpace(out.Title) != "" {
			setLocalizedTitle(&items[out.Index], target, strings.TrimSpace(out.Title))
		}
		if strings.TrimSpace(out.Summary) != "" {
			setLocalizedSummary(&items[out.Index], target, strings.TrimSpace(out.Summary))
		}
	}
}

// ----- helpers -----

func setLocalizedTitle(item *NewsItem, target, value string) {
	switch target {
	case NewsLanguageZH:
		if strings.TrimSpace(item.TitleZh) == "" {
			item.TitleZh = value
		}
	case NewsLanguageEN:
		if strings.TrimSpace(item.TitleEn) == "" {
			item.TitleEn = value
		}
	}
}

func setLocalizedSummary(item *NewsItem, target, value string) {
	switch target {
	case NewsLanguageZH:
		if strings.TrimSpace(item.SummaryZh) == "" {
			item.SummaryZh = value
		}
	case NewsLanguageEN:
		if strings.TrimSpace(item.SummaryEn) == "" {
			item.SummaryEn = value
		}
	}
}

func orAuto(language string) string {
	if strings.TrimSpace(language) == "" {
		return "auto"
	}
	return language
}

func languageDisplayName(code string) string {
	switch code {
	case NewsLanguageZH:
		return "Simplified Chinese (zh-CN)"
	case NewsLanguageEN:
		return "American English (en-US)"
	default:
		return code
	}
}

// translationCache is a small bounded in-memory cache keyed by
// (source, target, text) to avoid re-paying for the same translation when an
// article is re-fetched within the news TTL.
type translationCache struct {
	mu   sync.RWMutex
	data map[string]string
	cap  int
}

func newTranslationCache() *translationCache {
	return &translationCache{
		data: make(map[string]string, 256),
		cap:  4096,
	}
}

func (c *translationCache) key(text, source, target string) string {
	return source + "|" + target + "|" + text
}

func (c *translationCache) get(text, source, target string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[c.key(text, source, target)]
	return v, ok
}

func (c *translationCache) set(text, source, target, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.data) >= c.cap {
		// Naive trim: drop a single arbitrary entry to make room. We do not
		// care about LRU exactness here — translations are cheap to refetch
		// and the cache exists primarily to coalesce within a TTL window.
		for k := range c.data {
			delete(c.data, k)
			break
		}
	}
	c.data[c.key(text, source, target)] = value
}
