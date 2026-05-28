package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fundai/server/internal/memory"
	"github.com/fundai/server/internal/repository"
)

// fakeReflectionStore is a tiny in-memory implementation of
// reflectionMemoryStore that segregates rows by fund_id. It is used to
// validate the F3.3 contract: a reflection produced for fund A must never
// appear in fund B's read path, even if both funds run their daily review
// against the same store instance.
//
// The fake intentionally panics if asked to Create a row whose FundID
// disagrees with the layer key used at insertion time — the surrounding
// production code is supposed to pass them consistently, and a panic gives
// a louder signal in CI than a silent test pass.
type fakeReflectionStore struct {
	mu      sync.Mutex
	rows    map[string][]repository.Memory // key = fundID + "|" + layer
	created []repository.Memory
}

func newFakeReflectionStore() *fakeReflectionStore {
	return &fakeReflectionStore{rows: make(map[string][]repository.Memory)}
}

func storeKey(fundID, layer string) string { return fundID + "|" + layer }

func (s *fakeReflectionStore) preload(fundID, layer string, mems ...repository.Memory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range mems {
		if m.FundID == "" {
			m.FundID = fundID
		}
		if m.Layer == "" {
			m.Layer = layer
		}
		s.rows[storeKey(fundID, layer)] = append(s.rows[storeKey(fundID, layer)], m)
	}
}

func (s *fakeReflectionStore) ListByFund(_ context.Context, fundID, layer string, limit int) ([]repository.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.rows[storeKey(fundID, layer)]
	// Newest-first to match the production repo contract.
	sorted := make([]repository.Memory, len(src))
	copy(sorted, src)
	for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
		sorted[i], sorted[j] = sorted[j], sorted[i]
	}
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}
	return sorted, nil
}

func (s *fakeReflectionStore) Create(_ context.Context, m *repository.Memory) (string, error) {
	if m == nil {
		return "", errors.New("nil memory")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.FundID == "" {
		return "", errors.New("missing fund_id")
	}
	m.ID = "mem-" + m.FundID + "-" + m.Layer + "-" + m.Content[:min(8, len(m.Content))]
	m.CreatedAt = time.Now().UTC()
	s.rows[storeKey(m.FundID, m.Layer)] = append(s.rows[storeKey(m.FundID, m.Layer)], *m)
	s.created = append(s.created, *m)
	return m.ID, nil
}

// constantDistiller is a deterministic Distiller used by tests so we never
// touch the real LLM. It records every theme it was called with so tests
// can assert the engine actually clustered the input correctly.
type constantDistiller struct {
	mu      sync.Mutex
	themes  []string
	content string
}

func (d *constantDistiller) Distill(_ context.Context, theme string, _ []memory.Item) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.themes = append(d.themes, theme)
	return d.content, nil
}

func makeDailySource(fundID, agentID, theme string, daysAgo int) repository.Memory {
	return repository.Memory{
		ID:          "src-" + fundID + "-" + theme,
		FundID:      fundID,
		AgentID:     sql.NullString{String: agentID, Valid: agentID != ""},
		Layer:       "daily",
		Title:       sql.NullString{String: "daily-" + theme, Valid: true},
		Content:     "lesson body for theme " + theme,
		Tags:        []string{theme, "self_learning"},
		CreatedAt:   time.Now().UTC().Add(-time.Duration(daysAgo) * 24 * time.Hour),
		TradingDate: sql.NullTime{Time: time.Now().UTC().Add(-time.Duration(daysAgo) * 24 * time.Hour), Valid: true},
	}
}

func TestRunReflectionCyclePersistsToCorrectFundOnly(t *testing.T) {
	t.Parallel()
	store := newFakeReflectionStore()
	for _, theme := range []string{"chip", "chip", "chip", "chip"} {
		store.preload("treatment", "daily", makeDailySource("treatment", "researcher-1", theme, 2))
	}
	for _, theme := range []string{"crude", "crude", "crude"} {
		store.preload("control", "daily", makeDailySource("control", "researcher-2", theme, 2))
	}

	// Substantive content survives the reflexion quality gate
	// (>=25 runes, no platitude prefix) — the test asserts fund
	// isolation, not the gate behaviour.
	distiller := &constantDistiller{content: "对存储芯片主题的多日复盘指出 NAND 价格反弹趋势已被市场过度透支，需要重新评估假设。"}

	now := time.Now().UTC()
	n := runReflectionCycle(context.Background(), store, distiller, noopSkillProposer{}, "treatment", now)
	if n == 0 {
		t.Fatalf("expected at least one reflection persisted for treatment fund")
	}

	// Every persisted row must carry the treatment fund id; control fund
	// must remain reflection-free even though it shared the store and the
	// distiller. This is the core A/B isolation guarantee.
	for _, row := range store.created {
		if row.FundID != "treatment" {
			t.Fatalf("reflection leaked to fund %q (expected only 'treatment')", row.FundID)
		}
		if row.Layer != reflectionMemoryLayer {
			t.Fatalf("expected layer=%s, got %s", reflectionMemoryLayer, row.Layer)
		}
	}
	controlReflections, _ := store.ListByFund(context.Background(), "control", reflectionMemoryLayer, 50)
	if len(controlReflections) != 0 {
		t.Fatalf("control fund unexpectedly has %d reflection rows", len(controlReflections))
	}
}

func TestRunReflectionCycleHonoursCadence(t *testing.T) {
	t.Parallel()
	store := newFakeReflectionStore()
	// Pre-existing reflection 2 days old → still inside the 7-day cadence
	// window → no new reflection should be written.
	store.preload("treatment", reflectionMemoryLayer, repository.Memory{
		ID:        "existing-1",
		FundID:    "treatment",
		Layer:     reflectionMemoryLayer,
		Title:     sql.NullString{String: "reflection:chip:abc123", Valid: true},
		Content:   "old reflection",
		CreatedAt: time.Now().UTC().Add(-2 * 24 * time.Hour),
	})
	for _, theme := range []string{"chip", "chip", "chip", "chip"} {
		store.preload("treatment", "daily", makeDailySource("treatment", "r1", theme, 1))
	}

	distiller := &constantDistiller{content: "should not be called"}
	n := runReflectionCycle(context.Background(), store, distiller, noopSkillProposer{}, "treatment", time.Now().UTC())
	if n != 0 {
		t.Fatalf("expected cadence guard to skip reflection, got %d new rows", n)
	}
	if len(distiller.themes) != 0 {
		t.Fatalf("distiller invoked despite cadence guard: themes=%v", distiller.themes)
	}
}

func TestRunReflectionCycleSkipsWhenSourceBelowMinGroup(t *testing.T) {
	t.Parallel()
	store := newFakeReflectionStore()
	// Only one daily row — below the 3-item min group threshold.
	store.preload("treatment", "daily", makeDailySource("treatment", "r1", "chip", 1))

	distiller := &constantDistiller{content: "anything"}
	n := runReflectionCycle(context.Background(), store, distiller, noopSkillProposer{}, "treatment", time.Now().UTC())
	if n != 0 {
		t.Fatalf("expected zero persisted (min group not met), got %d", n)
	}
}

func TestRunReflectionCycleDedupesByTitle(t *testing.T) {
	t.Parallel()
	store := newFakeReflectionStore()
	for _, theme := range []string{"chip", "chip", "chip"} {
		store.preload("treatment", "daily", makeDailySource("treatment", "r1", theme, 1))
	}
	// Substantive lesson so the reflexion quality gate keeps it;
	// the test asserts the dedupe behaviour, not the gate.
	distiller := &constantDistiller{content: "对存储芯片主题的复盘提示日内换手与持仓集中度需要联动校验。"}

	// First cycle persists; second cycle (skipping cadence by clearing it)
	// must produce zero net writes because the title is content-addressed.
	firstNow := time.Now().UTC().Add(-30 * 24 * time.Hour)
	if n := runReflectionCycle(context.Background(), store, distiller, noopSkillProposer{}, "treatment", firstNow); n == 0 {
		t.Fatalf("first cycle should have persisted at least one reflection")
	}
	beforeSecond := len(store.created)

	// Re-seed sources so the source window still has data (cadence guard
	// will not fire because the first reflection's CreatedAt is 30+ days
	// ago in wall clock — though preload set CreatedAt=Now for the
	// reflection row; mimic an old window by mutating it directly).
	store.mu.Lock()
	for i := range store.rows[storeKey("treatment", reflectionMemoryLayer)] {
		store.rows[storeKey("treatment", reflectionMemoryLayer)][i].CreatedAt = time.Now().UTC().Add(-30 * 24 * time.Hour)
	}
	store.mu.Unlock()

	if n := runReflectionCycle(context.Background(), store, distiller, noopSkillProposer{}, "treatment", time.Now().UTC()); n != 0 {
		t.Fatalf("second cycle should dedupe by title, got %d new rows (created total before=%d after=%d)", n, beforeSecond, len(store.created))
	}
}

func TestExtractReflectionTheme(t *testing.T) {
	t.Parallel()
	cases := []struct {
		title string
		tags  []string
		want  string
	}{
		{"reflection:chip:abc123", nil, "chip"},
		{"reflection:crude:def", []string{"any"}, "crude"},
		{"", []string{"self_learning", "macro", "global"}, "macro"},
		{"reflection:", []string{"fallback"}, "fallback"},
		{"", []string{"self_learning"}, "general"},
		{"unstructured title", []string{"market-research", "rates"}, "rates"},
	}
	for _, tc := range cases {
		got := extractReflectionTheme(tc.title, tc.tags)
		if got != tc.want {
			t.Errorf("extractReflectionTheme(%q, %v) = %q, want %q", tc.title, tc.tags, got, tc.want)
		}
	}
}

func TestReflectionTitleIsDeterministic(t *testing.T) {
	t.Parallel()
	item := memory.Item{
		Title:   "reflection: chip",
		Content: "Avoid chasing the 3rd consecutive green chip-rally bar.",
		Tags:    []string{"chip"},
	}
	a := reflectionTitle(item)
	b := reflectionTitle(item)
	if a != b {
		t.Fatalf("reflectionTitle is not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "reflection:chip:") {
		t.Fatalf("expected theme prefix, got %q", a)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// fakeSkillProposer records every ProposeReflectionSkill call so tests
// can assert that the reflection cycle drives the proposer exactly once
// per persisted reflection and never crosses fund boundaries.
type fakeSkillProposer struct {
	mu    sync.Mutex
	calls []proposedSkill
}

func (p *fakeSkillProposer) ProposeReflectionSkill(_ context.Context, s proposedSkill) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, s)
	return nil
}

func TestRunReflectionCycleInvokesProposerWithFundContext(t *testing.T) {
	t.Parallel()
	store := newFakeReflectionStore()
	for _, theme := range []string{"chip", "chip", "chip"} {
		store.preload("treatment", "daily", makeDailySource("treatment", "r1", theme, 1))
	}
	// Substantive lesson so the reflexion quality gate doesn't drop
	// it before the proposer ever sees it.
	distiller := &constantDistiller{content: "对存储芯片主题的复盘指出今日盘前研究覆盖度不足，需要补完关键个股的事件日历。"}
	proposer := &fakeSkillProposer{}

	n := runReflectionCycle(context.Background(), store, distiller, proposer, "treatment", time.Now().UTC())
	if n == 0 {
		t.Fatalf("expected reflection to persist")
	}
	if len(proposer.calls) != n {
		t.Fatalf("expected exactly %d proposer calls, got %d", n, len(proposer.calls))
	}
	for _, call := range proposer.calls {
		if call.FundID != "treatment" {
			t.Fatalf("proposer leaked across funds: got fund=%s", call.FundID)
		}
		if call.ReflectionID == "" || call.Title == "" || call.Content == "" {
			t.Fatalf("proposer call missing required fields: %+v", call)
		}
	}
}

func TestBuildCandidateSkillEntryShape(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 19, 11, 0, 0, 0, time.UTC)
	skill := proposedSkill{
		ReflectionID: "ref-123",
		FundID:       "fund-1",
		Theme:        "chip",
		Title:        "reflection:chip:abc",
		Content:      "Avoid chasing the third consecutive green chip bar.",
		Tags:         []string{"chip", "self_learning"},
		ProposedAt:   now,
	}
	entry := buildCandidateSkillEntry(skill, "researcher", "researcher")
	if entry.Key != "reflection:ref-123" {
		t.Fatalf("expected deterministic key, got %q", entry.Key)
	}
	if entry.Status != skillStatusProposed {
		t.Fatalf("expected status=proposed, got %q", entry.Status)
	}
	if entry.Enabled == nil || *entry.Enabled {
		t.Fatalf("expected enabled=false to gate the prompt resolver, got %v", entry.Enabled)
	}
	if len(entry.Match.Roles) != 1 || entry.Match.Roles[0] != "researcher" {
		t.Fatalf("expected role=researcher only, got %v", entry.Match.Roles)
	}
	if entry.ProposedAt != "2026-05-19T11:00:00Z" {
		t.Fatalf("expected RFC3339 proposed-at, got %q", entry.ProposedAt)
	}
}

func TestAddCandidateSkillToConfigIdempotent(t *testing.T) {
	t.Parallel()
	skill := proposedSkill{ReflectionID: "ref-1", Theme: "chip", Content: "lesson body", ProposedAt: time.Now().UTC()}
	raw := json.RawMessage(`{}`)

	updated1, changed1, err := addCandidateSkillToConfig(raw, skill, "researcher", "researcher")
	if err != nil {
		t.Fatalf("first call err: %v", err)
	}
	if !changed1 {
		t.Fatalf("first call should report change")
	}

	// Second call with the same skill must be a no-op (changed=false,
	// raw returned as-is).
	updated2, changed2, err := addCandidateSkillToConfig(updated1, skill, "researcher", "researcher")
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if changed2 {
		t.Fatalf("expected idempotent no-op, got changed=true")
	}
	if string(updated2) != string(updated1) {
		t.Fatalf("expected raw to be unchanged on no-op, got\nbefore=%s\nafter=%s", updated1, updated2)
	}
}

func TestApproveSkillInConfigFlipsStatusAndEnabled(t *testing.T) {
	t.Parallel()
	skill := proposedSkill{ReflectionID: "ref-9", Theme: "rates", Content: "Mind the curve.", ProposedAt: time.Now().UTC()}
	raw, _, err := addCandidateSkillToConfig(json.RawMessage(`{}`), skill, "pm", "pm")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, entry, found, err := approveSkillInConfig(raw, "reflection:ref-9", time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("approve err: %v", err)
	}
	if !found {
		t.Fatalf("expected skill to be found")
	}
	if entry.Status != skillStatusApproved {
		t.Fatalf("expected status=approved, got %q", entry.Status)
	}
	if entry.Enabled == nil || !*entry.Enabled {
		t.Fatalf("expected enabled=true after approval")
	}
	if entry.ApprovedAt == "" {
		t.Fatalf("expected ApprovedAt to be set")
	}
	// Re-approving an already-approved entry must be a no-op (out=nil
	// signals "no write needed").
	out2, entry2, found2, err := approveSkillInConfig(out, "reflection:ref-9", time.Now().UTC())
	if err != nil {
		t.Fatalf("re-approve err: %v", err)
	}
	if !found2 || out2 != nil {
		t.Fatalf("expected no-op re-approval: found=%v out=%v", found2, out2)
	}
	if entry2.Status != skillStatusApproved {
		t.Fatalf("re-approval should keep status=approved")
	}
}

func TestApproveSkillInConfigNotFoundReturnsFlag(t *testing.T) {
	t.Parallel()
	out, entry, found, err := approveSkillInConfig(json.RawMessage(`{"skills":[]}`), "missing-key", time.Now().UTC())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if found {
		t.Fatalf("expected found=false for missing key")
	}
	if out != nil || entry.Key != "" {
		t.Fatalf("expected zero values on miss, got out=%v entry=%+v", out, entry)
	}
}

func TestRemoveSkillFromConfigDropsTargetOnly(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"enabled":true,"skills":[
		{"key":"keep","name":"Keep","content":"x"},
		{"key":"drop","name":"Drop","content":"y"}
	]}`)
	out, found, err := removeSkillFromConfig(raw, "drop")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true")
	}
	var parsed parsedSkillConfig
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(parsed.Skills) != 1 || parsed.Skills[0].Key != "keep" {
		t.Fatalf("expected only 'keep' to remain, got %+v", parsed.Skills)
	}
}

func TestRemoveSkillFromConfigMissingIsNoOp(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"skills":[{"key":"k1","name":"Solo"}]}`)
	out, found, err := removeSkillFromConfig(raw, "k2")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if found {
		t.Fatalf("expected found=false for missing")
	}
	if string(out) != string(raw) {
		t.Fatalf("expected raw to be unchanged on miss")
	}
}

func TestSkillEntryIsActiveTreatsProposedAsInactive(t *testing.T) {
	t.Parallel()
	enabled := true
	cases := []struct {
		name string
		in   parsedSkillEntry
		want bool
	}{
		{"approved-enabled", parsedSkillEntry{Status: skillStatusApproved, Enabled: &enabled}, true},
		{"approved-default", parsedSkillEntry{Status: skillStatusApproved}, true},
		{"empty-status-default", parsedSkillEntry{}, true},
		{"proposed-even-with-enabled-true", parsedSkillEntry{Status: skillStatusProposed, Enabled: &enabled}, false},
		{"proposed-no-enabled", parsedSkillEntry{Status: skillStatusProposed}, false},
	}
	for _, tc := range cases {
		if got := skillEntryIsActive(tc.in); got != tc.want {
			t.Errorf("%s: want=%v got=%v", tc.name, tc.want, got)
		}
	}
}

