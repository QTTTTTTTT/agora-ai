package advisorbilling

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/advisor"
	"github.com/fundai/server/internal/subscription"
)

type stubPlanLookup struct {
	plan *subscription.Plan
	err  error
}

func (s stubPlanLookup) GetEffectivePlan(_ context.Context, _ string) (*subscription.Plan, error) {
	return s.plan, s.err
}

func freshGate(t *testing.T, plan *subscription.Plan) (*Gate, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	frozen := time.Date(2026, 6, 7, 14, 30, 0, 0, time.UTC)
	g := NewGate(db, stubPlanLookup{plan: plan}, WithClock(func() time.Time { return frozen }))
	return g, mock, func() { _ = db.Close() }
}

func TestClassifyPreset(t *testing.T) {
	cases := []struct {
		name    string
		masters []string
		tactics []string
		want    ConsultKind
	}{
		{"single master is quick", []string{"buffett"}, nil, KindQuick},
		{"single tactic is quick", nil, []string{"tail_sniper"}, KindQuick},
		{"two masters is deep", []string{"buffett", "munger"}, nil, KindDeep},
		{"two tactics is deep", nil, []string{"a", "b"}, KindDeep},
		{"mixed pair is deep", []string{"buffett"}, []string{"tail_sniper"}, KindDeep},
		{"empty defaults to quick", nil, nil, KindQuick},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyPreset(advisor.PersonaPreset{}, tc.masters, tc.tactics); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestGate_Check_AllowsUnlimited(t *testing.T) {
	plan := &subscription.Plan{
		Tier:                      subscription.PlanPro,
		AdvisorDeepUnitsPerMonth:  -1,
		AdvisorQuickUnitsPerMonth: -1,
	}
	g, mock, done := freshGate(t, plan)
	defer done()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT deep_units_consumed")).
		WithArgs("u1", "2026-06").
		WillReturnRows(sqlmock.NewRows([]string{"deep_units_consumed"}).AddRow(42))

	dec, err := g.Check(context.Background(), "u1", KindDeep)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if dec.Remaining != -1 {
		t.Errorf("expected unlimited remaining, got %d", dec.Remaining)
	}
}

func TestGate_Check_BlocksOnExceeded(t *testing.T) {
	plan := &subscription.Plan{
		Tier:                      subscription.PlanFree,
		AdvisorDeepUnitsPerMonth:  5,
		AdvisorQuickUnitsPerMonth: 10,
	}
	g, mock, done := freshGate(t, plan)
	defer done()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT deep_units_consumed")).
		WithArgs("u1", "2026-06").
		WillReturnRows(sqlmock.NewRows([]string{"deep_units_consumed"}).AddRow(5))

	_, err := g.Check(context.Background(), "u1", KindDeep)
	if err == nil {
		t.Fatal("expected quota exceeded")
	}
	if !IsQuotaExceeded(err) {
		t.Fatalf("expected QuotaExceededError, got %T", err)
	}
	var q *QuotaExceededError
	_ = errors.As(err, &q)
	if q.UpgradeSuggested != subscription.PlanPro {
		t.Errorf("free should suggest pro, got %s", q.UpgradeSuggested)
	}
}

func TestGate_Check_NoUsageRowYet(t *testing.T) {
	plan := &subscription.Plan{
		Tier:                      subscription.PlanFree,
		AdvisorDeepUnitsPerMonth:  5,
		AdvisorQuickUnitsPerMonth: 15,
	}
	g, mock, done := freshGate(t, plan)
	defer done()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT quick_units_consumed")).
		WithArgs("u1", "2026-06").
		WillReturnRows(sqlmock.NewRows([]string{"quick_units_consumed"}))

	dec, err := g.Check(context.Background(), "u1", KindQuick)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if dec.Used != 0 || dec.Remaining != 15 {
		t.Errorf("expected fresh (0/15), got %d/%d", dec.Used, dec.Remaining)
	}
}

func TestGate_Consume_IncrementsDeep(t *testing.T) {
	plan := &subscription.Plan{
		Tier:                      subscription.PlanFree,
		AdvisorDeepUnitsPerMonth:  5,
		AdvisorQuickUnitsPerMonth: 15,
	}
	g, mock, done := freshGate(t, plan)
	defer done()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO user_advisor_monthly_usage")).
		WithArgs("u1", "2026-06", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"deep_units_consumed", "quick_units_consumed"}).AddRow(3, 0))

	dec, err := g.Consume(context.Background(), "u1", KindDeep, SourcePlan)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if dec.Used != 3 || dec.Remaining != 2 {
		t.Errorf("expected used=3 remaining=2, got %+v", dec)
	}
	if dec.Source != SourcePlan {
		t.Errorf("expected source=plan, got %s", dec.Source)
	}
}

func TestGate_Consume_UnlimitedRemainsUnlimited(t *testing.T) {
	plan := &subscription.Plan{
		Tier:                      subscription.PlanPro,
		AdvisorDeepUnitsPerMonth:  100,
		AdvisorQuickUnitsPerMonth: -1,
	}
	g, mock, done := freshGate(t, plan)
	defer done()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO user_advisor_monthly_usage")).
		WithArgs("u1", "2026-06", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"deep_units_consumed", "quick_units_consumed"}).AddRow(7, 99))

	dec, err := g.Consume(context.Background(), "u1", KindQuick, SourcePlan)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if dec.Remaining != -1 {
		t.Errorf("expected unlimited remaining, got %d", dec.Remaining)
	}
}

func TestGate_Summary_PopulatesAllFields(t *testing.T) {
	plan := &subscription.Plan{
		Tier:                      subscription.PlanFree,
		AdvisorDeepUnitsPerMonth:  5,
		AdvisorQuickUnitsPerMonth: 15,
		AllowAdvisorBYOK:          false,
	}
	g, mock, done := freshGate(t, plan)
	defer done()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT deep_units_consumed, quick_units_consumed")).
		WithArgs("u1", "2026-06").
		WillReturnRows(sqlmock.NewRows([]string{"deep_units_consumed", "quick_units_consumed"}).AddRow(5, 7))

	sum, err := g.Summary(context.Background(), "u1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if sum.DeepRemaining != 0 || sum.QuickRemaining != 8 {
		t.Errorf("bad remaining: %+v", sum)
	}
	if sum.UpgradeSuggested != subscription.PlanPro {
		t.Errorf("expected pro upgrade, got %s", sum.UpgradeSuggested)
	}
	if sum.NextResetAt.Day() != 1 || sum.NextResetAt.Month() != 7 {
		t.Errorf("expected next reset 2026-07-01, got %s", sum.NextResetAt)
	}
}

func TestGate_ErrUnconfigured(t *testing.T) {
	var g *Gate
	if _, err := g.Check(context.Background(), "u1", KindDeep); !errors.Is(err, ErrUnconfigured) {
		t.Errorf("expected ErrUnconfigured, got %v", err)
	}
}

// TestGate_Check_FallsThroughToCredits verifies the Phase C cascade:
// when plan-included monthly units are exhausted but the user has
// a credit balance, Check should admit + return Source=credit.
func TestGate_Check_FallsThroughToCredits(t *testing.T) {
	plan := &subscription.Plan{
		Tier:                      subscription.PlanFree,
		AdvisorDeepUnitsPerMonth:  5,
		AdvisorQuickUnitsPerMonth: 15,
	}
	g, mock, done := freshGate(t, plan)
	defer done()
	// Attach credits repo using same sqlmock db. The gate doesn't
	// hold a separate handle.
	creditsRepo := NewCreditsRepo(g.db)
	g.credits = creditsRepo

	mock.ExpectQuery(regexp.QuoteMeta("SELECT deep_units_consumed")).
		WithArgs("u1", "2026-06").
		WillReturnRows(sqlmock.NewRows([]string{"deep_units_consumed"}).AddRow(5))
	mock.ExpectQuery(regexp.QuoteMeta("FROM user_advisor_credits")).
		WithArgs("u1").
		WillReturnRows(sqlmock.NewRows([]string{
			"deep_units_balance", "quick_units_balance", "total_purchased_cents",
			"last_purchase_at", "last_consumption_at", "updated_at",
		}).AddRow(30, 0, 1900, nil, nil, time.Now()))

	dec, err := g.Check(context.Background(), "u1", KindDeep)
	if err != nil {
		t.Fatalf("expected admit via credits, got err: %v", err)
	}
	if dec.Source != SourceCredit {
		t.Errorf("expected source=credit, got %s", dec.Source)
	}
	if dec.CreditBalance != 30 || dec.Remaining != 30 {
		t.Errorf("expected credit balance 30, got %+v", dec)
	}
}

// TestGate_Check_QuotaExceededWhenBothEmpty — the cascade returns
// QuotaExceeded only when BOTH plan and credit buckets are dry.
func TestGate_Check_QuotaExceededWhenBothEmpty(t *testing.T) {
	plan := &subscription.Plan{
		Tier:                     subscription.PlanFree,
		AdvisorDeepUnitsPerMonth: 5,
	}
	g, mock, done := freshGate(t, plan)
	defer done()
	g.credits = NewCreditsRepo(g.db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT deep_units_consumed")).
		WithArgs("u1", "2026-06").
		WillReturnRows(sqlmock.NewRows([]string{"deep_units_consumed"}).AddRow(5))
	mock.ExpectQuery(regexp.QuoteMeta("FROM user_advisor_credits")).
		WithArgs("u1").
		WillReturnRows(sqlmock.NewRows([]string{
			"deep_units_balance", "quick_units_balance", "total_purchased_cents",
			"last_purchase_at", "last_consumption_at", "updated_at",
		}).AddRow(0, 0, 0, nil, nil, time.Now()))

	_, err := g.Check(context.Background(), "u1", KindDeep)
	if !IsQuotaExceeded(err) {
		t.Errorf("expected QuotaExceeded, got %v", err)
	}
}
