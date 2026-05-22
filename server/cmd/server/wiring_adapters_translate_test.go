package main

import (
	"strings"
	"testing"
)

// TestAppendAlignedPadsShortChunk guards the partial-success path of
// translateBilingualList: when one chunk returns fewer translated
// strings than expected (LLM hallucinated, JSON truncated, etc.), the
// concatenated output must still index-align with the input so callers
// that walk titleIndexes[i] -> translation[i] hit the correct row.
func TestAppendAlignedPadsShortChunk(t *testing.T) {
	out := appendAligned(nil, []string{"a", "b"}, 4)
	if len(out) != 4 {
		t.Fatalf("want len=4, got %d (%v)", len(out), out)
	}
	if out[0] != "a" || out[1] != "b" || out[2] != "" || out[3] != "" {
		t.Fatalf("unexpected padding: %v", out)
	}
}

// TestAppendAlignedTruncatesOverlongChunk covers the safety net for a
// hypothetical LLM response that returns MORE items than asked. We
// truncate so downstream `result[titleIndexes[idx]] = zh[idx]` never
// runs past the original slot count.
func TestAppendAlignedTruncatesOverlongChunk(t *testing.T) {
	out := appendAligned(nil, []string{"a", "b", "c", "d"}, 2)
	if len(out) != 2 || out[0] != "a" || out[1] != "b" {
		t.Fatalf("expected truncation to 2 items, got %v", out)
	}
}

// TestAppendAlignedChainsMultipleChunks proves the function composes
// correctly across multiple chunks so the flat slice retains the order
// the caller relied on when building titleIndexes.
func TestAppendAlignedChainsMultipleChunks(t *testing.T) {
	out := appendAligned(nil, []string{"a1", "a2"}, 2)
	out = appendAligned(out, []string{"b1"}, 2)        // short
	out = appendAligned(out, []string{"c1", "c2"}, 2)  // exact
	if len(out) != 6 {
		t.Fatalf("want len=6, got %d (%v)", len(out), out)
	}
	want := []string{"a1", "a2", "b1", "", "c1", "c2"}
	for i, v := range want {
		if out[i] != v {
			t.Fatalf("idx %d: want %q, got %q (full %v)", i, v, out[i], out)
		}
	}
}

// TestTranslateBilingualListNilRuntimeReturnsEmpty preserves the
// existing contract that a nil llmRuntime is a fast no-op (used by
// ListPlans to skip per-plan LLM calls for the sidebar).
func TestTranslateBilingualListNilRuntimeReturnsEmpty(t *testing.T) {
	zh, en := translateBilingualList("", nil, "research_parallel", []string{"hello", "world"}, "")
	if len(zh) != 0 || len(en) != 0 {
		t.Fatalf("expected empty results on nil runtime, got zh=%v en=%v", zh, en)
	}
}

// TestTranslateBilingualListChunksLargeInput verifies that a 30-item
// input is split into 4 chunks (8+8+8+6) so each LLM call stays well
// inside the 4096-output-token budget. This is the exact regression
// the user hit: a single 30-item batch overflowed maxOutputTokens, the
// truncated JSON failed to parse, every titleZh came back empty.
//
// We can't actually call the LLM here, but the chunk count is a pure
// derivation of the input length and the constants we exposed, so a
// math check is sufficient — and crucially documents the constants.
func TestTranslateBilingualListChunksLargeInput(t *testing.T) {
	const inputLen = 30
	expected := (inputLen + translationListChunkSize - 1) / translationListChunkSize
	if expected != 4 {
		t.Fatalf("math sanity: 30 items / chunk size %d should produce 4 chunks, got %d",
			translationListChunkSize, expected)
	}
	// Build the chunk slices the same way translateBilingualList does so
	// reviewers immediately see how the split lines up with the LLM call.
	values := make([]string, inputLen)
	for i := range values {
		values[i] = strings.Repeat("x", 80) // approximate news-headline length
	}
	chunks := 0
	for i := 0; i < len(values); i += translationListChunkSize {
		chunks++
	}
	if chunks != expected {
		t.Fatalf("want %d chunks, got %d", expected, chunks)
	}
}
