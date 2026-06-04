package attribution

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/fundai/server/internal/repository"
)

// TestGenerateLessons_EmitsTemplateAndPayload verifies the i18n
// contract introduced in migration 085 — every lesson type produces
// a non-empty TemplateKey plus a payload whose field set matches
// what the frontend dictionary is going to interpolate against.
//
// Concretely we want this test to fail loud the moment somebody:
//
//   - adds a new LessonKind without a TemplateKey,
//   - renames a payload field in lesson.go without bumping .vN,
//   - drops a payload field that the frontend still references.
//
// The frontend has a mirror test (web/src/lib/lessonRenderer.test.ts)
// that exercises the same template keys against the locale dictionary;
// together they pin both ends of the contract.
func TestGenerateLessons_EmitsTemplateAndPayload(t *testing.T) {
	tests := []struct {
		name             string
		report           AttributionReport
		wantTemplate     string
		wantPayloadKeys  []string
		wantSampleValues map[string]any // optional: spot-check a few values
	}{
		{
			name: "loser cell",
			report: AttributionReport{
				Window: Window{Days: 30},
				BySleeveRegime: []repository.SleeveRegimeStat{
					{Sleeve: "mean_reversion", Regime: "chop", TradeCount: 12, WinCount: 3, LossCount: 9, TotalPnL: -480, AvgPnLPct: -0.04, AvgHoldingDays: 2.1, WinRate: 0.25},
				},
			},
			wantTemplate:    "attribution.lesson.sleeve_regime_loser",
			wantPayloadKeys: []string{"sleeve", "regime", "trade_count", "win_rate", "total_pnl", "avg_pnl_pct", "avg_holding_days"},
			wantSampleValues: map[string]any{
				"sleeve":      "mean_reversion",
				"regime":      "chop",
				"trade_count": 12,
				// raw 0..1 ratio — UI multiplies by 100; see lessonRenderer.tsx
				"win_rate": 0.25,
			},
		},
		{
			name: "winner cell",
			report: AttributionReport{
				Window: Window{Days: 30},
				BySleeveRegime: []repository.SleeveRegimeStat{
					{Sleeve: "trend", Regime: "trend_up", TradeCount: 15, WinCount: 11, LossCount: 4, TotalPnL: 1240, AvgPnLPct: 0.08, AvgHoldingDays: 7, WinRate: 11.0 / 15.0},
				},
			},
			wantTemplate:    "attribution.lesson.sleeve_regime_winner",
			wantPayloadKeys: []string{"sleeve", "regime", "trade_count", "win_rate", "total_pnl", "avg_pnl_pct", "avg_holding_days"},
		},
		{
			name: "insufficient data — watching with earliest",
			report: AttributionReport{
				Window:           Window{Days: 30},
				OpenLotCount:     7,
				EarliestOpenedAt: sql.NullTime{Time: time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC), Valid: true},
			},
			wantTemplate:    "attribution.lesson.insufficient_data.watching",
			wantPayloadKeys: []string{"open_lot_count", "earliest_opened_at", "window_days"},
			wantSampleValues: map[string]any{
				"open_lot_count":     7,
				"earliest_opened_at": "2026-05-12",
				"window_days":        30,
			},
		},
		{
			name: "insufficient data — watching no date",
			report: AttributionReport{
				Window:       Window{Days: 30},
				OpenLotCount: 4,
				// EarliestOpenedAt deliberately invalid → fallback variant.
			},
			wantTemplate:    "attribution.lesson.insufficient_data.watching_no_date",
			wantPayloadKeys: []string{"open_lot_count", "window_days"},
		},
		{
			name: "insufficient data — empty",
			report: AttributionReport{
				Window: Window{Days: 30},
			},
			wantTemplate:    "attribution.lesson.insufficient_data.empty",
			wantPayloadKeys: []string{"window_days"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lessons := GenerateLessons(tc.report, LessonOptions{})
			if len(lessons) == 0 {
				t.Fatalf("no lessons produced; expected one with template %q", tc.wantTemplate)
			}
			l := lessons[0]
			if l.TemplateKey != tc.wantTemplate {
				t.Fatalf("template key: got %q want %q", l.TemplateKey, tc.wantTemplate)
			}
			if l.Payload == nil {
				t.Fatalf("payload is nil; expected keys %v", tc.wantPayloadKeys)
			}
			for _, k := range tc.wantPayloadKeys {
				if _, ok := l.Payload[k]; !ok {
					t.Errorf("payload missing key %q; got keys %v", k, payloadKeys(l.Payload))
				}
			}
			for k, v := range tc.wantSampleValues {
				if got := l.Payload[k]; got != v {
					t.Errorf("payload[%q] = %v (%T), want %v (%T)", k, got, got, v, v)
				}
			}
			// Title / Body must still be set so the legacy clients
			// (and the LLM lesson_replay context) keep working.
			if l.Title == "" || l.Body == "" {
				t.Errorf("title/body must not be empty when template is set; got title=%q body=%q",
					l.Title, l.Body)
			}
		})
	}
}

// TestGenerateLessons_PayloadIsJSONSerializable ensures every payload
// value round-trips through encoding/json without surprises. Our
// MemoryRepo persists the payload as jsonb, so a non-encodable value
// (channel, func, …) would only blow up at runtime when the nightly
// memory writer fires. Catching it here keeps the surface honest.
func TestGenerateLessons_PayloadIsJSONSerializable(t *testing.T) {
	report := AttributionReport{
		Window: Window{Days: 30},
		BySleeveRegime: []repository.SleeveRegimeStat{
			{Sleeve: "trend", Regime: "trend_up", TradeCount: 15, WinCount: 11, LossCount: 4, TotalPnL: 1240, AvgPnLPct: 0.08, AvgHoldingDays: 7, WinRate: 11.0 / 15.0},
		},
		OpenLotCount:     7,
		EarliestOpenedAt: sql.NullTime{Time: time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC), Valid: true},
	}
	lessons := GenerateLessons(report, LessonOptions{})
	for _, l := range lessons {
		if l.Payload == nil {
			continue
		}
		buf, err := json.Marshal(l.Payload)
		if err != nil {
			t.Fatalf("marshal payload for %s: %v", l.Kind, err)
		}
		// And round-trip decode → ensures no NaN / +Inf snuck in.
		var back map[string]any
		if err := json.Unmarshal(buf, &back); err != nil {
			t.Fatalf("unmarshal payload for %s: %v", l.Kind, err)
		}
	}
}

// TestBuildMemoryRow_PersistsTemplateAndPayload pins the lesson →
// repository.Memory mapping. A regression here means the i18n
// columns silently stop being written to the DB.
func TestBuildMemoryRow_PersistsTemplateAndPayload(t *testing.T) {
	stat := repository.SleeveRegimeStat{
		Sleeve: "trend", Regime: "trend_up", TradeCount: 15,
		TotalPnL: 1240, WinRate: 11.0 / 15.0, AvgPnLPct: 0.08, AvgHoldingDays: 7,
	}
	l := buildLoserLesson(stat) // any builder works; we only care about the mapping
	mem := buildMemoryRow("fund-1", "agent-1", time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC), l)

	if !mem.TemplateKey.Valid || mem.TemplateKey.String != l.TemplateKey {
		t.Fatalf("template_key not persisted: got %+v want %q", mem.TemplateKey, l.TemplateKey)
	}
	if len(mem.Payload) == 0 {
		t.Fatalf("payload not persisted")
	}
	var decoded map[string]any
	if err := json.Unmarshal(mem.Payload, &decoded); err != nil {
		t.Fatalf("persisted payload is not valid JSON: %v (%s)", err, string(mem.Payload))
	}
	if decoded["sleeve"] != "trend" {
		t.Fatalf("payload sleeve = %v want %q", decoded["sleeve"], "trend")
	}
}

// TestBuildMemoryRow_OmitsTemplateForLegacyLesson covers the
// fallback path: a Lesson without a TemplateKey (e.g. one produced
// by a pre-085 builder we forgot to migrate, or any future lesson
// type we haven't translated yet) must still produce a valid row,
// just with NULL template_key + NULL payload.
func TestBuildMemoryRow_OmitsTemplateForLegacyLesson(t *testing.T) {
	l := Lesson{
		Kind:     LessonSleeveRegimeLoser,
		Severity: SeverityCritical,
		Title:    "legacy title",
		Body:     "legacy body",
		Tags:     []string{"legacy"},
		// TemplateKey deliberately empty.
	}
	mem := buildMemoryRow("fund-1", "", time.Now(), l)
	if mem.TemplateKey.Valid {
		t.Fatalf("expected NULL template_key for legacy lesson, got %q", mem.TemplateKey.String)
	}
	if len(mem.Payload) != 0 {
		t.Fatalf("expected nil payload for legacy lesson, got %s", string(mem.Payload))
	}
	if mem.Content != "legacy body" {
		t.Fatalf("content lost: got %q", mem.Content)
	}
}

func payloadKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
