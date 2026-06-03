package llm

import (
	"context"
	"sync"
	"testing"
)

func TestReplaceSystemConfig_BasicSwap(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderOpenAI: "old-openai"},
		map[ModelTier]*ModelConfig{},
		nil, nil,
	)

	if got := router.SystemAPIKeySnapshot()[ProviderOpenAI]; got != "old-openai" {
		t.Fatalf("initial key wrong: %q", got)
	}
	if !router.HasProviderKey(ProviderOpenAI) {
		t.Fatalf("HasProviderKey openai false")
	}
	if router.HasProviderKey(ProviderClaude) {
		t.Fatalf("HasProviderKey claude true before reload")
	}

	gen0 := ReloadGeneration()
	router.ReplaceSystemConfig(
		map[Provider]string{
			ProviderOpenAI: "new-openai",
			ProviderClaude: "new-claude",
		},
		map[ModelTier]*ModelConfig{},
	)
	if ReloadGeneration() != gen0+1 {
		t.Fatalf("reload generation did not advance: got %d want %d", ReloadGeneration(), gen0+1)
	}
	if got := router.SystemAPIKeySnapshot()[ProviderOpenAI]; got != "new-openai" {
		t.Fatalf("openai key not replaced: %q", got)
	}
	if got := router.SystemAPIKeySnapshot()[ProviderClaude]; got != "new-claude" {
		t.Fatalf("claude key not added: %q", got)
	}
	if !router.HasProviderKey(ProviderClaude) {
		t.Fatalf("HasProviderKey claude still false after reload")
	}
}

func TestReplaceSystemConfig_ClearAll(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderOpenAI: "x", ProviderClaude: "y"},
		map[ModelTier]*ModelConfig{},
		nil, nil,
	)
	router.ReplaceSystemConfig(nil, nil)
	if router.HasProviderKey(ProviderOpenAI) {
		t.Fatalf("openai key not cleared")
	}
	if router.HasProviderKey(ProviderClaude) {
		t.Fatalf("claude key not cleared")
	}
	snap := router.SystemAPIKeySnapshot()
	if len(snap) != 0 {
		t.Fatalf("snapshot not empty: %d", len(snap))
	}
}

func TestSystemAPIKeySnapshot_IsDefensiveCopy(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderOpenAI: "real"},
		map[ModelTier]*ModelConfig{},
		nil, nil,
	)
	snap := router.SystemAPIKeySnapshot()
	snap[ProviderOpenAI] = "tampered"
	snap[ProviderClaude] = "injected"
	if router.SystemAPIKeySnapshot()[ProviderOpenAI] != "real" {
		t.Fatalf("internal map was mutated through snapshot")
	}
	if router.HasProviderKey(ProviderClaude) {
		t.Fatalf("internal map gained tampered claude key")
	}
}

// TestReplaceSystemConfig_RaceWithResolve hammers the router with
// concurrent ResolveModel readers while a writer flips the
// systemAPIKeys map. Must pass under `go test -race`.
func TestReplaceSystemConfig_RaceWithResolve(t *testing.T) {
	router := NewModelRouter(
		map[Provider]string{ProviderOpenAI: "k0"},
		map[ModelTier]*ModelConfig{},
		nil, nil,
	)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = router.ResolveModel(context.Background(), &ChatRequest{
					UserID:    "u",
					ModelTier: TierStandard,
				})
				_ = router.HasProviderKey(ProviderOpenAI)
				_ = router.SystemAPIKeySnapshot()
			}
		}()
	}

	for i := 0; i < 200; i++ {
		key := "k0"
		if i%2 == 0 {
			key = "k-flip"
		}
		router.ReplaceSystemConfig(
			map[Provider]string{ProviderOpenAI: key},
			map[ModelTier]*ModelConfig{},
		)
	}

	close(stop)
	wg.Wait()
}
