package main

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

// TestConvertMemoryEntry_PassesThroughTemplateKeyAndPayload pins
// the wire-format contract introduced by migration 085: when a
// Memory row carries a TemplateKey + jsonb Payload, the JSON DTO
// must surface them under "templateKey" / "payload" so the frontend
// renderer (web/src/lib/lessonRenderer.ts) can localise the row.
//
// This test guards against silent regressions in convertMemoryEntry,
// which is the single fan-in funnel for every memory list/get/search
// endpoint. If somebody re-introduces the bug where Memory.Payload
// is dropped on the floor, this test fails before it ships.
func TestConvertMemoryEntry_PassesThroughTemplateKeyAndPayload(t *testing.T) {
	rawPayload := json.RawMessage(`{"sleeve":"trend","regime":"trend_up","trade_count":15,"win_rate":0.7333,"total_pnl":1240}`)
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mem := &repository.Memory{
		ID:          "11111111-1111-1111-1111-111111111111",
		FundID:      "ffffffff-ffff-ffff-ffff-ffffffffffff",
		Layer:       "attribution",
		Title:       sql.NullString{String: "Sleeve trend is profitable…", Valid: true},
		Content:     "english fallback",
		Visibility:  "private",
		Sensitivity: "internal",
		OriginKind:  "native",
		Tags:        []string{"winner", "sleeve:trend", "regime:trend_up"},
		TradingDate: sql.NullTime{Time: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC), Valid: true},
		CreatedAt:   now,
		UpdatedAt:   now,
		TemplateKey: sql.NullString{String: "attribution.lesson.sleeve_regime_winner", Valid: true},
		Payload:     rawPayload,
	}
	got := convertMemoryEntry(mem)
	if got.TemplateKey != "attribution.lesson.sleeve_regime_winner" {
		t.Fatalf("template key not surfaced: got %q", got.TemplateKey)
	}
	if string(got.Payload) != string(rawPayload) {
		t.Fatalf("payload not surfaced verbatim:\n got: %s\nwant: %s", string(got.Payload), string(rawPayload))
	}
	// Marshal the DTO and confirm both fields show up in the wire
	// format with the expected snake-cased keys consumers look for.
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(wire), `"templateKey":"attribution.lesson.sleeve_regime_winner"`) {
		t.Fatalf("templateKey missing from wire: %s", wire)
	}
	if !strings.Contains(string(wire), `"payload":{"sleeve":"trend"`) {
		t.Fatalf("payload missing from wire: %s", wire)
	}
}

// TestConvertMemoryEntry_LegacyRowOmitsI18nFields covers the
// backward-compat path. A Memory row with no template_key (the
// pre-085 reality) must serialise without "templateKey" or "payload"
// keys at all so legacy frontend builds keep working. The omitempty
// tags on api.MemoryEntry handle this; we assert the wire shape.
func TestConvertMemoryEntry_LegacyRowOmitsI18nFields(t *testing.T) {
	mem := &repository.Memory{
		ID:          "22222222-2222-2222-2222-222222222222",
		FundID:      "ffffffff-ffff-ffff-ffff-ffffffffffff",
		Layer:       "attribution",
		Content:     "raw english",
		Visibility:  "private",
		Sensitivity: "internal",
		OriginKind:  "native",
		Tags:        []string{"insufficient_data"},
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		// TemplateKey deliberately invalid (NULL in DB).
	}
	got := convertMemoryEntry(mem)
	if got.TemplateKey != "" {
		t.Fatalf("template key should be empty for legacy row; got %q", got.TemplateKey)
	}
	if len(got.Payload) != 0 {
		t.Fatalf("payload should be empty for legacy row; got %s", string(got.Payload))
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(wire), `"templateKey"`) {
		t.Fatalf("legacy DTO must not emit templateKey field: %s", wire)
	}
	if strings.Contains(string(wire), `"payload"`) {
		t.Fatalf("legacy DTO must not emit payload field: %s", wire)
	}
}

// TestConvertAgentLearningRecord_PropagatesTemplateAndPayload mirrors
// the MemoryEntry test for the agent-learning UI path. The agent
// learning panel renders structured lessons (per-fund per-day) and
// must receive the same i18n contract — without it the per-agent
// learning tab would still render English while the rest of the UI
// flips to the user's locale.
func TestConvertAgentLearningRecord_PropagatesTemplateAndPayload(t *testing.T) {
	rawPayload := json.RawMessage(`{"sleeve":"mean_reversion","regime":"chop","trade_count":12,"win_rate":0.25,"total_pnl":-480,"avg_pnl_pct":-0.04,"avg_holding_days":2.1}`)
	mem := &repository.Memory{
		ID:          "33333333-3333-3333-3333-333333333333",
		FundID:      "ffffffff-ffff-ffff-ffff-ffffffffffff",
		Layer:       "attribution",
		Title:       sql.NullString{String: "Sleeve losing money", Valid: true},
		Content:     "english fallback body",
		Visibility:  "private",
		Sensitivity: "internal",
		OriginKind:  "native",
		Tags:        []string{"loser", "sleeve:mean_reversion", "regime:chop"},
		TemplateKey: sql.NullString{String: "attribution.lesson.sleeve_regime_loser", Valid: true},
		Payload:     rawPayload,
	}
	got := convertAgentLearningRecord(mem)
	if got.TemplateKey != "attribution.lesson.sleeve_regime_loser" {
		t.Fatalf("template key not surfaced: got %q", got.TemplateKey)
	}
	if string(got.Payload) != string(rawPayload) {
		t.Fatalf("payload not surfaced verbatim: got %s", string(got.Payload))
	}
}
