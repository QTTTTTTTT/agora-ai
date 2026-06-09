package main

import (
	"context"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/compliance"
	"github.com/fundai/server/internal/repository"
)

// complianceAdapter implements api.ComplianceService against the
// PostgreSQL-backed repository.ComplianceRepo and the in-process
// compliance.Mode setting.
//
// The adapter owns:
//
//   - the current Mode (Publisher / RIA), parsed from env once at
//     boot. Changes require a restart — that's intentional;
//     hot-swapping compliance posture mid-process would create
//     an audit gap where some users in flight see one disclosure
//     and others see another.
//
//   - per-surface text-version constants so we can bump them when
//     legal review changes the wording. Bumping the version
//     forces every user to re-acknowledge (the HasAcknowledged
//     repo query requires text_version >= current).
//
// The lookup-on-every-request DB hits are fine — the
// compliance_acknowledgments table is tiny (one row per
// user × surface). For the rare case where the DB is briefly
// unavailable, the handlers degrade-gracefully (see
// api/compliance_handler.go).
type complianceAdapter struct {
	repo *repository.ComplianceRepo
	mode compliance.Mode
	// textVersions[surface] returns the legal-team-approved
	// "current version" of the disclosure for that surface. Used
	// both when persisting (so we record which version the user
	// saw) and when gating (HasAcknowledged demands >= current).
	textVersions map[compliance.Surface]int
}

func newComplianceAdapter(repo *repository.ComplianceRepo, mode compliance.Mode) *complianceAdapter {
	return &complianceAdapter{
		repo: repo,
		mode: mode,
		textVersions: map[compliance.Surface]int{
			compliance.SurfaceAdvisor:      1,
			compliance.SurfacePaperTrading: 1,
			compliance.SurfaceBacktest:     1,
			compliance.SurfaceCNIntraday:   1,
			compliance.SurfaceDailyPicks:   1,
		},
	}
}

func (a *complianceAdapter) CurrentMode() string {
	if a == nil {
		return string(compliance.DefaultMode)
	}
	return string(a.mode)
}

func (a *complianceAdapter) RecordAcknowledgment(userID string, input api.ComplianceAckInput) (*api.ComplianceAckView, error) {
	if a == nil || a.repo == nil {
		return nil, api.ErrComplianceUnconfigured
	}
	surface := strings.ToLower(strings.TrimSpace(input.Surface))
	if surface == "" {
		surface = "global"
	}
	mode := strings.ToLower(strings.TrimSpace(input.Mode))
	if mode == "" {
		mode = string(a.mode)
	}
	locale := strings.TrimSpace(input.Locale)
	if locale == "" {
		locale = "en"
	}
	textVersion := input.TextVersion
	if textVersion <= 0 {
		textVersion = a.versionFor(compliance.Surface(surface))
	}
	row := repository.AckRow{
		UserID:           userID,
		Surface:          surface,
		Mode:             mode,
		Locale:           locale,
		AcknowledgedText: input.AcknowledgedText,
		TextVersion:      textVersion,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := a.repo.UpsertAcknowledgment(ctx, row)
	if err != nil {
		return nil, err
	}
	return &api.ComplianceAckView{
		ID:               id,
		Surface:          surface,
		Mode:             mode,
		Locale:           locale,
		AcknowledgedAt:   time.Now().UTC(),
		AcknowledgedText: input.AcknowledgedText,
		TextVersion:      textVersion,
	}, nil
}

func (a *complianceAdapter) ListAcknowledgments(userID string) ([]api.ComplianceAckView, error) {
	if a == nil || a.repo == nil {
		return nil, api.ErrComplianceUnconfigured
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := a.repo.ListAcknowledgments(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]api.ComplianceAckView, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.ComplianceAckView{
			ID:               r.ID,
			Surface:          r.Surface,
			Mode:             r.Mode,
			Locale:           r.Locale,
			AcknowledgedAt:   r.AcknowledgedAt,
			AcknowledgedText: r.AcknowledgedText,
			TextVersion:      r.TextVersion,
		})
	}
	return out, nil
}

func (a *complianceAdapter) ListViolations(limit int) ([]api.CompliancePhraseViolationView, error) {
	if a == nil || a.repo == nil {
		return nil, api.ErrComplianceUnconfigured
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := a.repo.RecentViolations(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]api.CompliancePhraseViolationView, 0, len(rows))
	for _, r := range rows {
		view := api.CompliancePhraseViolationView{
			ID:             r.ID,
			Surface:        r.Surface,
			Rule:           r.Rule,
			OriginalPhrase: r.OriginalPhrase,
			Replacement:    r.Replacement,
			FlaggedAt:      r.FlaggedAt,
		}
		if r.UserID.Valid {
			view.UserID = r.UserID.String
		}
		if r.FullRedacted.Valid {
			view.FullRedacted = r.FullRedacted.String
		}
		if r.SourceEntity.Valid {
			view.SourceEntity = r.SourceEntity.String
		}
		if r.SourceID.Valid {
			view.SourceID = r.SourceID.String
		}
		out = append(out, view)
	}
	return out, nil
}

func (a *complianceAdapter) versionFor(surface compliance.Surface) int {
	if v, ok := a.textVersions[surface]; ok && v > 0 {
		return v
	}
	return 1
}
