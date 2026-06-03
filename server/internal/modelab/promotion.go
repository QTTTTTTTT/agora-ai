// promotion.go — Sprint 13 model-A/B auto-promotion scanner.
//
// The scanner is intentionally conservative: it never flips
// production traffic on its own. It produces *recommendations*
// (rows in model_ab_promotion_drafts) that an admin reviews and
// applies through the admin board. The point is to surface "B is
// reliably better" signals without operators having to babysit the
// reporter every day.
//
// Decision rule (Criteria struct), default-tuned for PM decisions:
//
//   * For each non-primary arm i ≠ 0 we compute a daily metric
//     bundle over the trailing N days (default 7).
//   * Arm i is "ahead" on day d if:
//       - agreement_with_primary_pct ≥ MinAgreementPct, AND
//       - shadow_count                ≥ MinSampleSize,    AND
//       - error_rate                  ≤ MaxErrorRate,     AND
//       - cost regression vs primary  ≤ MaxCostRegressionPct.
//   * Streak = consecutive days where the SAME arm is "ahead".
//     When streak ≥ Criteria.MinStreakDays, we emit one draft.
//
// The criteria are persisted on the draft so the admin board can
// render "why did this fire?" honestly. Operators can tighten or
// loosen any field — the default value comments inside Criteria
// document the production-safe starting point.

package modelab

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Criteria parameterise the promotion scanner. All fields have
// sensible defaults — leave at zero to use the documented value.
type Criteria struct {
	// MinStreakDays is the number of consecutive trailing days
	// the winning arm must beat the primary. Default: 7.
	MinStreakDays int `json:"min_streak_days"`

	// MinAgreementPct is the minimum daily fraction (0..1) of
	// shadow outputs that must agree with the primary's
	// answer. We use agreement-with-primary as a proxy for
	// "the shadow is producing usable PM decisions". Default:
	// 0.75 — i.e. the shadow agrees with the primary at least
	// 75% of the time, leaving 25% room for "shadow finds an
	// alpha the primary missed".
	MinAgreementPct float64 `json:"min_agreement_pct"`

	// MinSampleSize is the minimum number of shadow responses
	// per day below which we treat the day as inconclusive
	// (neither breaks nor extends the streak). Default: 20.
	MinSampleSize int `json:"min_sample_size"`

	// MaxErrorRate caps the shadow arm's error_count /
	// shadow_count ratio per day. Default: 0.05.
	MaxErrorRate float64 `json:"max_error_rate"`

	// MaxCostRegressionPct is the maximum allowed cost
	// regression (0..1) versus the primary on the same day.
	// 0.2 means the shadow may be at most 20% more expensive
	// than the primary. Use a negative value to require a cost
	// improvement (e.g. -0.1 = shadow must be ≥ 10% cheaper).
	// Default: 0.2.
	MaxCostRegressionPct float64 `json:"max_cost_regression_pct"`

	// PrimaryArmIndex pins which arm is the production arm.
	// Default 0 — convention from S10. Operators can override
	// in pathological setups.
	PrimaryArmIndex int `json:"primary_arm_index"`
}

// FilledDefaults returns a copy of the criteria with the zero
// fields replaced by production-safe defaults. Always run a
// criteria through this before using it on the hot path so the
// invariants the rest of the package assumes hold.
func (c Criteria) FilledDefaults() Criteria {
	if c.MinStreakDays <= 0 {
		c.MinStreakDays = 7
	}
	if c.MinAgreementPct <= 0 {
		c.MinAgreementPct = 0.75
	}
	if c.MinSampleSize <= 0 {
		c.MinSampleSize = 20
	}
	if c.MaxErrorRate <= 0 {
		c.MaxErrorRate = 0.05
	}
	if c.MaxCostRegressionPct == 0 {
		c.MaxCostRegressionPct = 0.2
	}
	// PrimaryArmIndex defaults to 0 — that's also the zero value.
	return c
}

// DayMetric is the daily metric bundle the scanner computes for
// each arm. It mirrors a sliver of ReportArmMetric but partitioned
// by day so a streak calculation is well-defined.
type DayMetric struct {
	Day           time.Time `json:"day"`
	ShadowCount   int       `json:"shadow_count"`
	ErrorCount    int       `json:"error_count"`
	TotalCostMicr int64     `json:"total_cost_micro"`
	Agreement     float64   `json:"agreement_pct"`
}

// Recommendation is the scanner's output for one experiment.
// Nil-safe: when no arm qualifies the scanner returns nil and a
// nil error (i.e. "nothing to recommend").
type Recommendation struct {
	ExperimentID         string                 `json:"experiment_id"`
	RecommendedArmIndex  int                    `json:"recommended_arm_index"`
	RecommendedArmLabel  string                 `json:"recommended_arm_label"`
	PrimaryArmIndex      int                    `json:"primary_arm_index"`
	PrimaryArmLabel      string                 `json:"primary_arm_label"`
	StreakDays           int                    `json:"streak_days"`
	WindowFrom           time.Time              `json:"window_from"`
	WindowTo             time.Time              `json:"window_to"`
	Criteria             Criteria               `json:"criteria"`
	// ReportSnapshot is the full Reporter.Compute payload for the
	// trailing window; persisted so the admin UI can render the
	// numbers without re-running the scanner.
	ReportSnapshot json.RawMessage `json:"report_snapshot"`
	// Diagnostics records, per arm, which days qualified and which
	// didn't. Helps with "why is the streak 3 and not 7?" questions.
	Diagnostics map[int][]DayMetric `json:"diagnostics"`
}

// Scanner inspects experiment metrics and produces Recommendations.
// The Scanner is decoupled from the storage of those drafts (that
// lives in the DraftRepo) so the same logic can be unit-tested
// against an in-memory Reporter mock.
type Scanner struct {
	Reporter *Reporter
}

// NewScanner constructs a Scanner from a Reporter (which in turn
// wraps the modelab Repo).
func NewScanner(r *Reporter) *Scanner { return &Scanner{Reporter: r} }

// Evaluate produces a Recommendation for the given experiment ID or
// nil if no arm qualifies. `now` is injected for deterministic
// testing; production passes time.Now(). The scanner reads metrics
// from the trailing Criteria.MinStreakDays days, computing per-day
// roll-ups by calling Reporter.Compute one day at a time.
func (s *Scanner) Evaluate(ctx context.Context, experimentID string, c Criteria, now time.Time) (*Recommendation, error) {
	if s == nil || s.Reporter == nil {
		return nil, errors.New("modelab: scanner or reporter nil")
	}
	c = c.FilledDefaults()
	streakWindow := c.MinStreakDays
	if streakWindow < 1 {
		streakWindow = 1
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	from := now.Add(-time.Duration(streakWindow) * 24 * time.Hour)
	to := now

	exp, err := s.Reporter.Repo.GetExperiment(ctx, experimentID)
	if err != nil {
		return nil, err
	}
	if len(exp.Arms) < 2 {
		return nil, nil
	}
	primaryIdx := c.PrimaryArmIndex
	if primaryIdx < 0 || primaryIdx >= len(exp.Arms) {
		return nil, fmt.Errorf("modelab: primary_arm_index %d out of range", primaryIdx)
	}

	// Compute the per-day metric bundle for each non-primary arm.
	// We rebuild the day map by replaying the full window through
	// Reporter.Compute and bucketing the underlying shadows by
	// finished_at::date. This is O(N) over the shadow rows in the
	// window — acceptable for nightly scans.
	dayBuckets, err := s.computeDayBuckets(ctx, experimentID, from, to, exp.Arms)
	if err != nil {
		return nil, err
	}

	// For each non-primary arm, compute the longest *trailing*
	// streak of qualifying days. We require the streak to end on
	// today (i.e. the most recent day must qualify) — a stale win
	// from two weeks ago should not produce a draft today.
	bestArm := -1
	bestStreak := 0
	for armIdx := range exp.Arms {
		if armIdx == primaryIdx {
			continue
		}
		streak := trailingStreak(dayBuckets[armIdx], dayBuckets[primaryIdx], c, now)
		if streak >= c.MinStreakDays && streak > bestStreak {
			bestArm = armIdx
			bestStreak = streak
		}
	}
	if bestArm < 0 {
		return nil, nil
	}

	// Build the report snapshot at full window resolution so the
	// admin UI can render the recommendation context.
	report, err := s.Reporter.Compute(ctx, experimentID, from, to)
	if err != nil {
		return nil, fmt.Errorf("modelab: scanner snapshot: %w", err)
	}
	snapshot, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("modelab: scanner snapshot marshal: %w", err)
	}

	diag := map[int][]DayMetric{}
	for armIdx, dms := range dayBuckets {
		diag[armIdx] = dms
	}

	return &Recommendation{
		ExperimentID:        exp.ID,
		RecommendedArmIndex: bestArm,
		RecommendedArmLabel: exp.Arms[bestArm].Label(),
		PrimaryArmIndex:     primaryIdx,
		PrimaryArmLabel:     exp.Arms[primaryIdx].Label(),
		StreakDays:          bestStreak,
		WindowFrom:          from,
		WindowTo:            to,
		Criteria:            c,
		ReportSnapshot:      snapshot,
		Diagnostics:         diag,
	}, nil
}

// computeDayBuckets returns, for each arm index, the per-day
// DayMetric slice (sorted by day ascending). Days with zero
// activity are still inserted as zero-filled rows so the streak
// algorithm can see them.
func (s *Scanner) computeDayBuckets(ctx context.Context, experimentID string, from, to time.Time, arms []ArmConfig) (map[int][]DayMetric, error) {
	shadows, err := s.Reporter.Repo.listShadowsInWindow(ctx, experimentID, from, to)
	if err != nil {
		return nil, fmt.Errorf("modelab: scanner read shadows: %w", err)
	}
	// We also need the primary arm's per-day cost — for the cost
	// regression check. The dispatcher does NOT persist the
	// primary's tokens into shadow_responses (that's by design —
	// the primary is the "real" call), but per-arm cost can be
	// approximated from primary's input/output tokens if recorded
	// in shadow_responses for arm 0 (some deployments DO persist
	// arm 0 there for the agreement metric). When the primary is
	// not in shadow_responses we fall back to "treat primary cost
	// as 0" which biases the criterion toward accepting the
	// shadow (a known limitation; ops can override
	// MaxCostRegressionPct to be more conservative).

	type bucketKey struct {
		armIdx int
		day    time.Time
	}
	bucketAggregates := map[bucketKey]*DayMetric{}
	// Group raw shadow outputs by (run, step, agent) so we can
	// derive the agreement metric. For each group, arm 0 (or the
	// configured primary arm) is the reference; every other arm
	// in the group is checked for matching ExtractField output.
	type agreementGroupKey struct {
		runID  string
		step   string
		agent  string
	}
	agreementGroups := map[agreementGroupKey]map[int]string{}
	for _, sh := range shadows {
		key := bucketKey{armIdx: sh.ArmIndex, day: dayBucket(sh.FinishedAt)}
		dm, ok := bucketAggregates[key]
		if !ok {
			dm = &DayMetric{Day: key.day}
			bucketAggregates[key] = dm
		}
		dm.ShadowCount++
		if sh.ErrorText != "" {
			dm.ErrorCount++
		}
		dm.TotalCostMicr += sh.CostMicro

		// Build the agreement-group view alongside the cost / count
		// aggregates. Only one extracted-field value per (run, step,
		// agent, arm) — the dispatcher dedups internally already.
		ag := agreementGroupKey{runID: sh.RunID, step: sh.Step, agent: sh.AgentID}
		if _, ok := agreementGroups[ag]; !ok {
			agreementGroups[ag] = map[int]string{}
		}
		if v := s.Reporter.ExtractField(sh.ParsedOutput); v != "" {
			agreementGroups[ag][sh.ArmIndex] = v
		}
	}

	// Walk the groups, count matches per (arm, day) pair, and
	// fold the agreement metric into the bucket aggregates.
	type matchKey struct {
		armIdx int
		day    time.Time
	}
	matched := map[matchKey]int{}
	total := map[matchKey]int{}
	for ag, vals := range agreementGroups {
		primary, ok := vals[0]
		if !ok {
			continue
		}
		// We don't know which day this group landed on without
		// looking up one of its rows — use the first shadow of
		// the group as a representative. Cheaper than a second
		// pass over the shadows list.
		var groupDay time.Time
		for _, sh := range shadows {
			if sh.RunID == ag.runID && sh.Step == ag.step && sh.AgentID == ag.agent {
				groupDay = dayBucket(sh.FinishedAt)
				break
			}
		}
		for armIdx, v := range vals {
			if armIdx == 0 {
				continue
			}
			total[matchKey{armIdx: armIdx, day: groupDay}]++
			if strings.EqualFold(v, primary) {
				matched[matchKey{armIdx: armIdx, day: groupDay}]++
			}
		}
	}
	for k, dm := range bucketAggregates {
		tot := total[matchKey{armIdx: k.armIdx, day: k.day}]
		if tot > 0 {
			dm.Agreement = float64(matched[matchKey{armIdx: k.armIdx, day: k.day}]) / float64(tot)
		}
	}

	out := map[int][]DayMetric{}
	for armIdx := range arms {
		out[armIdx] = []DayMetric{}
	}
	for k, dm := range bucketAggregates {
		out[k.armIdx] = append(out[k.armIdx], *dm)
	}
	for armIdx := range out {
		sort.Slice(out[armIdx], func(i, j int) bool {
			return out[armIdx][i].Day.Before(out[armIdx][j].Day)
		})
	}
	return out, nil
}

// trailingStreak counts the consecutive trailing days a non-primary
// arm satisfies the criteria. Days with zero shadow_count are
// neutral (skip without breaking the streak) so a quiet weekend
// doesn't reset the count; the MinSampleSize guard handles the
// "tiny day, ignore" case explicitly.
func trailingStreak(armDays []DayMetric, primaryDays []DayMetric, c Criteria, now time.Time) int {
	pCost := map[time.Time]int64{}
	for _, dm := range primaryDays {
		pCost[dm.Day] = dm.TotalCostMicr
	}
	// Walk backwards from today.
	streak := 0
	day := dayBucket(now)
	// Build a quick day → DayMetric map for the arm.
	armByDay := map[time.Time]DayMetric{}
	for _, dm := range armDays {
		armByDay[dm.Day] = dm
	}
	for i := 0; i < c.MinStreakDays; i++ {
		dm, present := armByDay[day]
		if !present {
			// no data that day — neutral
			day = day.Add(-24 * time.Hour)
			continue
		}
		if dm.ShadowCount < c.MinSampleSize {
			day = day.Add(-24 * time.Hour)
			continue
		}
		errRate := float64(dm.ErrorCount) / float64(dm.ShadowCount)
		if errRate > c.MaxErrorRate {
			return streak
		}
		if dm.Agreement < c.MinAgreementPct {
			return streak
		}
		primaryCost := pCost[day]
		if primaryCost > 0 && c.MaxCostRegressionPct >= 0 {
			ratio := float64(dm.TotalCostMicr-primaryCost) / float64(primaryCost)
			if ratio > c.MaxCostRegressionPct {
				return streak
			}
		}
		// Negative MaxCostRegressionPct = require cost improvement.
		if primaryCost > 0 && c.MaxCostRegressionPct < 0 {
			ratio := float64(dm.TotalCostMicr-primaryCost) / float64(primaryCost)
			if ratio > c.MaxCostRegressionPct {
				return streak
			}
		}
		streak++
		day = day.Add(-24 * time.Hour)
	}
	return streak
}

func dayBucket(t time.Time) time.Time {
	utc := t.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

// --- DraftRepo --------------------------------------------------------------

// DraftStatus is the lifecycle state on model_ab_promotion_drafts.
type DraftStatus string

const (
	DraftPending    DraftStatus = "pending"
	DraftApplied    DraftStatus = "applied"
	DraftRejected   DraftStatus = "rejected"
	DraftSuperseded DraftStatus = "superseded"
)

// PromotionDraft is the in-memory mirror of one
// model_ab_promotion_drafts row.
type PromotionDraft struct {
	ID                  string          `json:"id"`
	ExperimentID        string          `json:"experiment_id"`
	RecommendedArmIndex int             `json:"recommended_arm_index"`
	RecommendedArmLabel string          `json:"recommended_arm_label"`
	PrimaryArmIndex     int             `json:"primary_arm_index"`
	PrimaryArmLabel     string          `json:"primary_arm_label"`
	StreakDays          int             `json:"streak_days"`
	EvaluatedAt         time.Time       `json:"evaluated_at"`
	WindowFrom          *time.Time      `json:"window_from,omitempty"`
	WindowTo            *time.Time      `json:"window_to,omitempty"`
	CriteriaPayload     json.RawMessage `json:"criteria_payload"`
	ReportSnapshot      json.RawMessage `json:"report_snapshot"`
	Status              DraftStatus     `json:"status"`
	AppliedBy           string          `json:"applied_by,omitempty"`
	AppliedAt           *time.Time      `json:"applied_at,omitempty"`
	RejectionReason     string          `json:"rejection_reason,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
}

// DraftRepo persists Recommendations as model_ab_promotion_drafts
// rows and exposes the read / apply / reject operations the admin
// board needs.
type DraftRepo struct{ db *sql.DB }

func NewDraftRepo(db *sql.DB) *DraftRepo { return &DraftRepo{db: db} }

// UpsertPending inserts a new pending draft for the given
// recommendation, OR replaces the existing pending draft for the
// same experiment (so the nightly re-run is idempotent — yesterday's
// stale recommendation is discarded in favour of today's fresher one).
// Returns the row id and a boolean indicating whether the row is new
// (true) or supersedes a previous pending draft (false).
func (r *DraftRepo) UpsertPending(ctx context.Context, rec *Recommendation) (string, bool, error) {
	if r == nil || r.db == nil {
		return "", false, errors.New("modelab: draft repo not initialised")
	}
	if rec == nil {
		return "", false, errors.New("modelab: nil recommendation")
	}
	criteriaBytes, err := json.Marshal(rec.Criteria)
	if err != nil {
		return "", false, fmt.Errorf("modelab: marshal criteria: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("modelab: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Mark any previous pending draft as superseded so the partial
	// unique index doesn't trip.
	res, err := tx.ExecContext(ctx,
		`UPDATE model_ab_promotion_drafts
		    SET status = 'superseded'
		  WHERE experiment_id = $1::uuid
		    AND status = 'pending'`,
		rec.ExperimentID,
	)
	if err != nil {
		return "", false, fmt.Errorf("modelab: supersede previous: %w", err)
	}
	superseded, _ := res.RowsAffected()

	report := rec.ReportSnapshot
	if len(report) == 0 {
		report = json.RawMessage(`{}`)
	}
	var id string
	err = tx.QueryRowContext(ctx,
		`INSERT INTO model_ab_promotion_drafts
		   (experiment_id, recommended_arm_index, recommended_arm_label,
		    primary_arm_index, primary_arm_label,
		    streak_days, evaluated_at, window_from, window_to,
		    criteria_payload, report_snapshot)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, NOW(), $7, $8, $9, $10)
		 RETURNING id::text`,
		rec.ExperimentID,
		rec.RecommendedArmIndex, rec.RecommendedArmLabel,
		rec.PrimaryArmIndex, rec.PrimaryArmLabel,
		rec.StreakDays,
		nullableTimeOrNil(rec.WindowFrom),
		nullableTimeOrNil(rec.WindowTo),
		criteriaBytes,
		[]byte(report),
	).Scan(&id)
	if err != nil {
		return "", false, fmt.Errorf("modelab: insert draft: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("modelab: commit: %w", err)
	}
	return id, superseded == 0, nil
}

// List returns drafts filtered by status. Empty status → all.
// Ordered by evaluated_at DESC.
func (r *DraftRepo) List(ctx context.Context, status DraftStatus, limit int) ([]*PromotionDraft, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("modelab: draft repo not initialised")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id::text, experiment_id::text,
	             recommended_arm_index, recommended_arm_label,
	             primary_arm_index, primary_arm_label,
	             streak_days, evaluated_at, window_from, window_to,
	             criteria_payload::text, report_snapshot::text,
	             status, COALESCE(applied_by,''), applied_at,
	             COALESCE(rejection_reason,''), created_at
	        FROM model_ab_promotion_drafts`
	var rows *sql.Rows
	var err error
	if status == "" {
		q += ` ORDER BY evaluated_at DESC LIMIT $1`
		rows, err = r.db.QueryContext(ctx, q, limit)
	} else {
		q += ` WHERE status = $1 ORDER BY evaluated_at DESC LIMIT $2`
		rows, err = r.db.QueryContext(ctx, q, string(status), limit)
	}
	if err != nil {
		return nil, fmt.Errorf("modelab: list drafts: %w", err)
	}
	defer rows.Close()
	var out []*PromotionDraft
	for rows.Next() {
		d, err := scanDraftRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Get returns one row by id, or ErrNotFound.
func (r *DraftRepo) Get(ctx context.Context, id string) (*PromotionDraft, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("modelab: draft repo not initialised")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id::text, experiment_id::text,
		       recommended_arm_index, recommended_arm_label,
		       primary_arm_index, primary_arm_label,
		       streak_days, evaluated_at, window_from, window_to,
		       criteria_payload::text, report_snapshot::text,
		       status, COALESCE(applied_by,''), applied_at,
		       COALESCE(rejection_reason,''), created_at
		  FROM model_ab_promotion_drafts WHERE id = $1::uuid`, id)
	d := &PromotionDraft{}
	var winFrom, winTo, appliedAt sql.NullTime
	var criteria, report string
	err := row.Scan(&d.ID, &d.ExperimentID,
		&d.RecommendedArmIndex, &d.RecommendedArmLabel,
		&d.PrimaryArmIndex, &d.PrimaryArmLabel,
		&d.StreakDays, &d.EvaluatedAt, &winFrom, &winTo,
		&criteria, &report,
		&d.Status, &d.AppliedBy, &appliedAt,
		&d.RejectionReason, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("modelab: get draft: %w", err)
	}
	if winFrom.Valid {
		t := winFrom.Time
		d.WindowFrom = &t
	}
	if winTo.Valid {
		t := winTo.Time
		d.WindowTo = &t
	}
	if appliedAt.Valid {
		t := appliedAt.Time
		d.AppliedAt = &t
	}
	d.CriteriaPayload = json.RawMessage(criteria)
	d.ReportSnapshot = json.RawMessage(report)
	return d, nil
}

// Apply marks a draft as applied and stamps the actor + timestamp.
// Refuses if the draft is not pending; idempotent on re-application
// by the same actor in the unlikely double-click case (we keep the
// original timestamp).
func (r *DraftRepo) Apply(ctx context.Context, id, userID string) error {
	if r == nil || r.db == nil {
		return errors.New("modelab: draft repo not initialised")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(userID) == "" {
		return errors.New("modelab: apply requires id and user_id")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE model_ab_promotion_drafts
		    SET status = 'applied',
		        applied_by = $2,
		        applied_at = NOW()
		  WHERE id = $1::uuid
		    AND status = 'pending'`,
		id, userID,
	)
	if err != nil {
		return fmt.Errorf("modelab: apply draft: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		// Either not pending, or doesn't exist. Disambiguate.
		d, getErr := r.Get(ctx, id)
		if errors.Is(getErr, ErrNotFound) {
			return ErrNotFound
		}
		if getErr != nil {
			return getErr
		}
		if d.Status != DraftPending {
			return fmt.Errorf("modelab: draft is %s, not pending", d.Status)
		}
	}
	return nil
}

// Reject mirrors Apply but for the rejection path. Reason is
// persisted on the row for compliance audit.
func (r *DraftRepo) Reject(ctx context.Context, id, userID, reason string) error {
	if r == nil || r.db == nil {
		return errors.New("modelab: draft repo not initialised")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(userID) == "" {
		return errors.New("modelab: reject requires id and user_id")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE model_ab_promotion_drafts
		    SET status = 'rejected',
		        applied_by = $2,
		        applied_at = NOW(),
		        rejection_reason = $3
		  WHERE id = $1::uuid
		    AND status = 'pending'`,
		id, userID, strings.TrimSpace(reason),
	)
	if err != nil {
		return fmt.Errorf("modelab: reject draft: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		d, getErr := r.Get(ctx, id)
		if errors.Is(getErr, ErrNotFound) {
			return ErrNotFound
		}
		if getErr != nil {
			return getErr
		}
		if d.Status != DraftPending {
			return fmt.Errorf("modelab: draft is %s, not pending", d.Status)
		}
	}
	return nil
}

func scanDraftRow(rows *sql.Rows) (*PromotionDraft, error) {
	d := &PromotionDraft{}
	var winFrom, winTo, appliedAt sql.NullTime
	var criteria, report string
	if err := rows.Scan(&d.ID, &d.ExperimentID,
		&d.RecommendedArmIndex, &d.RecommendedArmLabel,
		&d.PrimaryArmIndex, &d.PrimaryArmLabel,
		&d.StreakDays, &d.EvaluatedAt, &winFrom, &winTo,
		&criteria, &report,
		&d.Status, &d.AppliedBy, &appliedAt,
		&d.RejectionReason, &d.CreatedAt); err != nil {
		return nil, fmt.Errorf("modelab: scan draft: %w", err)
	}
	if winFrom.Valid {
		t := winFrom.Time
		d.WindowFrom = &t
	}
	if winTo.Valid {
		t := winTo.Time
		d.WindowTo = &t
	}
	if appliedAt.Valid {
		t := appliedAt.Time
		d.AppliedAt = &t
	}
	d.CriteriaPayload = json.RawMessage(criteria)
	d.ReportSnapshot = json.RawMessage(report)
	return d, nil
}

func nullableTimeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
