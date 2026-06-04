package main

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

// TestMemoryRowsToLessonDTO_PassesThroughTemplateAndPayload pins the
// S15 contract on the second consumer of the memories table — the
// StrategyAttributionPanel rail. The first consumer (MemoryEntry) is
// covered by memory_i18n_test.go. We test both because they live in
// completely different DTO types and could drift independently if a
// future refactor only touches one of them.
//
// What we want this test to fail on:
//
//   - somebody removes the TemplateKey/Payload assignment in
//     memoryRowsToLessonDTO,
//   - jsonb bytes get re-marshalled and the field-set drifts (e.g. a
//     wrapper {"data": …} appears),
//   - a legacy row (NULL template_key) accidentally writes the empty
//     string into the wire format and breaks the omitempty contract
//     downstream consumers (and frontend snapshot tests) rely on.
func TestMemoryRowsToLessonDTO_PassesThroughTemplateAndPayload(t *testing.T) {
	rawPayload := json.RawMessage(`{"sleeve":"trend","regime":"trend_up","trade_count":15,"win_rate":0.7333,"total_pnl":1240,"avg_pnl_pct":0.08,"avg_holding_days":7}`)
	now := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	rows := []repository.Memory{
		{
			ID:          "i18n-row",
			FundID:      "fund-1",
			Layer:       "attribution",
			Title:       sql.NullString{String: "Sleeve trend is profitable", Valid: true},
			Content:     "english fallback body",
			Tags:        []string{"winner", "sleeve:trend", "regime:trend_up"},
			CreatedAt:   now,
			TemplateKey: sql.NullString{String: "attribution.lesson.sleeve_regime_winner", Valid: true},
			Payload:     rawPayload,
		},
		{
			ID:        "legacy-row",
			FundID:    "fund-1",
			Layer:     "attribution",
			Title:     sql.NullString{String: "Old lesson", Valid: true},
			Content:   "old english body",
			Tags:      []string{"loser", "sleeve:mean_reversion", "regime:chop"},
			CreatedAt: now.Add(-time.Hour),
			// TemplateKey deliberately invalid (NULL in DB) — the row
			// predates migration 085 and must keep working unchanged.
		},
	}

	dtos := memoryRowsToLessonDTO(rows)
	if len(dtos) != 2 {
		t.Fatalf("expected 2 DTOs, got %d", len(dtos))
	}

	// Newest-first ordering is asserted by sort.SliceStable on
	// CreatedAt; the i18n row was created later so it should be first.
	want, legacy := dtos[0], dtos[1]
	if want.TemplateKey != "attribution.lesson.sleeve_regime_winner" {
		t.Fatalf("template key not surfaced: got %q", want.TemplateKey)
	}
	if string(want.Payload) != string(rawPayload) {
		t.Fatalf("payload not surfaced verbatim:\n got: %s\nwant: %s", string(want.Payload), string(rawPayload))
	}
	if legacy.TemplateKey != "" {
		t.Fatalf("legacy row leaked a non-empty template_key: %q", legacy.TemplateKey)
	}
	if len(legacy.Payload) != 0 {
		t.Fatalf("legacy row leaked a payload: %s", string(legacy.Payload))
	}

	// Wire-format guard: marshal both DTOs and check omitempty does
	// the right thing — i18n row keeps the fields, legacy row drops them.
	encI18n, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal i18n: %v", err)
	}
	if !strings.Contains(string(encI18n), `"templateKey":"attribution.lesson.sleeve_regime_winner"`) {
		t.Fatalf("templateKey missing from wire: %s", encI18n)
	}
	if !strings.Contains(string(encI18n), `"payload":{"sleeve":"trend"`) {
		t.Fatalf("payload missing from wire: %s", encI18n)
	}
	encLegacy, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if strings.Contains(string(encLegacy), `"templateKey"`) {
		t.Fatalf("legacy DTO must not emit templateKey field: %s", encLegacy)
	}
	if strings.Contains(string(encLegacy), `"payload"`) {
		t.Fatalf("legacy DTO must not emit payload field: %s", encLegacy)
	}
}

// TestMemoryRowsToLessonDTO_OnlyWhitespaceTemplateKeyTreatedAsLegacy
// is a small but real edge case: the migration's CHECK constraint
// rejects a whitespace-only template_key, but a coding bug somewhere
// upstream could still set TemplateKey.Valid=true with a blank string.
// We treat it as legacy rather than emitting "" on the wire — that's
// what the frontend renderer already does on its end, and matching the
// behaviour here keeps the contract symmetric.
func TestMemoryRowsToLessonDTO_OnlyWhitespaceTemplateKeyTreatedAsLegacy(t *testing.T) {
	rows := []repository.Memory{
		{
			ID:          "spurious",
			FundID:      "fund-1",
			Layer:       "attribution",
			Content:     "body",
			Tags:        []string{"insufficient_data"},
			CreatedAt:   time.Now(),
			TemplateKey: sql.NullString{String: "   ", Valid: true},
		},
	}
	dtos := memoryRowsToLessonDTO(rows)
	if dtos[0].TemplateKey != "" {
		t.Fatalf("whitespace template_key must be sanitised, got %q", dtos[0].TemplateKey)
	}
	if len(dtos[0].Payload) != 0 {
		t.Fatalf("expected no payload for legacy-shaped row, got %s", string(dtos[0].Payload))
	}
}
