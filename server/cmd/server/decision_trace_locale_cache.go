// decision_trace_locale_cache.go — F32 DecisionCenter locality cache.
//
// Why this file exists
// --------------------
// Before F32 every GET /api/funds/{fundId}/decision-trace fired 3-5
// synchronous LLM round-trips on the request path to populate ZH/EN
// variants of plan reasoning, action reasoning, discussion summary and
// consensus lists (see translateBilingualText / translateBilingualList).
// With Gemini-via-OpenAI-compat sitting behind a slow gateway each
// click in the Decision Center cost 10-30 s of wall time even though
// the underlying text never changed.
//
// The cache here gives us three independent wins:
//
//  1. **Content-addressed memoisation.** Translations are pure
//     functions of (step, tier, language-pair, source). Hashing on
//     SHA-256(payload) means the second click on the same plan — or
//     any plan sharing action reasoning text — is a memory hit.
//  2. **Single-flight de-duplication.** Two concurrent decision-trace
//     requests for the same plan no longer issue duplicate LLM calls;
//     the loser waits on the winner's result.
//  3. **Negative caching.** Failures are remembered for a short TTL so
//     a flaky upstream cannot trigger N retries per click. Failure TTL
//     is deliberately shorter than the success TTL so transient
//     outages clear on their own.
//
// Scope: process-local on purpose. Translations are non-critical
// metadata; the source text is the canonical store. We trade a small
// duplication cost across replicas for zero coordination overhead.
//
// Bounded by entry count, not bytes — the dominant cost is the LLM
// round-trip avoided, not the few KB of JSON we hold. A naive
// eviction policy (drop arbitrary key when capacity reached) is fine
// because the cache is read-mostly and contents are cheap to refetch.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// translationCacheEntry stores the result of a translation lookup. We
// keep both the positive (zh/en lists) and the negative outcome (err
// boolean) so we can short-circuit retries from the cache directly.
type translationCacheEntry struct {
	zh        []string
	en        []string
	failed    bool      // true when the upstream call returned no usable data
	expiresAt time.Time // when this entry becomes stale and may be re-fetched
}

// translationCacheFlight is the single-flight barrier used to dedupe
// concurrent lookups for the same key. The first goroutine populates
// done; siblings wait on it and then read the result.
type translationCacheFlight struct {
	done chan struct{}
	res  translationCacheEntry
}

// translationLocaleCache is the LRU-by-arrival cache shared by the two
// bilingual translation helpers. It is process-local and goroutine
// safe.
//
// We deliberately do NOT use a per-key TTL goroutine. Stale entries
// are evicted lazily on read because the cache is small (a few
// thousand entries) and the dominant cost is the LLM round-trip the
// cache prevents, not the few bytes of stale data we briefly retain.
type translationLocaleCache struct {
	mu          sync.Mutex
	entries     map[string]translationCacheEntry
	inFlight    map[string]*translationCacheFlight
	capacity    int
	successTTL  time.Duration
	failureTTL  time.Duration
	insertion   []string // FIFO insertion order for naive O(1) eviction
	nowFunc     func() time.Time
}

// newTranslationLocaleCache returns a cache with sensible defaults.
// Capacity / TTLs are configurable for tests; production callers
// should rely on the defaults.
func newTranslationLocaleCache(capacity int, successTTL, failureTTL time.Duration) *translationLocaleCache {
	if capacity <= 0 {
		capacity = 4096
	}
	if successTTL <= 0 {
		successTTL = 24 * time.Hour
	}
	if failureTTL <= 0 {
		failureTTL = 30 * time.Second
	}
	return &translationLocaleCache{
		entries:    make(map[string]translationCacheEntry, capacity),
		inFlight:   make(map[string]*translationCacheFlight),
		capacity:   capacity,
		successTTL: successTTL,
		failureTTL: failureTTL,
		insertion:  make([]string, 0, capacity),
		nowFunc:    time.Now,
	}
}

// translationCacheKey produces a stable hash for (stepName, tier,
// targetLanguages, values). Two calls with the same inputs produce
// the same key so the cache hits across goroutines and requests.
//
// We hash all components together so a tier change or step rename
// invalidates entries automatically — there is no risk of a "wrong
// model wrote this cell" footgun.
func translationCacheKey(stepName, tier string, targets []string, values []string) string {
	payload := struct {
		Step    string   `json:"step"`
		Tier    string   `json:"tier"`
		Targets []string `json:"targets"`
		Values  []string `json:"values"`
	}{
		Step:    strings.TrimSpace(stepName),
		Tier:    strings.TrimSpace(tier),
		Targets: append([]string(nil), targets...),
		Values:  append([]string(nil), values...),
	}
	bytes, err := json.Marshal(payload)
	if err != nil {
		// Marshal can only fail for unsupported types; the fields
		// above are all primitive strings so this branch is
		// unreachable in practice. We still return a deterministic
		// fallback so the call site is never blocked.
		hashed := sha256.Sum256([]byte(strings.Join(values, "|")))
		return hex.EncodeToString(hashed[:])
	}
	hashed := sha256.Sum256(bytes)
	return hex.EncodeToString(hashed[:])
}

// translationLoader is the fetcher invoked exactly once per cache
// miss (with single-flight guarding concurrent misses). Returning
// failed=true (or err) feeds the negative cache so a flaky upstream
// does not trigger a retry storm.
type translationLoader func() (zh []string, en []string, failed bool)

// GetOrLoad returns the cached translation if present and fresh.
// Otherwise the loader runs under a single-flight lock; concurrent
// callers for the same key all observe the same outcome.
func (c *translationLocaleCache) GetOrLoad(key string, loader translationLoader) (zh []string, en []string, failed bool) {
	now := c.nowFunc()

	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && now.Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.zh, entry.en, entry.failed
	}
	if flight, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		<-flight.done
		return flight.res.zh, flight.res.en, flight.res.failed
	}
	flight := &translationCacheFlight{done: make(chan struct{})}
	c.inFlight[key] = flight
	c.mu.Unlock()

	gotZh, gotEn, gotFailed := loader()

	ttl := c.successTTL
	if gotFailed {
		ttl = c.failureTTL
	}
	entry := translationCacheEntry{
		zh:        append([]string(nil), gotZh...),
		en:        append([]string(nil), gotEn...),
		failed:    gotFailed,
		expiresAt: c.nowFunc().Add(ttl),
	}
	flight.res = entry

	c.mu.Lock()
	delete(c.inFlight, key)
	c.entries[key] = entry
	c.insertion = append(c.insertion, key)
	if len(c.entries) > c.capacity {
		c.evictOldestLocked()
	}
	c.mu.Unlock()
	close(flight.done)

	return entry.zh, entry.en, entry.failed
}

// evictOldestLocked drops the oldest insertion-order entries until we
// are back under capacity. Caller MUST hold c.mu.
func (c *translationLocaleCache) evictOldestLocked() {
	for len(c.entries) > c.capacity && len(c.insertion) > 0 {
		key := c.insertion[0]
		c.insertion = c.insertion[1:]
		delete(c.entries, key)
	}
}

// Size reports the number of currently cached entries. Exposed for
// tests / future admin metrics.
func (c *translationLocaleCache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// translationTargetsForList / translationTargetsForText return the
// target-language pair used to key the cache. We keep the two
// constants in a single place so future additions of more languages
// only need editing here.
var (
	translationTargetsBilingual = []string{"zh", "en"}
)

// looksLikeChineseOnly returns true when every non-whitespace
// character in s is in a CJK block. We use this to short-circuit
// translation when the source is already in the user's preferred
// language and the only "translation" needed is a copy.
//
// The detector is intentionally generous about whitespace, digits,
// ASCII punctuation, and basic Latin symbols (commonly mixed into
// CJK financial text) so a single English ticker like "MU" inside a
// Chinese sentence does not flip the language.
func looksLikeChineseOnly(s string) bool {
	hasCJK := false
	for _, r := range s {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF:
			hasCJK = true
		case r >= 0x3400 && r <= 0x4DBF:
			hasCJK = true
		case r >= 0x20000 && r <= 0x2A6DF:
			hasCJK = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			// whitespace ignored
		case r >= '0' && r <= '9':
			// digits OK in either language
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			// allow short Latin tokens (tickers, units); do not flip
			// the language for them.
		case strings.ContainsRune(",.;:!?()[]{}/\\-+*%&'\"`~_<>=@#$^|，。；：！？（）【】「」、—…", r):
			// punctuation OK
		default:
			// any other character (e.g. extended Latin letters,
			// Cyrillic, Arabic, emoji) makes the detector return
			// false so we still translate.
			return false
		}
	}
	return hasCJK
}

// looksLikeEnglishOnly returns true when the source text contains no
// CJK characters. ASCII English plus numbers / common punctuation
// passes. This intentionally allows mixed European accented Latin
// (which the LLM can render in EN without a real translation step)
// because we still treat it as English-source for caching purposes.
func looksLikeEnglishOnly(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return false
		}
		if r >= 0x3400 && r <= 0x4DBF {
			return false
		}
		if r >= 0x20000 && r <= 0x2A6DF {
			return false
		}
	}
	return true
}
