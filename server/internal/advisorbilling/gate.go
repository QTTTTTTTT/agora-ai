// Package advisorbilling implements the per-user quota gate that
// fronts every /api/advisor/consult call.
//
// Why a separate package (not part of advisor or subscription):
//   - subscription is fund-scoped; advisor consultations are
//     fund-less, and mixing the two ledgers would couple the
//     fund-mode kill-switch to advisor monetisation.
//   - advisor.Service is the orchestrator; adding "are you over
//     quota?" logic there would entangle business rules with
//     LLM panel routing and make Service.Consult hard to test.
//   - Phase C (Credit packs) will plug a CreditRepo into the same
//     Gate; doing it as a single Gate type that fans out to several
//     ledgers in Check() lets us keep the call-site (handleConsult)
//     stable while we add layers underneath.
//
// One Gate instance lives on the wired server for the lifetime of
// the process. Every consult flows through Gate.Check then
// Gate.Consume — no lock is held between the two calls (a tiny
// race window where two concurrent consults each see "1 unit left"
// and both proceed is acceptable for a UX-tier guardrail; hard
// enforcement happens at the DB CHECK level on consumed >= 0).
package advisorbilling

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/advisor"
	"github.com/fundai/server/internal/subscription"
)

// ConsultKind classifies a consultation for billing purposes.
// Computed from the resolved persona preset's master/tactic key
// counts, NOT from the wire-shape — so a "custom" preset with a
// single master still gets quick-priced and a deep preset with
// only one master left after kind enforcement still gets deep.
//
// Cutoff at >= 2 agents on either side keeps the math simple:
// single-agent presets (single master OR single tactic) are
// "quick" and burn the quick bucket; everything else is "deep".
type ConsultKind string

const (
	KindDeep  ConsultKind = "deep"
	KindQuick ConsultKind = "quick"
)

// ClassifyPreset returns the billing kind for a resolved preset
// and the actual master/tactic key lists that will be fanned out
// (the service applies the same `len() >= 2` heuristic).
//
// custom preset with empty stored keys falls back to the wire-
// provided custom keys via the masterKeys / tacticKeys args the
// caller passes in (matching what advisor.Service.resolveKeys
// produces). For a clean "no keys at all" case we treat it as
// quick — the gate will admit it, then the service will reject
// with ErrUnsupportedPreset and the consume hook never fires.
func ClassifyPreset(preset advisor.PersonaPreset, resolvedMasters, resolvedTactics []string) ConsultKind {
	mc, tc := len(resolvedMasters), len(resolvedTactics)
	if mc+tc >= 2 {
		return KindDeep
	}
	_ = preset
	return KindQuick
}

// EffectivePlanLookup is the subset of subscription.SubscriptionService
// the Gate depends on. Defined as an interface so tests can swap in
// a fake that returns whatever Plan they want without spinning up
// a real subscription service.
type EffectivePlanLookup interface {
	GetEffectivePlan(ctx context.Context, userID string) (*subscription.Plan, error)
}

// Gate is the per-user advisor consultation guardrail.
type Gate struct {
	db      *sql.DB
	plans   EffectivePlanLookup
	credits *CreditsRepo

	// clock and yearMonthFn are injectable for tests; production
	// gets time.Now and a UTC YYYY-MM formatter.
	clock       func() time.Time
	yearMonthFn func(time.Time) string
}

// GateOption configures construction.
type GateOption func(*Gate)

// WithClock overrides the clock (used by tests).
func WithClock(c func() time.Time) GateOption {
	return func(g *Gate) {
		if c != nil {
			g.clock = c
		}
	}
}

// WithCreditsRepo plugs the Phase C-1 credit-pack ledger into the
// gate. When supplied, Check/Consume cascade plan → credits before
// returning QuotaExceeded. nil-safe: leaving credits unset gives
// Phase A behaviour (plan-only).
func WithCreditsRepo(repo *CreditsRepo) GateOption {
	return func(g *Gate) {
		g.credits = repo
	}
}

// NewGate wires the Gate with the supplied DB handle + plan lookup.
// Either being nil is permitted — Check will then return ErrUnconfigured,
// matching the "service not wired" branch of the advisor handler.
func NewGate(db *sql.DB, plans EffectivePlanLookup, opts ...GateOption) *Gate {
	g := &Gate{
		db:          db,
		plans:       plans,
		clock:       time.Now,
		yearMonthFn: defaultYearMonth,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// ErrUnconfigured means the wiring layer didn't attach a Gate; the
// handler should fall back to "advisor unavailable" rather than
// silently bypassing the quota check.
var ErrUnconfigured = errors.New("advisorbilling: gate not configured")

// QuotaExceededError is returned by Check when the user's plan
// quota for the relevant ConsultKind is exhausted. Surfaces as
// HTTP 402 Payment Required at the handler so the front-end can
// branch on it (vs. a generic 5xx).
type QuotaExceededError struct {
	Kind            ConsultKind
	PlanTier        subscription.PlanTier
	Limit           int
	Used            int
	NextResetAt     time.Time
	UpgradeSuggested subscription.PlanTier
}

func (e *QuotaExceededError) Error() string {
	limit := "0"
	if e != nil {
		limit = fmt.Sprintf("%d", e.Limit)
	}
	if e == nil {
		return "advisorbilling: quota exceeded"
	}
	return fmt.Sprintf(
		"advisorbilling: %s quota exceeded for plan %s (used=%d, limit=%s)",
		e.Kind, e.PlanTier, e.Used, limit,
	)
}

// IsQuotaExceeded reports whether err is a QuotaExceededError.
func IsQuotaExceeded(err error) bool {
	var q *QuotaExceededError
	return errors.As(err, &q)
}

// UnitSource records which bucket paid for (or will pay for) a
// consultation. Surfaced into advisor_consultations.service_unit_source
// for auditing.
type UnitSource string

const (
	SourcePlan      UnitSource = "plan"
	SourceCredit    UnitSource = "credit"
	SourceUnmetered UnitSource = "unmetered" // admin / system overrides
)

// Decision is returned by Check on success — the handler can use
// the remaining counts to set rate-limit-style response headers.
type Decision struct {
	Kind             ConsultKind
	PlanTier         subscription.PlanTier
	Source           UnitSource
	Limit            int
	Used             int
	Remaining        int
	NextResetAt      time.Time
	CreditBalance    int
}

// Check returns nil + Decision when the user can proceed with a
// consult of the given kind. The check cascades:
//
//   1. plan-included monthly bucket — when the user is under the
//      plan's deep/quick cap, the call is admitted and the
//      Decision.Source is "plan".
//   2. credit-pack balance — when plan units are exhausted but
//      the user has paid credits left, the call is admitted and
//      the Decision.Source is "credit".
//   3. quota exhausted — returns *QuotaExceededError. The handler
//      surfaces it as HTTP 402 with the upgrade suggestion.
//
// Phase C upgrades this from the Phase A "plan-only" version
// without changing the call-site signature; existing handlers
// keep working.
func (g *Gate) Check(ctx context.Context, userID string, kind ConsultKind) (*Decision, error) {
	if g == nil || g.db == nil || g.plans == nil {
		return nil, ErrUnconfigured
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, errors.New("advisorbilling: userID required")
	}
	plan, err := g.plans.GetEffectivePlan(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("advisorbilling: load plan: %w", err)
	}
	if plan == nil {
		plan = subscription.Plans[subscription.PlanFree]
	}

	limit := planLimitFor(plan, kind)
	month := g.currentYearMonth()
	used, err := g.readUsed(ctx, userID, month, kind)
	if err != nil {
		return nil, err
	}
	dec := &Decision{
		Kind:        kind,
		PlanTier:    plan.Tier,
		Source:      SourcePlan,
		Limit:       limit,
		Used:        used,
		NextResetAt: g.nextResetAt(),
	}
	// Layer 1: unlimited plan.
	if limit < 0 {
		dec.Remaining = -1
		return dec, nil
	}
	// Layer 1 (cont'd): plan-included monthly bucket has room.
	if used < limit {
		dec.Remaining = limit - used
		// Best-effort: include credit balance so the SPA can
		// display "12 plan + 30 credits left this month".
		if g.credits != nil {
			if bal, berr := g.credits.Balance(ctx, userID); berr == nil && bal != nil {
				dec.CreditBalance = creditBalanceFor(bal, kind)
			}
		}
		return dec, nil
	}
	// Layer 2: credit-pack fallback.
	if g.credits != nil {
		bal, berr := g.credits.Balance(ctx, userID)
		if berr == nil && bal != nil {
			credit := creditBalanceFor(bal, kind)
			if credit > 0 {
				dec.Source = SourceCredit
				dec.CreditBalance = credit
				dec.Remaining = credit
				return dec, nil
			}
		}
	}
	// Layer 3: exhausted.
	return nil, &QuotaExceededError{
		Kind:             kind,
		PlanTier:         plan.Tier,
		Limit:            limit,
		Used:             used,
		NextResetAt:      dec.NextResetAt,
		UpgradeSuggested: suggestUpgrade(plan.Tier),
	}
}

// Consume atomically debits one unit from the user's quota. Called
// by the handler ONLY after the underlying advisor.Service.Consult
// returns a non-nil result — we don't charge users for consults
// that errored upstream of the LLM (e.g. preset not found, panel
// build failure).
//
// The expected source is passed in (decided by the matching Check
// call moments earlier) so we know which ledger to debit. When
// the expected source is "plan" but the plan is full at consume
// time (rare race: two near-simultaneous consults both saw room),
// we transparently fall back to the credit ledger.
//
// Returns the post-consume Decision so the handler can stamp the
// X-Advisor-Remaining headers on the response.
func (g *Gate) Consume(ctx context.Context, userID string, kind ConsultKind, expected UnitSource) (*Decision, error) {
	if g == nil || g.db == nil || g.plans == nil {
		return nil, ErrUnconfigured
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, errors.New("advisorbilling: userID required")
	}
	plan, err := g.plans.GetEffectivePlan(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("advisorbilling: load plan: %w", err)
	}
	if plan == nil {
		plan = subscription.Plans[subscription.PlanFree]
	}
	limit := planLimitFor(plan, kind)

	// Path 1 — plan-included bucket.
	if expected == SourcePlan || expected == "" {
		dec, err := g.consumePlanUnit(ctx, userID, kind, plan, limit)
		if err == nil {
			return dec, nil
		}
		if !errors.Is(err, errPlanFull) {
			return nil, err
		}
		// Fall through to credits.
	}

	// Path 2 — credit pack bucket.
	if g.credits != nil {
		ok, cerr := g.credits.ConsumeCredits(ctx, userID, kind)
		if cerr != nil {
			return nil, cerr
		}
		if ok {
			bal, berr := g.credits.Balance(ctx, userID)
			remaining := 0
			if berr == nil && bal != nil {
				remaining = creditBalanceFor(bal, kind)
			}
			return &Decision{
				Kind:          kind,
				PlanTier:      plan.Tier,
				Source:        SourceCredit,
				Limit:         limit,
				NextResetAt:   g.nextResetAt(),
				CreditBalance: remaining,
				Remaining:     remaining,
			}, nil
		}
	}

	// Nothing left to debit. The handler already ran the panel
	// (we're in the post-call hook), so refusing here doesn't
	// help the user. We return a Decision with Remaining=0 +
	// Source=unmetered so the audit row reflects "we couldn't
	// charge for this one" and ops can investigate.
	return &Decision{
		Kind:        kind,
		PlanTier:    plan.Tier,
		Source:      SourceUnmetered,
		Limit:       limit,
		NextResetAt: g.nextResetAt(),
	}, nil
}

// errPlanFull is an internal sentinel signalling "plan-included
// bucket is exhausted — try credits". Not exported.
var errPlanFull = errors.New("advisorbilling: plan-included monthly bucket full")

func (g *Gate) consumePlanUnit(ctx context.Context, userID string, kind ConsultKind, plan *subscription.Plan, limit int) (*Decision, error) {
	month := g.currentYearMonth()
	column, err := columnFor(kind)
	if err != nil {
		return nil, err
	}
	now := g.clock().UTC()
	q := fmt.Sprintf(`
		INSERT INTO user_advisor_monthly_usage
		    (user_id, year_month, %s, last_consumed_at)
		VALUES ($1, $2, 1, $3)
		ON CONFLICT (user_id, year_month) DO UPDATE
		SET %s = user_advisor_monthly_usage.%s + 1,
		    last_consumed_at = EXCLUDED.last_consumed_at
		RETURNING deep_units_consumed, quick_units_consumed
	`, column, column, column)
	var deepUsed, quickUsed int
	if err := g.db.QueryRowContext(ctx, q, userID, month, now).Scan(&deepUsed, &quickUsed); err != nil {
		return nil, fmt.Errorf("advisorbilling: consume %s unit: %w", kind, err)
	}
	used := deepUsed
	if kind == KindQuick {
		used = quickUsed
	}
	// Limit < 0 → unlimited, always succeeds via plan.
	// Limit >= 0 and after-increment used > limit → we just
	// crossed the cap; reverse-decrement so we don't charge the
	// user this consultation against the plan, and signal a
	// fall-through to credits.
	if limit >= 0 && used > limit {
		// Best-effort reversal — if it fails we still let the
		// fallthrough happen; the worst case is the user shows
		// 1 over quota for ~24h until next consultation.
		undoQ := fmt.Sprintf(`
			UPDATE user_advisor_monthly_usage
			SET %s = GREATEST(%s - 1, 0), updated_at = NOW()
			WHERE user_id = $1 AND year_month = $2
		`, column, column)
		_, _ = g.db.ExecContext(ctx, undoQ, userID, month)
		return nil, errPlanFull
	}
	dec := &Decision{
		Kind:        kind,
		PlanTier:    plan.Tier,
		Source:      SourcePlan,
		Limit:       limit,
		Used:        used,
		NextResetAt: g.nextResetAt(),
	}
	if limit < 0 {
		dec.Remaining = -1
	} else {
		dec.Remaining = limit - used
	}
	return dec, nil
}

// Summary is the read-only view powering GET /api/advisor/billing/summary.
type Summary struct {
	PlanTier             subscription.PlanTier `json:"plan_tier"`
	YearMonth            string                `json:"year_month"`
	DeepLimit            int                   `json:"deep_limit"`
	DeepUsed             int                   `json:"deep_used"`
	DeepRemaining        int                   `json:"deep_remaining"`
	QuickLimit           int                   `json:"quick_limit"`
	QuickUsed            int                   `json:"quick_used"`
	QuickRemaining       int                   `json:"quick_remaining"`
	NextResetAt          time.Time             `json:"next_reset_at"`
	AllowAdvisorBYOK     bool                  `json:"allow_advisor_byok"`
	UpgradeSuggested     subscription.PlanTier `json:"upgrade_suggested,omitempty"`
	CreditDeepBalance    int                   `json:"credit_deep_balance"`
	CreditQuickBalance   int                   `json:"credit_quick_balance"`
	TotalPurchasedCents  int64                 `json:"total_purchased_cents"`
}

// Summary returns the user's current advisor billing snapshot.
// Used by the AdvisorBillingHeader in the SPA and by the BYOK
// settings page to render "you have 87 / 100 deep consults left
// this month, resets 2026-07-01".
func (g *Gate) Summary(ctx context.Context, userID string) (*Summary, error) {
	if g == nil || g.db == nil || g.plans == nil {
		return nil, ErrUnconfigured
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, errors.New("advisorbilling: userID required")
	}
	plan, err := g.plans.GetEffectivePlan(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("advisorbilling: load plan: %w", err)
	}
	if plan == nil {
		plan = subscription.Plans[subscription.PlanFree]
	}
	month := g.currentYearMonth()

	var deepUsed, quickUsed int
	err = g.db.QueryRowContext(ctx, `
		SELECT deep_units_consumed, quick_units_consumed
		FROM user_advisor_monthly_usage
		WHERE user_id = $1 AND year_month = $2
	`, userID, month).Scan(&deepUsed, &quickUsed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("advisorbilling: read summary: %w", err)
	}

	deepLimit := plan.AdvisorDeepUnitsPerMonth
	quickLimit := plan.AdvisorQuickUnitsPerMonth
	summary := &Summary{
		PlanTier:         plan.Tier,
		YearMonth:        month,
		DeepLimit:        deepLimit,
		DeepUsed:         deepUsed,
		QuickLimit:       quickLimit,
		QuickUsed:        quickUsed,
		NextResetAt:      g.nextResetAt(),
		AllowAdvisorBYOK: plan.AllowAdvisorBYOK,
	}
	summary.DeepRemaining = remainingFor(deepLimit, deepUsed)
	summary.QuickRemaining = remainingFor(quickLimit, quickUsed)
	// Layer-2 credit-pack balance.
	if g.credits != nil {
		if bal, berr := g.credits.Balance(ctx, userID); berr == nil && bal != nil {
			summary.CreditDeepBalance = bal.DeepUnitsBalance
			summary.CreditQuickBalance = bal.QuickUnitsBalance
			summary.TotalPurchasedCents = bal.TotalPurchasedCents
		}
	}
	// Upgrade hint only when BOTH plan and credit buckets are
	// empty on at least one side. Otherwise users with a healthy
	// credit balance see a wrongly-aggressive upsell.
	if (summary.DeepRemaining == 0 && summary.CreditDeepBalance == 0) ||
		(summary.QuickRemaining == 0 && summary.CreditQuickBalance == 0) {
		summary.UpgradeSuggested = suggestUpgrade(plan.Tier)
	}
	return summary, nil
}

func creditBalanceFor(b *Balance, kind ConsultKind) int {
	if b == nil {
		return 0
	}
	switch kind {
	case KindDeep:
		return b.DeepUnitsBalance
	case KindQuick:
		return b.QuickUnitsBalance
	default:
		return 0
	}
}

// --- internals -------------------------------------------------------------

func (g *Gate) readUsed(ctx context.Context, userID, month string, kind ConsultKind) (int, error) {
	column, err := columnFor(kind)
	if err != nil {
		return 0, err
	}
	q := fmt.Sprintf(`
		SELECT %s
		FROM user_advisor_monthly_usage
		WHERE user_id = $1 AND year_month = $2
	`, column)
	var used int
	if err := g.db.QueryRowContext(ctx, q, userID, month).Scan(&used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("advisorbilling: read %s used: %w", kind, err)
	}
	return used, nil
}

func (g *Gate) currentYearMonth() string {
	return g.yearMonthFn(g.clock().UTC())
}

func (g *Gate) nextResetAt() time.Time {
	now := g.clock().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return first.AddDate(0, 1, 0)
}

func defaultYearMonth(t time.Time) string {
	return t.UTC().Format("2006-01")
}

func planLimitFor(plan *subscription.Plan, kind ConsultKind) int {
	if plan == nil {
		return 0
	}
	switch kind {
	case KindDeep:
		return plan.AdvisorDeepUnitsPerMonth
	case KindQuick:
		return plan.AdvisorQuickUnitsPerMonth
	default:
		return 0
	}
}

func columnFor(kind ConsultKind) (string, error) {
	switch kind {
	case KindDeep:
		return "deep_units_consumed", nil
	case KindQuick:
		return "quick_units_consumed", nil
	default:
		return "", fmt.Errorf("advisorbilling: unknown ConsultKind %q", kind)
	}
}

func remainingFor(limit, used int) int {
	if limit < 0 {
		return -1
	}
	if used >= limit {
		return 0
	}
	return limit - used
}

func suggestUpgrade(tier subscription.PlanTier) subscription.PlanTier {
	switch tier {
	case subscription.PlanFree:
		return subscription.PlanPro
	case subscription.PlanPro:
		return subscription.PlanPremium
	case subscription.PlanPremium:
		return subscription.PlanEnterprise
	default:
		return ""
	}
}
