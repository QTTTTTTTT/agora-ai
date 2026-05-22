package main

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/llm"
	"github.com/fundai/server/internal/memory"
	"github.com/fundai/server/internal/repository"
)

// Reflection cadence + window defaults. They are intentionally generous to
// keep LLM cost predictable: a fund whose daily review already publishes
// granular per-agent learnings doesn't need to be re-distilled every day,
// and a 30-day window is enough to surface a recurring theme.
const (
	defaultReflectionCadenceDays   = 7
	defaultReflectionLookbackDays  = 30
	defaultReflectionMinGroupSize  = 3
	defaultReflectionMaxGroups     = 6
	defaultReflectionMaxItemsRunes = 800
	defaultReflectionMaxItems      = 12
	reflectionMemoryLayer          = "long_term"
)

// reflectionMemoryStore is the minimal slice of *repository.MemoryRepo that
// the reflection driver depends on. Defining it here lets unit tests inject
// a fake without standing up Postgres, while keeping the production code
// path identical (concrete *MemoryRepo satisfies the interface implicitly).
//
// The interface is intentionally NOT exported: it's an internal contract
// used only by maybeRunReflection + reflectionServiceAdapter and not part
// of the public surface.
type reflectionMemoryStore interface {
	ListByFund(ctx context.Context, fundID, layer string, limit int) ([]repository.Memory, error)
	Create(ctx context.Context, m *repository.Memory) (string, error)
}

// proposedSkill is the value object passed from the reflection driver to a
// skillProposer. It carries everything needed to construct an entry in an
// agent's SkillConfig but stops short of choosing the target agents — that
// fan-out is the proposer's responsibility (because it needs the team
// roster, which the reflection driver does not).
type proposedSkill struct {
	ReflectionID string
	FundID       string
	Theme        string
	Title        string
	Content      string
	Tags         []string
	ProposedAt   time.Time
}

// skillProposer turns a long-term reflection into per-agent candidate
// skills. The contract: for every team member of FundID, append a
// candidate parsedSkillEntry to that agent's SkillConfig.skills if one
// with the same key is not already present. Implementations must be
// idempotent — the reflection driver may call propose multiple times for
// the same reflection (e.g. after a crash recovery) and the resulting
// SkillConfig must converge.
type skillProposer interface {
	ProposeReflectionSkill(ctx context.Context, skill proposedSkill) error
}

// noopSkillProposer is used in tests that exercise the reflection cycle
// without caring about skill propagation. It satisfies skillProposer and
// silently swallows every call.
type noopSkillProposer struct{}

func (noopSkillProposer) ProposeReflectionSkill(_ context.Context, _ proposedSkill) error {
	return nil
}

// runtimeSkillProposer is the production implementation of skillProposer.
// On each call it:
//
//  1. Loads the fund's team members.
//  2. For every member, loads the agent record.
//  3. Appends a candidate parsedSkillEntry (status=proposed, enabled=false)
//     keyed by the reflection ID so a repeat call is a no-op (idempotent).
//  4. Writes the updated SkillConfig back via AgentRepo.Update.
//
// Failures on individual agents are logged and the loop continues — a
// per-agent permission/serialisation error must not stall the broadcast
// to the rest of the team.
type runtimeSkillProposer struct {
	teamRepo  *repository.TeamRepo
	agentRepo *repository.AgentRepo
}

func (p *runtimeSkillProposer) ProposeReflectionSkill(ctx context.Context, skill proposedSkill) error {
	if p == nil || p.teamRepo == nil || p.agentRepo == nil {
		return nil
	}
	members, err := p.teamRepo.ListByFund(ctx, skill.FundID)
	if err != nil {
		return fmt.Errorf("propose skill: list team: %w", err)
	}
	for _, m := range members {
		agent, err := p.agentRepo.GetByID(ctx, m.AgentID)
		if err != nil {
			slog.Warn("propose skill: load agent failed", "agent_id", m.AgentID, "fund_id", skill.FundID, "err", err)
			continue
		}
		updated, changed, err := addCandidateSkillToConfig(agent.SkillConfig, skill, agent.Role, m.Role)
		if err != nil {
			slog.Warn("propose skill: serialise failed", "agent_id", agent.ID, "err", err)
			continue
		}
		if !changed {
			continue
		}
		agent.SkillConfig = updated
		if err := p.agentRepo.Update(ctx, agent); err != nil {
			slog.Warn("propose skill: persist failed", "agent_id", agent.ID, "err", err)
			continue
		}
	}
	return nil
}

// maybeRunReflection promotes recent daily/agent learnings into long-term
// reflections via the memory.Reflect engine. It is invoked at the tail of
// ConsolidateDaily (so the workflow itself drives the cadence; no separate
// cron is required) and is internally rate-limited:
//
//  1. If the LLM runtime is unavailable, the function is a no-op so the
//     daily review never fails because of an optional learning enhancement.
//  2. If a reflection was already written for this fund in the last
//     defaultReflectionCadenceDays days, the function is a no-op too — the
//     intent is "weekly synthesis", not "every workflow run".
//  3. If the source memory window has fewer items than the minimum group
//     size, the function emits an info log and returns (saves token cost).
//
// All errors are logged + swallowed: a failing distillation must not block
// the workflow's "run_completed" event.
func (m *runtimeMemorySystem) maybeRunReflection(ctx context.Context, fundID string, fund *repository.Fund, tradingDate time.Time) {
	if m == nil || m.memoryRepo == nil {
		return
	}
	if m.llmRuntime == nil {
		// Without an LLM we cannot distil; this is expected in dev/test
		// environments and not a fatal condition.
		return
	}
	// The proposer is only wired when both team + agent repos are present
	// (Production always supplies both; some test paths skip them, in
	// which case we fall back to noop so the reflection still runs).
	var proposer skillProposer = noopSkillProposer{}
	if m.teamRepo != nil && m.agentRepo != nil {
		proposer = &runtimeSkillProposer{teamRepo: m.teamRepo, agentRepo: m.agentRepo}
	}
	runReflectionCycle(ctx, m.memoryRepo, newLLMReflectionDistiller(m.llmRuntime, fund, llm.TierStandard), proposer, fundID, tradingDate)
}

// runReflectionCycle is the testable core of maybeRunReflection. It is
// fully decoupled from llm/Postgres: the caller supplies a Distiller, a
// reflectionMemoryStore, and a skillProposer so unit tests can verify
// cadence, dedupe, per-fund isolation, and candidate-skill broadcasting
// deterministically.
//
// Per-fund isolation is enforced by construction: every store call is
// parameterised by the input fundID, and every persisted row carries
// FundID=fundID. There is no code path through which a reflection for fund
// A can be written to fund B's row, even if the upstream daily-review loop
// is buggy. F3.3 lock-in tests pin this guarantee.
//
// Candidate skill propagation (F4.2) runs once per newly-persisted
// reflection. A nil proposer means "don't propagate" — useful for tests
// that just want to exercise the persistence path.
func runReflectionCycle(ctx context.Context, store reflectionMemoryStore, distiller memory.Distiller, proposer skillProposer, fundID string, tradingDate time.Time) int {
	if store == nil || distiller == nil {
		return 0
	}
	now := tradingDate
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cadence := time.Duration(defaultReflectionCadenceDays) * 24 * time.Hour
	if recent, err := store.ListByFund(ctx, fundID, reflectionMemoryLayer, 1); err == nil && len(recent) > 0 {
		if now.Sub(recent[0].CreatedAt) < cadence {
			return 0
		}
	}

	since := now.AddDate(0, 0, -defaultReflectionLookbackDays)
	source, err := collectReflectionSource(ctx, store, fundID, since)
	if err != nil {
		slog.Warn("memory reflection: source collection failed", "fund_id", fundID, "err", err)
		return 0
	}
	if len(source) < defaultReflectionMinGroupSize {
		return 0
	}

	reflections, err := memory.Reflect(ctx, source, distiller, memory.ReflectParams{
		FundID:              fundID,
		Now:                 now,
		MinGroupSize:        defaultReflectionMinGroupSize,
		MaxGroups:           defaultReflectionMaxGroups,
		MaxItemsPerGroup:    defaultReflectionMaxItems,
		MaxItemContentRunes: defaultReflectionMaxItemsRunes,
	})
	if errors.Is(err, memory.ErrNothingToReflect) {
		return 0
	}
	if err != nil {
		slog.Warn("memory reflection: engine error", "fund_id", fundID, "err", err)
		return 0
	}

	persisted := 0
	existing, _ := store.ListByFund(ctx, fundID, reflectionMemoryLayer, 50)
	existingTitles := make(map[string]struct{}, len(existing))
	for _, row := range existing {
		if row.Title.Valid {
			existingTitles[row.Title.String] = struct{}{}
		}
	}
	for _, item := range reflections {
		// Skip duplicates: reflections are content-addressed by SHA-1 of
		// (theme + content); if the same lesson is already on file we
		// don't re-insert. This is best-effort — concurrent writers may
		// still race but a duplicate is a cosmetic issue at worst.
		title := reflectionTitle(item)
		if _, dup := existingTitles[title]; dup {
			continue
		}
		id, err := store.Create(ctx, &repository.Memory{
			FundID:      fundID,
			Layer:       reflectionMemoryLayer,
			Visibility:  "fund",
			Sensitivity: "internal",
			OriginKind:  "native",
			Title:       nullString(title),
			Content:     item.Content,
			TradingDate: nullTimeFromReflection(now),
			Tags:        item.Tags,
		})
		if err != nil {
			slog.Warn("memory reflection: persist failed", "fund_id", fundID, "title", title, "err", err)
			continue
		}
		existingTitles[title] = struct{}{}
		persisted++

		// Broadcast as a candidate skill to the fund's team. Failures
		// here are logged but never abort the reflection cycle — the
		// reflection itself is already on disk and the user can still
		// see it via the read endpoint.
		if proposer != nil {
			theme := extractReflectionTheme(title, item.Tags)
			if err := proposer.ProposeReflectionSkill(ctx, proposedSkill{
				ReflectionID: id,
				FundID:       fundID,
				Theme:        theme,
				Title:        title,
				Content:      item.Content,
				Tags:         item.Tags,
				ProposedAt:   now,
			}); err != nil {
				slog.Warn("memory reflection: skill propose failed", "fund_id", fundID, "title", title, "err", err)
			}
		}
	}
	if persisted > 0 {
		slog.Info("memory reflection: persisted long-term lessons", "fund_id", fundID, "count", persisted)
	}
	return persisted
}

// collectReflectionSource pulls recent `daily` + `agent` layer memories
// (those are the rows ConsolidateDaily writes) and projects them onto
// memory.Item. Items older than `since` are dropped client-side because the
// repo doesn't expose a since/until window for layer queries.
func collectReflectionSource(ctx context.Context, store reflectionMemoryStore, fundID string, since time.Time) ([]memory.Item, error) {
	layers := []string{"daily", "agent"}
	const perLayerLimit = 60
	var items []memory.Item
	for _, layer := range layers {
		rows, err := store.ListByFund(ctx, fundID, layer, perLayerLimit)
		if err != nil {
			return nil, fmt.Errorf("list memories layer=%s: %w", layer, err)
		}
		for _, row := range rows {
			if row.CreatedAt.Before(since) {
				continue
			}
			items = append(items, memory.Item{
				ID:        row.ID,
				FundID:    row.FundID,
				AgentID:   row.AgentID.String,
				Layer:     row.Layer,
				Kind:      "lesson",
				Title:     row.Title.String,
				Content:   row.Content,
				Tags:      row.Tags,
				CreatedAt: row.CreatedAt,
			})
		}
	}
	return items, nil
}

// reflectionTitle builds a deterministic title from the reflection's theme
// + content prefix. The hash suffix avoids collisions when an LLM picks the
// same opening line on two themes.
func reflectionTitle(item memory.Item) string {
	theme := "general"
	if strings.HasPrefix(strings.ToLower(item.Title), "reflection: ") {
		theme = strings.TrimPrefix(strings.ToLower(item.Title), "reflection: ")
	} else if len(item.Tags) > 0 {
		theme = strings.ToLower(strings.TrimSpace(item.Tags[0]))
	}
	digestInput := theme + "|" + strings.TrimSpace(item.Content)
	sum := sha1.Sum([]byte(digestInput))
	return fmt.Sprintf("reflection:%s:%s", theme, hex.EncodeToString(sum[:6]))
}

// nullTimeFromReflection mirrors nullString for time.Time fields. Named with
// a suffix to avoid clashing with potential future helpers in this package.
func nullTimeFromReflection(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: !t.IsZero()}
}

// llmReflectionDistiller is a memory.Distiller backed by the platform LLM
// runtime. It is intentionally minimal: the prompt asks for a single 1-2
// sentence lesson per theme so the LLM bill stays bounded and the resulting
// reflection is dense enough to be useful as a long-term recall hit.
type llmReflectionDistiller struct {
	runtime *llmRuntime
	fund    *repository.Fund
	tier    llm.ModelTier
}

func newLLMReflectionDistiller(runtime *llmRuntime, fund *repository.Fund, tier llm.ModelTier) *llmReflectionDistiller {
	return &llmReflectionDistiller{runtime: runtime, fund: fund, tier: tier}
}

func (d *llmReflectionDistiller) Distill(ctx context.Context, theme string, items []memory.Item) (string, error) {
	if d == nil || d.runtime == nil {
		return "", errors.New("llm reflection distiller: nil runtime")
	}
	// Build the prompt deterministically: sort items by recency desc so the
	// LLM sees the most recent context first; this matters when the model
	// context is small and the prompt gets truncated upstream.
	sorted := append([]memory.Item(nil), items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.After(sorted[j].CreatedAt)
	})

	var sb strings.Builder
	sb.WriteString("Theme: ")
	sb.WriteString(theme)
	sb.WriteString("\n\nDaily/agent memories from the last 30 days:\n\n")
	for i, item := range sorted {
		sb.WriteString(fmt.Sprintf("[%d] %s — %s\n", i+1, item.CreatedAt.UTC().Format("2006-01-02"), strings.TrimSpace(item.Content)))
	}

	req := llm.ChatRequest{
		ModelTier: d.tier,
		Messages: []llm.ChatMessage{
			{
				Role: "system",
				Content: `You are the long-term memory consolidator for an investment fund's AI agents. ` +
					`Given a theme and a batch of recent daily/agent learnings, output a single 1-2 sentence ` +
					`lesson in English that captures the durable insight. Be concrete (mention symbols, ` +
					`indicators, or trade patterns when relevant) and avoid hedging language. Do NOT echo the ` +
					`input bullets verbatim. If the inputs contradict each other, surface the contradiction ` +
					`explicitly so the PM agent can resolve it next session.`,
			},
			{Role: "user", Content: sb.String()},
		},
		MaxTokens:   220,
		Temperature: 0.3,
		StepName:    "memory_reflection",
	}
	if d.fund != nil {
		req.FundID = d.fund.ID
	}
	resp, err := d.runtime.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", errors.New("llm reflection distiller: empty response")
	}
	return strings.TrimSpace(resp.Content), nil
}

// agentSkillServiceAdapter implements api.AgentSkillService. It treats
// every agent's SkillConfig JSON as the authoritative skill library and
// projects it onto api.AgentSkillEntry for the management UI.
//
// Authorisation: skill mutations require the caller to own the agent
// record (authorizeAgentAccess wraps the FundCompany ACL used elsewhere).
type agentSkillServiceAdapter struct {
	db        *sql.DB
	agentRepo *repository.AgentRepo
}

func newAgentSkillServiceAdapter(db *sql.DB) *agentSkillServiceAdapter {
	return &agentSkillServiceAdapter{
		db:        db,
		agentRepo: repository.NewAgentRepo(db),
	}
}

// authorize mirrors teamServiceAdapter's ownership check used by the
// learning endpoints: GetOwnedByID returns ErrNotFound when the caller is
// not the agent's owner, which the API handler maps to a 404. This avoids
// leaking the existence of agents owned by other users. The user ID is
// already guaranteed non-empty by requireAuthenticatedUserID upstream.
func (s *agentSkillServiceAdapter) authorize(ctx context.Context, userID, agentID string) (*repository.Agent, error) {
	agent, err := s.agentRepo.GetOwnedByID(ctx, strings.TrimSpace(userID), strings.TrimSpace(agentID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	return agent, nil
}

func (s *agentSkillServiceAdapter) ListSkills(userID, agentID string) (*api.AgentSkillList, error) {
	ctx := context.Background()
	agent, err := s.authorize(ctx, userID, agentID)
	if err != nil {
		return nil, err
	}
	config := parseSkillConfig(agent.SkillConfig)
	out := &api.AgentSkillList{AgentID: agent.ID, Skills: make([]api.AgentSkillEntry, 0, len(config.Skills))}
	for _, skill := range config.Skills {
		out.Skills = append(out.Skills, agentSkillEntryFromParsed(skill))
	}
	return out, nil
}

func (s *agentSkillServiceAdapter) ApproveSkill(userID, agentID, skillKey string) (*api.AgentSkillEntry, error) {
	ctx := context.Background()
	agent, err := s.authorize(ctx, userID, agentID)
	if err != nil {
		return nil, err
	}
	updatedRaw, updatedEntry, found, err := approveSkillInConfig(agent.SkillConfig, skillKey, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, api.ErrNotFound
	}
	if updatedRaw != nil {
		agent.SkillConfig = updatedRaw
		if err := s.agentRepo.Update(ctx, agent); err != nil {
			return nil, mapRepositoryError(err)
		}
	}
	entry := agentSkillEntryFromParsed(updatedEntry)
	return &entry, nil
}

func (s *agentSkillServiceAdapter) RejectSkill(userID, agentID, skillKey string) error {
	ctx := context.Background()
	agent, err := s.authorize(ctx, userID, agentID)
	if err != nil {
		return err
	}
	updatedRaw, found, err := removeSkillFromConfig(agent.SkillConfig, skillKey)
	if err != nil {
		return err
	}
	if !found {
		return api.ErrNotFound
	}
	agent.SkillConfig = updatedRaw
	if err := s.agentRepo.Update(ctx, agent); err != nil {
		return mapRepositoryError(err)
	}
	return nil
}

// agentSkillEntryFromParsed projects the internal parsedSkillEntry onto
// the JSON-shaped api.AgentSkillEntry. Status defaults to "approved" when
// missing so the legacy data (no Status field) renders as expected.
func agentSkillEntryFromParsed(skill parsedSkillEntry) api.AgentSkillEntry {
	status := strings.TrimSpace(skill.Status)
	if status == "" {
		status = skillStatusApproved
	}
	enabled := skillEntryEnabled(skill)
	return api.AgentSkillEntry{
		Key:         skill.Key,
		Name:        skill.Name,
		Description: skill.Description,
		Content:     skill.Content,
		Status:      status,
		Source:      skill.Source,
		Enabled:     enabled,
		Priority:    skill.Priority,
		Roles:       append([]string(nil), skill.Match.Roles...),
		Focuses:     append([]string(nil), skill.Match.Focuses...),
		ProposedAt:  skill.ProposedAt,
		ApprovedAt:  skill.ApprovedAt,
	}
}

// approveSkillInConfig flips the skill with the supplied key to
// status=approved + enabled=true. Returns the updated raw config, the
// updated entry, a found flag, and an error. If the skill is already
// approved the function still returns the entry but updatedRaw is nil so
// callers can skip the write.
func approveSkillInConfig(raw json.RawMessage, skillKey string, now time.Time) (json.RawMessage, parsedSkillEntry, bool, error) {
	config := parsedSkillConfig{Enabled: true}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(raw, &config); err != nil {
			return nil, parsedSkillEntry{}, false, fmt.Errorf("parse skill config: %w", err)
		}
	}
	for i := range config.Skills {
		if config.Skills[i].Key != skillKey {
			continue
		}
		entry := config.Skills[i]
		alreadyApproved := !strings.EqualFold(entry.Status, skillStatusProposed) && skillEntryEnabled(entry)
		if alreadyApproved {
			return nil, entry, true, nil
		}
		enabled := true
		entry.Status = skillStatusApproved
		entry.Enabled = &enabled
		entry.ApprovedAt = now.UTC().Format(time.RFC3339)
		config.Skills[i] = entry
		out, err := json.Marshal(config)
		if err != nil {
			return nil, parsedSkillEntry{}, true, fmt.Errorf("marshal skill config: %w", err)
		}
		return out, entry, true, nil
	}
	return nil, parsedSkillEntry{}, false, nil
}

// removeSkillFromConfig drops the entry whose Key matches. Returns the
// updated raw config + a found flag.
func removeSkillFromConfig(raw json.RawMessage, skillKey string) (json.RawMessage, bool, error) {
	config := parsedSkillConfig{Enabled: true}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(raw, &config); err != nil {
			return nil, false, fmt.Errorf("parse skill config: %w", err)
		}
	}
	filtered := make([]parsedSkillEntry, 0, len(config.Skills))
	found := false
	for _, skill := range config.Skills {
		if skill.Key == skillKey {
			found = true
			continue
		}
		filtered = append(filtered, skill)
	}
	if !found {
		return raw, false, nil
	}
	config.Skills = filtered
	out, err := json.Marshal(config)
	if err != nil {
		return nil, true, fmt.Errorf("marshal skill config: %w", err)
	}
	return out, true, nil
}

// reflectionServiceAdapter implements api.ReflectionService. It is a thin
// read-only projection over the memories table: long-term reflections are
// stored alongside other memory rows, so this adapter pre-filters by layer
// + parses the theme out of the deterministic title we wrote in
// maybeRunReflection. Authorisation reuses the same fund-access guard used
// by the existing memory endpoints to keep the rules consistent.
type reflectionServiceAdapter struct {
	db          *sql.DB
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	memoryRepo  *repository.MemoryRepo
}

func newReflectionServiceAdapter(db *sql.DB) *reflectionServiceAdapter {
	return &reflectionServiceAdapter{
		db:          db,
		fundRepo:    repository.NewFundRepo(db),
		companyRepo: repository.NewFundCompanyRepo(db),
		memoryRepo:  repository.NewMemoryRepo(db),
	}
}

// ListReflections returns the most recent long-term reflections for the
// fund, oldest-first so the frontend can render them as an upward-flowing
// timeline. Limit is clamped by the caller; we additionally enforce a
// floor of 1 so a sloppy 0/negative input doesn't return everything.
func (s *reflectionServiceAdapter) ListReflections(userID, fundID string, limit int) (*api.ReflectionList, error) {
	ctx := context.Background()
	fund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, fundID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.memoryRepo.ListByFund(ctx, fund.ID, reflectionMemoryLayer, limit)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	out := &api.ReflectionList{
		FundID:      fund.ID,
		GeneratedAt: time.Now().UTC(),
		Items:       make([]api.ReflectionItem, 0, len(rows)),
	}
	// ListByFund returns newest-first; render the timeline oldest-first so
	// the most recent reflection is at the bottom (matching a typical
	// activity log).
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		item := api.ReflectionItem{
			ID:        row.ID,
			FundID:    row.FundID,
			Title:     row.Title.String,
			Content:   row.Content,
			Tags:      row.Tags,
			Theme:     extractReflectionTheme(row.Title.String, row.Tags),
			CreatedAt: row.CreatedAt,
		}
		if row.TradingDate.Valid {
			item.TradingDate = row.TradingDate.Time.Format("2006-01-02")
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// candidateSkillKey is the deterministic key used to dedupe proposed
// skills derived from the same reflection. Mirrors reflectionTitle's
// content-addressed style so a retry after a crash converges instead of
// proliferating duplicate proposals.
func candidateSkillKey(skill proposedSkill) string {
	if strings.TrimSpace(skill.ReflectionID) != "" {
		return "reflection:" + skill.ReflectionID
	}
	// Fallback for tests that don't carry an ID: hash the title so the
	// key still de-duplicates across multiple calls with the same input.
	sum := sha1.Sum([]byte(skill.Title + "|" + skill.Content))
	return "reflection:title:" + hex.EncodeToString(sum[:6])
}

// buildCandidateSkillEntry projects a proposedSkill onto the inline
// parsedSkillEntry shape that the agent's SkillConfig already understands.
// The candidate is constructed with status="proposed" and enabled=false so
// the prompt resolver refuses to surface it until a human explicitly
// approves. The Match field is narrowed to the agent's own role so even
// an accidentally-approved skill cannot bleed into another agent's prompts.
func buildCandidateSkillEntry(skill proposedSkill, agentRole, memberRole string) parsedSkillEntry {
	disabled := false
	desc := firstSentence(skill.Content, 160)
	roles := dedupeNonEmptyLower([]string{agentRole, memberRole})
	tags := dedupeNonEmptyLower(skill.Tags)
	return parsedSkillEntry{
		Key:         candidateSkillKey(skill),
		Name:        "Reflection: " + skill.Theme,
		Description: desc,
		Content:     strings.TrimSpace(skill.Content),
		Enabled:     &disabled,
		Priority:    0,
		Status:      skillStatusProposed,
		Source:      "reflection:" + skill.ReflectionID,
		ProposedAt:  skill.ProposedAt.UTC().Format(time.RFC3339),
		Match: parsedSkillMatch{
			Roles:            roles,
			ScenarioKeywords: tags,
		},
	}
}

// addCandidateSkillToConfig appends a candidate skill to the raw
// SkillConfig JSON unless an entry with the same Key already exists.
// Returns the updated raw JSON, a changed flag, and an error.
//
// The function is conservative on parse failures: if json.Unmarshal can't
// decode the incoming raw, we propagate the error rather than silently
// reset the agent's skill list. Existing legacy semantics treat an empty
// or "null" raw as "no skills yet"; we preserve that.
func addCandidateSkillToConfig(raw json.RawMessage, skill proposedSkill, agentRole, memberRole string) (json.RawMessage, bool, error) {
	config := parsedSkillConfig{Enabled: true}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(raw, &config); err != nil {
			return nil, false, fmt.Errorf("parse skill config: %w", err)
		}
	}
	candidate := buildCandidateSkillEntry(skill, agentRole, memberRole)
	for _, existing := range config.Skills {
		if existing.Key == candidate.Key {
			return raw, false, nil
		}
	}
	config.Skills = append(config.Skills, candidate)
	out, err := json.Marshal(config)
	if err != nil {
		return nil, false, fmt.Errorf("marshal skill config: %w", err)
	}
	return out, true, nil
}

// dedupeNonEmptyLower returns the input strings lowercased, trimmed, and
// deduplicated. Empty entries are dropped. Order is preserved (first
// occurrence wins) so the resulting Match.Roles list is stable enough to
// be JSON-diffed in tests.
func dedupeNonEmptyLower(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	out := make([]string, 0, len(input))
	for _, v := range input {
		clean := strings.ToLower(strings.TrimSpace(v))
		if clean == "" {
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

// firstSentence returns at most `maxRunes` runes from the first sentence
// of `s`. We use it to build a short description shown next to the skill
// name in the UI. Sentence boundary detection is intentionally simple:
// the first period/newline wins.
func firstSentence(s string, maxRunes int) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	if idx := strings.IndexAny(trimmed, ".\n"); idx >= 0 {
		trimmed = strings.TrimSpace(trimmed[:idx])
	}
	if maxRunes <= 0 {
		return trimmed
	}
	runes := []rune(trimmed)
	if len(runes) <= maxRunes {
		return trimmed
	}
	return string(runes[:maxRunes]) + "…"
}

// extractReflectionTheme pulls the theme segment out of a reflection title
// of the form "reflection:<theme>:<hash>". Falls back to the first tag or
// the literal "general" so the UI always has something to render.
func extractReflectionTheme(title string, tags []string) string {
	trimmed := strings.TrimSpace(title)
	if strings.HasPrefix(trimmed, "reflection:") {
		parts := strings.SplitN(strings.TrimPrefix(trimmed, "reflection:"), ":", 2)
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			return parts[0]
		}
	}
	for _, tag := range tags {
		clean := strings.ToLower(strings.TrimSpace(tag))
		if clean != "" && clean != "self_learning" && clean != "market-research" {
			return clean
		}
	}
	return "general"
}
