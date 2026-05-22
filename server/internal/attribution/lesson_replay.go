package attribution

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Lesson replay (PR-3A10)
// ---------------------------------------------------------------------------
//
// PromptScorecard already gives the LLM PM a structured, numeric
// view of "which (sleeve, regime) cells have made or lost money".
// That's the right channel for fresh aggregate stats but it
// loses the textual reasoning the lesson generator already wrote
// — sentences like "Across 12 closed lots in regime range, the
// mean_reversion sleeve recorded a 22% win rate ... consider
// pausing this combination". Those sentences are persisted in
// the memories table (layer="attribution") and shown on the
// AgentLearning dashboard, but the LLM never sees them.
//
// BuildLessonReplay closes that gap. It walks a slice of recent
// attribution memories, picks the most-actionable subset, and
// renders them into a prompt-ready Markdown block the LLM can
// scan. The system prompt (decision/prompt.go) instructs the
// model how to honour the block — see Rule 4.x there.
//
// Design rules:
//   - Read-only. We never re-derive lessons here; we just choose
//     which already-written ones to forward.
//   - Compact. The LLM has a finite attention budget; capping
//     MaxLessons + truncating body text keeps the inserted
//     section under ~2KB even on noisy funds.
//   - Dedup. If the daily review ran twice with overlapping
//     output, the same (sleeve, regime, kind) lesson can land
//     in memories more than once. We keep only the freshest
//     copy so the prompt doesn't repeat itself.
//   - Severity-first. critical > warning > info ordering so the
//     model reads the loudest signals first; ties broken by
//     recency so today's lesson outranks last week's.
//   - Deterministic. Same input ↦ same output. Tests pin the
//     order + truncation contract.

// LessonReplay is the LLM-facing wrapper. Summary is what we
// paste into the prompt; Rows is the structured equivalent kept
// around for tests + future dashboard "show what the LLM saw"
// debugging surface.
type LessonReplay struct {
	// Window is the time horizon used to pick lessons, e.g.
	// "last 14 days". Empty when no rows survived the filter.
	Window string

	// Rows are the kept lessons, in the order they appear in
	// Summary (severity DESC, then recency DESC). Each Body is
	// already truncated to MaxBodyChars.
	Rows []LessonReplayRow

	// Summary is a multi-line, prompt-ready rendering of Rows.
	// Empty when Rows is empty. Wiring should test for
	// Summary != "" before pasting into the user prompt.
	Summary string
}

// LessonReplayRow is one prompt-bound lesson. We DROP the
// numeric fields (TradeCount / WinRate / TotalPnL / …) because
// PromptScorecard already covers those; replay is the textual
// channel that complements it.
type LessonReplayRow struct {
	Kind      string    // "sleeve_regime_loser" / "winner" / "insufficient_data"
	Severity  string    // "critical" / "warning" / "info"
	Sleeve    string    // optional context tags pulled out of memory.tags
	Regime    string
	Title     string
	Body      string    // truncated to MaxBodyChars
	CreatedAt time.Time
}

// LessonReplayOptions tunes BuildLessonReplay. Zero values fall
// back to defaults via effective().
type LessonReplayOptions struct {
	// MaxLessons caps the prompt-bound subset. Default: 5.
	// Below 5 the model misses noteworthy patterns; above ~8 the
	// inserted block starts to push useful context (positions,
	// quant signals) out of the attention window.
	MaxLessons int
	// MaxBodyChars truncates each lesson body. Default: 480.
	// Long bodies waste tokens and rarely add information past
	// the first sentence — the generator templates are already
	// terse. We cut at the last full sentence boundary before
	// the limit when one exists.
	MaxBodyChars int
	// IncludeInsufficientData decides whether the "no data yet"
	// lesson kind survives the filter. Default: false. The LLM
	// doesn't need to know attribution is still warming up — it
	// can just rely on Scorecard being empty.
	IncludeInsufficientData bool
	// LookbackDays bounds how old a lesson can be and still
	// appear. Default: 14 days. Older lessons may reference
	// regimes / sleeve params that have since been retuned;
	// keeping the window short stops stale "trend × chop is a
	// loser" hints from haunting today's prompts after the
	// operator already pulled trend out of the chop regime.
	LookbackDays int
}

func (o LessonReplayOptions) effective() LessonReplayOptions {
	out := o
	if out.MaxLessons <= 0 {
		out.MaxLessons = 5
	}
	if out.MaxBodyChars <= 0 {
		out.MaxBodyChars = 480
	}
	if out.LookbackDays <= 0 {
		out.LookbackDays = 14
	}
	return out
}

// BuildLessonReplay distils a slice of attribution-layer memories
// into a prompt-ready replay block.
//
// Contract:
//   - `now` is the reference timestamp for the lookback filter.
//     Pass time.Now() in production; tests pin it for stability.
//   - Input memories MUST already be filtered to layer="attribution".
//     We don't re-filter — the caller's repository layer is the
//     single source of truth for what "an attribution memory"
//     means.
//   - Returns the zero value when nothing survives. Caller
//     should test Summary != "" before pasting into the prompt.
func BuildLessonReplay(memories []repository.Memory, now time.Time, opts LessonReplayOptions) LessonReplay {
	o := opts.effective()
	if len(memories) == 0 {
		return LessonReplay{}
	}
	cutoff := now.Add(-time.Duration(o.LookbackDays) * 24 * time.Hour)

	// Walk through memories collecting candidates. A single
	// (kind, sleeve, regime) tuple is collapsed to the freshest
	// row — daily reruns of the lesson generator legitimately
	// regenerate the same lesson when the underlying stats
	// haven't moved, and we only want one copy in the prompt.
	type bucket struct {
		row LessonReplayRow
	}
	keyed := make(map[string]bucket, len(memories))
	for _, m := range memories {
		row, ok := memoryToReplayRow(m)
		if !ok {
			continue
		}
		if row.CreatedAt.Before(cutoff) {
			continue
		}
		if !o.IncludeInsufficientData && row.Kind == string(LessonInsufficientData) {
			continue
		}
		key := replayKey(row)
		if existing, dup := keyed[key]; dup {
			if !row.CreatedAt.After(existing.row.CreatedAt) {
				continue
			}
		}
		// Truncate body once we know we're keeping the row.
		row.Body = truncateBody(row.Body, o.MaxBodyChars)
		keyed[key] = bucket{row: row}
	}
	if len(keyed) == 0 {
		return LessonReplay{}
	}

	rows := make([]LessonReplayRow, 0, len(keyed))
	for _, b := range keyed {
		rows = append(rows, b.row)
	}
	// Sort: severity DESC (critical first), then recency DESC.
	// Ties broken deterministically by sleeve+regime so two
	// equally-severe lessons keep the same order across runs.
	sort.SliceStable(rows, func(i, j int) bool {
		si := severityRank(rows[i].Severity)
		sj := severityRank(rows[j].Severity)
		if si != sj {
			return si > sj
		}
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		}
		if rows[i].Sleeve != rows[j].Sleeve {
			return rows[i].Sleeve < rows[j].Sleeve
		}
		return rows[i].Regime < rows[j].Regime
	})
	if len(rows) > o.MaxLessons {
		rows = rows[:o.MaxLessons]
	}

	return LessonReplay{
		Window:  fmt.Sprintf("last %d days", o.LookbackDays),
		Rows:    rows,
		Summary: renderReplaySummary(o.LookbackDays, rows),
	}
}

// memoryToReplayRow projects a repository.Memory onto a
// LessonReplayRow. Returns (zero, false) when the row isn't a
// real lesson — most commonly an entry that wasn't tagged with
// a recognisable kind/sleeve/regime trio, which happens for
// legacy attribution memories written before PR-3A5.
func memoryToReplayRow(m repository.Memory) (LessonReplayRow, bool) {
	kind := classifyMemoryKind(m.Tags)
	if kind == "" {
		// Best-effort heuristic: if the title carries
		// "Sleeve ... regime" the row IS a lesson but the
		// tag set was malformed. Fall back to "loser" so it
		// at least surfaces — better than silently dropping.
		if strings.Contains(m.Title.String, "regime ") && strings.Contains(strings.ToLower(m.Title.String), "losing") {
			kind = string(LessonSleeveRegimeLoser)
		} else if strings.Contains(strings.ToLower(m.Title.String), "no closed trades") {
			kind = string(LessonInsufficientData)
		}
	}
	if kind == "" {
		return LessonReplayRow{}, false
	}
	severity := classifyMemorySeverity(kind)
	sleeve, regime := extractSleeveRegimeTags(m.Tags)
	return LessonReplayRow{
		Kind:      kind,
		Severity:  severity,
		Sleeve:    sleeve,
		Regime:    regime,
		Title:     strings.TrimSpace(m.Title.String),
		Body:     strings.TrimSpace(m.Content),
		CreatedAt: m.CreatedAt,
	}, true
}

// classifyMemoryKind looks at the tag set for a lesson kind
// marker. The attribution lesson generator writes "loser",
// "winner", "insufficient_data" as standalone tags (see
// lesson.go). Returns "" when none match.
func classifyMemoryKind(tags []string) string {
	for _, t := range tags {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "loser":
			return string(LessonSleeveRegimeLoser)
		case "winner":
			return string(LessonSleeveRegimeWinner)
		case "insufficient_data":
			return string(LessonInsufficientData)
		}
	}
	return ""
}

// classifyMemorySeverity maps a lesson kind back onto a severity
// label, mirroring the levels the generator actually writes.
// We don't read severity out of the memory row directly because
// the legacy memories layer doesn't store it; the canonical
// mapping lives inside the lesson generator.
func classifyMemorySeverity(kind string) string {
	switch kind {
	case string(LessonSleeveRegimeLoser):
		return string(SeverityCritical)
	case string(LessonSleeveRegimeWinner):
		return string(SeverityInfo)
	case string(LessonInsufficientData):
		return string(SeverityInfo)
	default:
		return string(SeverityInfo)
	}
}

// extractSleeveRegimeTags pulls the canonical "sleeve:X" /
// "regime:Y" tags written by the lesson generator. Returns
// empties when one or both are missing — that's the legacy /
// insufficient_data shape, which carries no sleeve at all.
func extractSleeveRegimeTags(tags []string) (string, string) {
	var sleeve, regime string
	for _, t := range tags {
		trim := strings.TrimSpace(t)
		switch {
		case strings.HasPrefix(trim, "sleeve:"):
			sleeve = strings.TrimSpace(strings.TrimPrefix(trim, "sleeve:"))
		case strings.HasPrefix(trim, "regime:"):
			regime = strings.TrimSpace(strings.TrimPrefix(trim, "regime:"))
		}
	}
	return sleeve, regime
}

// replayKey produces a deterministic dedup key per (kind,
// sleeve, regime). The insufficient_data lesson has no
// sleeve/regime so we fold all instances under one key.
func replayKey(row LessonReplayRow) string {
	if row.Kind == string(LessonInsufficientData) {
		return row.Kind
	}
	return row.Kind + "|" + strings.ToLower(row.Sleeve) + "|" + strings.ToLower(row.Regime)
}

// severityRank turns the severity label into a comparable int.
// critical(3) > warning(2) > info(1) > unknown(0).
func severityRank(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case string(SeverityCritical):
		return 3
	case string(SeverityWarning):
		return 2
	case string(SeverityInfo):
		return 1
	}
	return 0
}

// truncateBody clips a body string at the last sentence boundary
// that fits inside maxChars. When no period sits within the
// window we fall back to a hard cut + ellipsis. The aim is to
// keep the LLM-readable text grammatically clean rather than
// chopping mid-word.
func truncateBody(body string, maxChars int) string {
	if maxChars <= 0 || len(body) <= maxChars {
		return body
	}
	cut := body[:maxChars]
	// Find the last period (.) or full-stop equivalent inside
	// the cut window. We deliberately accept "?" and "!" too so
	// non-English summaries don't get garbled.
	for i := len(cut) - 1; i >= 0; i-- {
		ch := cut[i]
		if ch == '.' || ch == '?' || ch == '!' || ch == '\n' {
			return strings.TrimSpace(cut[:i+1])
		}
	}
	return strings.TrimSpace(cut) + "…"
}

// renderReplaySummary turns the kept rows into the prompt-ready
// Markdown block. Format is part of the contract with the system
// prompt (see decision/prompt.go Rule 4.x). Changes here must
// be matched there.
func renderReplaySummary(windowDays int, rows []LessonReplayRow) string {
	if len(rows) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Recent attribution lessons (last %d days, most-severe first):\n", windowDays)
	for _, r := range rows {
		header := strings.ToUpper(r.Severity)
		if r.Sleeve != "" || r.Regime != "" {
			header += fmt.Sprintf(" [%s × %s]", coalesce(r.Sleeve, "(any)"), coalesce(r.Regime, "(any)"))
		}
		fmt.Fprintf(&sb, "  - %s %s\n", header, strings.TrimSpace(r.Title))
		if r.Body != "" {
			fmt.Fprintf(&sb, "      %s\n", strings.ReplaceAll(strings.TrimSpace(r.Body), "\n", " "))
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func coalesce(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
