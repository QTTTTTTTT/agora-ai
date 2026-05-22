package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// ChatCacheConfig configures the in-process exact-match LLM cache.
type ChatCacheConfig struct {
	Enabled    bool
	TTL        time.Duration
	MaxEntries int
}

type chatCacheEntry struct {
	resp      *ChatResponse
	createdAt time.Time
}

type chatCacheInflight struct {
	wg   sync.WaitGroup
	resp *ChatResponse
	err  error
}

// ChatCacheLease represents ownership of, or a wait on, an in-flight request.
type ChatCacheLease struct {
	cache  *ChatCache
	key    string
	call   *chatCacheInflight
	leader bool
}

// ChatCache is a small TTL-bound exact-match cache for deterministic-ish LLM calls.
// It is intentionally process-local: a restart clears it and no prompt content is
// persisted outside memory.
type ChatCache struct {
	mu         sync.Mutex
	enabled    bool
	ttl        time.Duration
	maxEntries int
	items      map[string]chatCacheEntry
	inflight   map[string]*chatCacheInflight
}

// NewChatCache creates a new exact-match chat cache.
func NewChatCache(cfg ChatCacheConfig) *ChatCache {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	return &ChatCache{
		enabled:    cfg.Enabled,
		ttl:        ttl,
		maxEntries: maxEntries,
		items:      make(map[string]chatCacheEntry),
		inflight:   make(map[string]*chatCacheInflight),
	}
}

func (c *ChatCache) Get(key string) (*ChatResponse, bool) {
	if c == nil || !c.enabled || strings.TrimSpace(key) == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok {
		return nil, false
	}
	if time.Since(entry.createdAt) > c.ttl {
		delete(c.items, key)
		return nil, false
	}
	return cloneCachedChatResponse(entry.resp), true
}

func (c *ChatCache) Set(key string, resp *ChatResponse) {
	if c == nil || !c.enabled || strings.TrimSpace(key) == "" || resp == nil || strings.TrimSpace(resp.Content) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.items[key] = chatCacheEntry{resp: cloneChatResponse(resp), createdAt: now}
	c.evictLocked(now)
}

// Acquire registers an in-flight request for key. The first caller is the
// leader and should execute the provider request. Concurrent duplicate callers
// receive a non-leader lease and should call Wait instead of invoking the LLM.
func (c *ChatCache) Acquire(key string) *ChatCacheLease {
	if c == nil || !c.enabled || strings.TrimSpace(key) == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if call, ok := c.inflight[key]; ok {
		return &ChatCacheLease{cache: c, key: key, call: call, leader: false}
	}
	call := &chatCacheInflight{}
	call.wg.Add(1)
	c.inflight[key] = call
	return &ChatCacheLease{cache: c, key: key, call: call, leader: true}
}

func (l *ChatCacheLease) IsLeader() bool {
	return l != nil && l.leader
}

func (l *ChatCacheLease) Wait() (*ChatResponse, error) {
	if l == nil || l.call == nil {
		return nil, nil
	}
	l.call.wg.Wait()
	return cloneCachedChatResponse(l.call.resp), l.call.err
}

func (l *ChatCacheLease) Finish(resp *ChatResponse, err error) {
	if l == nil || l.cache == nil || l.call == nil || !l.leader {
		return
	}
	l.cache.mu.Lock()
	if current := l.cache.inflight[l.key]; current == l.call {
		delete(l.cache.inflight, l.key)
	}
	l.call.resp = cloneChatResponse(resp)
	l.call.err = err
	l.call.wg.Done()
	l.cache.mu.Unlock()
}

func (c *ChatCache) evictLocked(now time.Time) {
	for key, entry := range c.items {
		if now.Sub(entry.createdAt) > c.ttl {
			delete(c.items, key)
		}
	}
	for len(c.items) > c.maxEntries {
		var oldestKey string
		var oldest time.Time
		for key, entry := range c.items {
			if oldestKey == "" || entry.createdAt.Before(oldest) {
				oldestKey = key
				oldest = entry.createdAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.items, oldestKey)
	}
}

type chatCacheKeyPayload struct {
	Owner       string        `json:"owner"`
	Provider    Provider      `json:"provider"`
	Model       string        `json:"model"`
	BaseURL     string        `json:"base_url,omitempty"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	StepName    string        `json:"step_name,omitempty"`
	Messages    []ChatMessage `json:"messages"`
}

func buildChatCacheKey(req ChatRequest, cfg *ModelConfig) string {
	if cfg == nil {
		return ""
	}
	payload := chatCacheKeyPayload{
		Owner:       strings.TrimSpace(req.EffectiveOwner()),
		Provider:    cfg.Provider,
		Model:       strings.TrimSpace(cfg.ModelName),
		BaseURL:     strings.TrimSpace(cfg.BaseURL),
		MaxTokens:   cfg.MaxTokens,
		Temperature: cfg.Temperature,
		StepName:    strings.TrimSpace(req.StepName),
		Messages:    req.Messages,
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func cloneChatResponse(resp *ChatResponse) *ChatResponse {
	if resp == nil {
		return nil
	}
	cp := *resp
	return &cp
}

func cloneCachedChatResponse(resp *ChatResponse) *ChatResponse {
	cp := cloneChatResponse(resp)
	if cp == nil {
		return nil
	}
	cp.LatencyMs = 0
	cp.InputCost = 0
	cp.OutputCost = 0
	cp.TotalCost = 0
	cp.InputPrice = 0
	cp.OutputPrice = 0
	cp.TotalPrice = 0
	return cp
}
