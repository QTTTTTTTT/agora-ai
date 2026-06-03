package modelab

import (
	"testing"
	"time"
)

func TestCriteria_FilledDefaults(t *testing.T) {
	c := Criteria{}.FilledDefaults()
	if c.MinStreakDays != 7 {
		t.Fatalf("expected MinStreakDays=7, got %d", c.MinStreakDays)
	}
	if c.MinAgreementPct != 0.75 {
		t.Fatalf("expected MinAgreementPct=0.75, got %f", c.MinAgreementPct)
	}
	if c.MinSampleSize != 20 {
		t.Fatalf("expected MinSampleSize=20, got %d", c.MinSampleSize)
	}
	if c.MaxErrorRate != 0.05 {
		t.Fatalf("expected MaxErrorRate=0.05, got %f", c.MaxErrorRate)
	}
	if c.MaxCostRegressionPct != 0.2 {
		t.Fatalf("expected MaxCostRegressionPct=0.2, got %f", c.MaxCostRegressionPct)
	}
}

// TestTrailingStreak_HappyPath checks that 7 qualifying trailing
// days return a streak of 7.
func TestTrailingStreak_HappyPath(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	armDays := []DayMetric{}
	primaryDays := []DayMetric{}
	// Build 7 consecutive qualifying days.
	for i := 0; i < 7; i++ {
		day := dayBucket(now.Add(-time.Duration(i) * 24 * time.Hour))
		armDays = append(armDays, DayMetric{
			Day:           day,
			ShadowCount:   100,
			ErrorCount:    1,
			TotalCostMicr: 1000,
			Agreement:     0.85,
		})
		primaryDays = append(primaryDays, DayMetric{Day: day, TotalCostMicr: 1000})
	}
	c := Criteria{}.FilledDefaults()
	got := trailingStreak(armDays, primaryDays, c, now)
	if got != 7 {
		t.Fatalf("expected streak=7, got %d", got)
	}
}

// TestTrailingStreak_BreaksOnLowAgreement verifies that one bad
// day in the middle of the window resets the streak.
func TestTrailingStreak_BreaksOnLowAgreement(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	armDays := []DayMetric{}
	primaryDays := []DayMetric{}
	for i := 0; i < 7; i++ {
		day := dayBucket(now.Add(-time.Duration(i) * 24 * time.Hour))
		agreement := 0.85
		if i == 2 {
			// Day i=2 (i.e. 2 days ago) has bad agreement.
			agreement = 0.5
		}
		armDays = append(armDays, DayMetric{
			Day:           day,
			ShadowCount:   100,
			ErrorCount:    1,
			TotalCostMicr: 1000,
			Agreement:     agreement,
		})
		primaryDays = append(primaryDays, DayMetric{Day: day, TotalCostMicr: 1000})
	}
	c := Criteria{}.FilledDefaults()
	got := trailingStreak(armDays, primaryDays, c, now)
	if got != 2 {
		t.Fatalf("expected streak=2 (stops at the bad day), got %d", got)
	}
}

// TestTrailingStreak_NeutralOnSmallSample — a day below
// MinSampleSize is neutral (skipped) rather than breaking the
// streak. Allows weekends with low call volume not to invalidate
// the recommendation.
func TestTrailingStreak_NeutralOnSmallSample(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	armDays := []DayMetric{}
	primaryDays := []DayMetric{}
	for i := 0; i < 7; i++ {
		day := dayBucket(now.Add(-time.Duration(i) * 24 * time.Hour))
		count := 100
		if i == 3 {
			count = 5
		}
		armDays = append(armDays, DayMetric{
			Day:           day,
			ShadowCount:   count,
			ErrorCount:    0,
			TotalCostMicr: 1000,
			Agreement:     0.85,
		})
		primaryDays = append(primaryDays, DayMetric{Day: day, TotalCostMicr: 1000})
	}
	c := Criteria{}.FilledDefaults()
	got := trailingStreak(armDays, primaryDays, c, now)
	if got != 6 {
		t.Fatalf("expected streak=6 (one neutral day skipped), got %d", got)
	}
}

// TestTrailingStreak_CostRegressionBreaks — when the shadow arm is
// >20% more expensive than the primary, the streak terminates on
// that day.
func TestTrailingStreak_CostRegressionBreaks(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	armDays := []DayMetric{}
	primaryDays := []DayMetric{}
	for i := 0; i < 7; i++ {
		day := dayBucket(now.Add(-time.Duration(i) * 24 * time.Hour))
		armCost := int64(1000)
		if i == 4 {
			armCost = 2000 // 100% regression
		}
		armDays = append(armDays, DayMetric{
			Day:           day,
			ShadowCount:   100,
			TotalCostMicr: armCost,
			Agreement:     0.85,
		})
		primaryDays = append(primaryDays, DayMetric{Day: day, TotalCostMicr: 1000})
	}
	c := Criteria{}.FilledDefaults()
	got := trailingStreak(armDays, primaryDays, c, now)
	if got != 4 {
		t.Fatalf("expected streak=4 (stops at cost regression), got %d", got)
	}
}

// TestTrailingStreak_NegativeMaxCostRegression — operator requires
// cost improvement; the shadow must be cheaper.
func TestTrailingStreak_NegativeMaxCostRegression(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	armDays := []DayMetric{}
	primaryDays := []DayMetric{}
	for i := 0; i < 7; i++ {
		day := dayBucket(now.Add(-time.Duration(i) * 24 * time.Hour))
		armDays = append(armDays, DayMetric{
			Day:           day,
			ShadowCount:   100,
			TotalCostMicr: 950, // 5% cheaper
			Agreement:     0.85,
		})
		primaryDays = append(primaryDays, DayMetric{Day: day, TotalCostMicr: 1000})
	}
	c := Criteria{MaxCostRegressionPct: -0.1}.FilledDefaults() // require ≥10% cheaper
	c.MaxCostRegressionPct = -0.1                              // FilledDefaults preserves negatives via != 0 guard
	got := trailingStreak(armDays, primaryDays, c, now)
	if got != 0 {
		t.Fatalf("expected streak=0 (only 5%% cheaper but >=10%% required), got %d", got)
	}
}

func TestDayBucket_TruncatesToUTCDay(t *testing.T) {
	in := time.Date(2026, 6, 3, 23, 45, 12, 0, time.FixedZone("CST", 8*3600))
	got := dayBucket(in)
	if got.Hour() != 0 || got.Minute() != 0 || got.Second() != 0 {
		t.Fatalf("expected midnight UTC, got %v", got)
	}
	// In UTC, 2026-06-03 23:45 CST is 2026-06-03 15:45 UTC, so
	// the bucket is 2026-06-03 (UTC midnight).
	if got.Day() != 3 || got.Month() != 6 || got.Year() != 2026 {
		t.Fatalf("expected 2026-06-03, got %v", got)
	}
}
