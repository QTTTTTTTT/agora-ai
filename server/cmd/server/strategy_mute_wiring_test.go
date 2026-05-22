package main

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/attribution"
	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// loadMutedSleeveRegimes — reads attribution lessons from memories
// and folds them into a strategy.SleeveRegimeMute slice.
// ---------------------------------------------------------------------------

func TestLoadMutedSleeveRegimesNilReceiver(t *testing.T) {
	var a *runtimePMAgent
	if got := a.loadMutedSleeveRegimes(context.Background(), "fund-x"); got != nil {
		t.Fatalf("nil receiver should return nil, got %+v", got)
	}
}

func TestLoadMutedSleeveRegimesNoMemoryRepo(t *testing.T) {
	a := &runtimePMAgent{}
	if got := a.loadMutedSleeveRegimes(context.Background(), "fund-x"); got != nil {
		t.Fatalf("missing memoryRepo should return nil, got %+v", got)
	}
}

func TestLoadMutedSleeveRegimesIgnoresWinnerAndInsufficient(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags, created_at, updated_at`)).
		WithArgs("fund-1", attribution.MemoryLayer, 100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "owner_user_id", "visibility", "sensitivity", "origin_kind", "source_listing_id", "layer", "title", "content", "trading_date", "tags", "created_at", "updated_at"}).
			AddRow("mem-loser", "fund-1", sql.NullString{}, sql.NullString{}, "private", "internal", "native", sql.NullString{}, attribution.MemoryLayer, sql.NullString{String: "loser title", Valid: true}, "body", sql.NullTime{Time: now, Valid: true}, "{loser,sleeve:trend,regime:chop}", now, now).
			AddRow("mem-winner", "fund-1", sql.NullString{}, sql.NullString{}, "private", "internal", "native", sql.NullString{}, attribution.MemoryLayer, sql.NullString{String: "winner title", Valid: true}, "body", sql.NullTime{Time: now, Valid: true}, "{winner,sleeve:trend,regime:trend_up}", now, now).
			AddRow("mem-info", "fund-1", sql.NullString{}, sql.NullString{}, "private", "internal", "native", sql.NullString{}, attribution.MemoryLayer, sql.NullString{String: "no data", Valid: true}, "body", sql.NullTime{Time: now, Valid: true}, "{insufficient_data}", now, now))

	a := &runtimePMAgent{memoryRepo: repository.NewMemoryRepo(db)}
	got := a.loadMutedSleeveRegimes(context.Background(), "fund-1")
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 mute (only loser), got %d: %+v", len(got), got)
	}
	if got[0].Sleeve != "trend" || got[0].Regime != "chop" {
		t.Fatalf("loser mute mismatched: got %+v", got[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLoadMutedSleeveRegimesDeduplicatesAcrossDays(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	yesterday := now.Add(-24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags, created_at, updated_at`)).
		WithArgs("fund-1", attribution.MemoryLayer, 100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "owner_user_id", "visibility", "sensitivity", "origin_kind", "source_listing_id", "layer", "title", "content", "trading_date", "tags", "created_at", "updated_at"}).
			AddRow("a", "fund-1", sql.NullString{}, sql.NullString{}, "private", "internal", "native", sql.NullString{}, attribution.MemoryLayer, sql.NullString{String: "loser today", Valid: true}, "body", sql.NullTime{Time: now, Valid: true}, "{loser,sleeve:trend,regime:chop}", now, now).
			AddRow("b", "fund-1", sql.NullString{}, sql.NullString{}, "private", "internal", "native", sql.NullString{}, attribution.MemoryLayer, sql.NullString{String: "loser yesterday", Valid: true}, "body", sql.NullTime{Time: yesterday, Valid: true}, "{loser,sleeve:trend,regime:chop}", yesterday, yesterday))

	a := &runtimePMAgent{memoryRepo: repository.NewMemoryRepo(db)}
	got := a.loadMutedSleeveRegimes(context.Background(), "fund-1")
	if len(got) != 1 {
		t.Fatalf("expected deduped to 1 mute, got %d: %+v", len(got), got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLoadMutedSleeveRegimesSkipsMissingSleeveOrRegimeTag(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags, created_at, updated_at`)).
		WithArgs("fund-1", attribution.MemoryLayer, 100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "owner_user_id", "visibility", "sensitivity", "origin_kind", "source_listing_id", "layer", "title", "content", "trading_date", "tags", "created_at", "updated_at"}).
			AddRow("bad", "fund-1", sql.NullString{}, sql.NullString{}, "private", "internal", "native", sql.NullString{}, attribution.MemoryLayer, sql.NullString{String: "missing regime", Valid: true}, "body", sql.NullTime{Time: now, Valid: true}, "{loser,sleeve:trend}", now, now).
			AddRow("unspec", "fund-1", sql.NullString{}, sql.NullString{}, "private", "internal", "native", sql.NullString{}, attribution.MemoryLayer, sql.NullString{String: "unspec sleeve", Valid: true}, "body", sql.NullTime{Time: now, Valid: true}, "{loser,sleeve:(unspecified),regime:chop}", now, now))

	a := &runtimePMAgent{memoryRepo: repository.NewMemoryRepo(db)}
	got := a.loadMutedSleeveRegimes(context.Background(), "fund-1")
	if len(got) != 0 {
		t.Fatalf("malformed tag sets should produce no mutes, got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// isLoserLesson + extractSleeveRegimeTags — pure helpers
// ---------------------------------------------------------------------------

func TestIsLoserLessonDetectsTag(t *testing.T) {
	if !isLoserLesson([]string{"loser", "sleeve:trend", "regime:chop"}) {
		t.Fatal("loser tag should be detected")
	}
	if isLoserLesson([]string{"winner", "sleeve:trend"}) {
		t.Fatal("winner tag should not count as loser")
	}
	if isLoserLesson(nil) {
		t.Fatal("nil tags should not count")
	}
}

func TestExtractSleeveRegimeTagsHappyPath(t *testing.T) {
	s, r, ok := extractSleeveRegimeTags([]string{"loser", "sleeve:trend", "regime:chop"})
	if !ok || s != "trend" || r != "chop" {
		t.Fatalf("got (%q, %q, %v)", s, r, ok)
	}
}

func TestExtractSleeveRegimeTagsRejectsUnspecified(t *testing.T) {
	_, _, ok := extractSleeveRegimeTags([]string{"loser", "sleeve:(unspecified)", "regime:chop"})
	if ok {
		t.Fatal("(unspecified) sleeve should not produce a mute")
	}
}
