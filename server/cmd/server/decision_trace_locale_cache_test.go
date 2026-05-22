// decision_trace_locale_cache_test.go — F32 unit tests.
//
// These tests pin the three guarantees the locality cache makes:
//
//  1. Cache hits short-circuit the loader (no LLM round-trip).
//  2. Single-flight: concurrent misses run the loader exactly once.
//  3. Capacity bound: oldest entries get evicted instead of growing
//     the heap unbounded.
//
// Bonus: cover the language-detection helpers used by future
// short-circuit heuristics so we do not regress when adding them.
package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTranslationCacheReturnsCachedValueOnSecondCall(t *testing.T) {
	cache := newTranslationLocaleCache(8, time.Hour, time.Hour)
	calls := atomic.Int64{}
	loader := func() ([]string, []string, bool) {
		calls.Add(1)
		return []string{"中文"}, []string{"English"}, false
	}

	key := translationCacheKey("pm_plan", "critical", []string{"zh", "en"}, []string{"hello"})

	zh1, en1, failed1 := cache.GetOrLoad(key, loader)
	if failed1 {
		t.Fatalf("first call should not be marked failed")
	}
	if len(zh1) != 1 || zh1[0] != "中文" {
		t.Fatalf("unexpected zh on first call: %#v", zh1)
	}
	if len(en1) != 1 || en1[0] != "English" {
		t.Fatalf("unexpected en on first call: %#v", en1)
	}

	zh2, en2, failed2 := cache.GetOrLoad(key, loader)
	if failed2 {
		t.Fatalf("cached call should not be marked failed")
	}
	if zh2[0] != "中文" || en2[0] != "English" {
		t.Fatalf("cached result diverges from original: %#v / %#v", zh2, en2)
	}
	if calls.Load() != 1 {
		t.Fatalf("loader should run exactly once on cache hit, ran %d times", calls.Load())
	}
}

func TestTranslationCacheSingleFlightDedupesConcurrentMisses(t *testing.T) {
	cache := newTranslationLocaleCache(8, time.Hour, time.Hour)
	calls := atomic.Int64{}
	gate := make(chan struct{})
	loader := func() ([]string, []string, bool) {
		calls.Add(1)
		<-gate
		return []string{"ok"}, []string{"ok"}, false
	}

	key := translationCacheKey("daily_review", "standard", []string{"zh", "en"}, []string{"item-a", "item-b"})

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			zh, en, _ := cache.GetOrLoad(key, loader)
			if len(zh) != 1 || len(en) != 1 {
				t.Errorf("unexpected payload: zh=%v en=%v", zh, en)
			}
		}()
	}

	// Give all goroutines time to queue up on the in-flight barrier.
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("loader should run exactly once across %d concurrent callers, ran %d times", goroutines, got)
	}
}

func TestTranslationCacheFailureTTLIsShorterThanSuccess(t *testing.T) {
	cache := newTranslationLocaleCache(8, time.Hour, 10*time.Millisecond)
	calls := atomic.Int64{}
	loader := func() ([]string, []string, bool) {
		calls.Add(1)
		return nil, nil, true
	}

	key := translationCacheKey("pm_plan", "simple", []string{"zh", "en"}, []string{"flaky"})

	_, _, failed1 := cache.GetOrLoad(key, loader)
	if !failed1 {
		t.Fatalf("first call should report failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("loader should have run once, got %d", calls.Load())
	}

	// Within the failure TTL the cached failure is returned without
	// calling the loader again.
	_, _, failed2 := cache.GetOrLoad(key, loader)
	if !failed2 || calls.Load() != 1 {
		t.Fatalf("expected cached failure, got failed=%v calls=%d", failed2, calls.Load())
	}

	// After the failure TTL elapses the loader runs again so a
	// recovered upstream re-populates the cache.
	time.Sleep(20 * time.Millisecond)
	_, _, _ = cache.GetOrLoad(key, loader)
	if calls.Load() != 2 {
		t.Fatalf("expected loader rerun after failure TTL, calls=%d", calls.Load())
	}
}

func TestTranslationCacheEvictsOldestWhenOverCapacity(t *testing.T) {
	cache := newTranslationLocaleCache(2, time.Hour, time.Hour)

	loadConstant := func(value string) translationLoader {
		return func() ([]string, []string, bool) {
			return []string{value}, []string{value}, false
		}
	}

	keys := []string{
		translationCacheKey("step", "tier", []string{"zh", "en"}, []string{"a"}),
		translationCacheKey("step", "tier", []string{"zh", "en"}, []string{"b"}),
		translationCacheKey("step", "tier", []string{"zh", "en"}, []string{"c"}),
	}
	for i, key := range keys {
		cache.GetOrLoad(key, loadConstant(string(rune('a'+i))))
	}

	if size := cache.Size(); size != 2 {
		t.Fatalf("expected cache to be capped at 2 entries, got %d", size)
	}

	// The oldest entry ("a") should have been evicted; the most
	// recent two ("b" and "c") must still hit without invoking the
	// loader.
	gotLoad := false
	cache.GetOrLoad(keys[1], func() ([]string, []string, bool) {
		gotLoad = true
		return nil, nil, false
	})
	if gotLoad {
		t.Fatalf("second-newest entry should still be cached after eviction")
	}
}

func TestTranslationCacheKeyIsStable(t *testing.T) {
	key1 := translationCacheKey("pm_plan", "critical", []string{"zh", "en"}, []string{"alpha", "beta"})
	key2 := translationCacheKey("pm_plan", "critical", []string{"zh", "en"}, []string{"alpha", "beta"})
	if key1 != key2 {
		t.Fatalf("identical inputs must produce identical keys, got %s vs %s", key1, key2)
	}

	keyDifferent := translationCacheKey("pm_plan", "critical", []string{"zh", "en"}, []string{"alpha", "BETA"})
	if key1 == keyDifferent {
		t.Fatalf("differing input must produce different key")
	}

	keyTierBust := translationCacheKey("pm_plan", "simple", []string{"zh", "en"}, []string{"alpha", "beta"})
	if key1 == keyTierBust {
		t.Fatalf("changing tier must bust the cache key")
	}
}

func TestLooksLikeChineseOnlyAcceptsTickerMixedText(t *testing.T) {
	if !looksLikeChineseOnly("美光科技 MU 的 NAND 业绩亮眼。") {
		t.Fatalf("ticker-mixed Chinese should still be detected as Chinese-dominant")
	}
	if looksLikeChineseOnly("Micron Technology beat estimates.") {
		t.Fatalf("English-only text should not be detected as Chinese")
	}
	if looksLikeChineseOnly("") {
		t.Fatalf("empty string has no CJK characters")
	}
}

func TestLooksLikeEnglishOnlyRejectsCJK(t *testing.T) {
	if !looksLikeEnglishOnly("Micron Technology beat estimates.") {
		t.Fatalf("ASCII-only English should be detected as English-only")
	}
	if looksLikeEnglishOnly("Micron 表现亮眼") {
		t.Fatalf("text containing CJK should not be detected as English-only")
	}
	if !looksLikeEnglishOnly("") {
		t.Fatalf("empty string contains no CJK characters")
	}
}
