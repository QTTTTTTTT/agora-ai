// agent_portable_team_provider_test.go — covers the AP10
// wiring of cross-fund agent_portable retrieval.
//
// Two units under test:
//
//   1. agentPortableImportsOptOut(raw): the fund.config flag
//      resolver. Polarity-inverted relative to the other
//      flags in pm_path_feature_flag.go (default-on, explicit-
//      false-to-disable, fail-safe-block on malformed JSON).
//
//   2. agentPortableTeamProvider(fundRepo, teamRepo): the
//      closure passed into alphalesson.ContextOptions.TeamProvider.
//      Tests use sqlmock for both repos so the team-membership
//      filtering, opt-out resolution, and nil-safety paths
//      all get exercised end-to-end without a live DB.

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
)

// TestAgentPortableImportsOptOut_Matrix pins every branch of
// the truth table the resolver doc lists. The polarity is
// inverted (default-on rather than default-off) so each case
// is worth its own row — a refactor that "helpfully"
// normalised this resolver to match the other flags would
// silently flip every fund's import behaviour.
func TestAgentPortableImportsOptOut_Matrix(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantOut bool
	}{
		{
			name:    "nil bytes - pre-AP10 funds default to imports allowed",
			raw:     "",
			wantOut: false,
		},
		{
			name: "empty json object - key absent, imports allowed",
			raw:  `{}`, wantOut: false,
		},
		{
			name: "key explicitly true - imports allowed",
			raw:  `{"allow_agent_portable_imports": true}`, wantOut: false,
		},
		{
			name: "key explicitly false - imports BLOCKED",
			raw:  `{"allow_agent_portable_imports": false}`, wantOut: true,
		},
		{
			name: "malformed json - fail-safe BLOCK",
			raw:  `{"allow_agent_portable_imports":`, wantOut: true,
		},
		{
			name: "key coexists with other flags - other flags ignored",
			raw: `{
				"pm_path_child_splitting": true,
				"futures_cash_ledger_v2": true,
				"allow_agent_portable_imports": false
			}`,
			wantOut: true,
		},
		{
			name: "extra unknown keys do not affect resolution",
			raw: `{
				"some_future_flag": 42,
				"allow_agent_portable_imports": true,
				"another_thing": "ignore me"
			}`,
			wantOut: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentPortableImportsOptOut(json.RawMessage(tc.raw))
			if got != tc.wantOut {
				t.Errorf("agentPortableImportsOptOut(%q) = %v, want %v", tc.raw, got, tc.wantOut)
			}
		})
	}
}

// TestAgentPortableTeamProvider_FiltersInactiveMembers wires
// up sqlmock for both repos and exercises the happy path:
//   - 3 team members: 2 active + 1 inactive
//   - fund.config has the flag set to true (imports allowed)
// Expected: provider returns only the 2 active agents,
// optedOut=false, and no error.
func TestAgentPortableTeamProvider_FiltersInactiveMembers(t *testing.T) {
	teamDB, teamMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer teamDB.Close()
	teamRepo := repository.NewTeamRepo(teamDB)

	fundDB, fundMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer fundDB.Close()
	fundRepo := repository.NewFundRepo(fundDB)

	now := time.Now()
	uuidA := "11111111-1111-1111-1111-111111111111"
	uuidB := "22222222-2222-2222-2222-222222222222"
	uuidC := "33333333-3333-3333-3333-333333333333"

	teamMock.ExpectQuery("FROM fund_team_members").
		WithArgs("fund-A").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at",
		}).
			AddRow("m1", "fund-A", uuidA, "researcher", nil, now, "active", now).
			AddRow("m2", "fund-A", uuidB, "researcher", nil, now, "active", now).
			AddRow("m3", "fund-A", uuidC, "researcher", nil, now, "departed", now))

	// Fund row with the import flag explicitly true. Only the
	// columns that fundRepo.GetByID actually scans need to be
	// supplied; the rest are NULL/default values.
	fundMock.ExpectQuery("FROM funds WHERE id").
		WithArgs("fund-A").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav",
			"status", "config", "created_at", "updated_at",
		}).AddRow("fund-A", "company-1", "Fund A", "", "live",
			1000000.0, 1000000.0, 1000000.0, 1.0, "active",
			[]byte(`{"allow_agent_portable_imports": true}`), now, now))

	provider := agentPortableTeamProvider(fundRepo, teamRepo)
	team, regime, optedOut := provider(context.Background(), "fund-A")

	if len(team) != 2 {
		t.Fatalf("expected 2 active agents, got %d: %v", len(team), team)
	}
	if team[0] != uuidA || team[1] != uuidB {
		t.Errorf("active filter dropped wrong members: got %v", team)
	}
	if regime != "" {
		t.Errorf("regime should be empty in AP10 (deferred), got %q", regime)
	}
	if optedOut {
		t.Errorf("explicit allow=true should not flip optedOut, got %v", optedOut)
	}
	if err := teamMock.ExpectationsWereMet(); err != nil {
		t.Errorf("team mock: %v", err)
	}
	if err := fundMock.ExpectationsWereMet(); err != nil {
		t.Errorf("fund mock: %v", err)
	}
}

// TestAgentPortableTeamProvider_OptOutPropagates exercises the
// other side of the flag matrix: when the fund.config explicitly
// says allow_agent_portable_imports=false, the provider must
// return optedOut=true even with active team members. This is
// the lever a regulated / multi-LP fund flips to refuse
// inheritance regardless of who's on the team.
func TestAgentPortableTeamProvider_OptOutPropagates(t *testing.T) {
	teamDB, teamMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer teamDB.Close()
	teamRepo := repository.NewTeamRepo(teamDB)

	fundDB, fundMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer fundDB.Close()
	fundRepo := repository.NewFundRepo(fundDB)

	now := time.Now()
	uuidA := "11111111-1111-1111-1111-111111111111"

	teamMock.ExpectQuery("FROM fund_team_members").
		WithArgs("fund-A").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at",
		}).AddRow("m1", "fund-A", uuidA, "researcher", nil, now, "active", now))

	fundMock.ExpectQuery("FROM funds WHERE id").
		WithArgs("fund-A").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "company_id", "name", "description", "trading_mode",
			"initial_capital", "current_capital", "total_assets", "nav",
			"status", "config", "created_at", "updated_at",
		}).AddRow("fund-A", "company-1", "Fund A", "", "live",
			1000000.0, 1000000.0, 1000000.0, 1.0, "active",
			[]byte(`{"allow_agent_portable_imports": false}`), now, now))

	provider := agentPortableTeamProvider(fundRepo, teamRepo)
	team, _, optedOut := provider(context.Background(), "fund-A")

	if !optedOut {
		t.Errorf("explicit allow=false should set optedOut=true")
	}
	// Team list is still resolved — the contract is that the
	// caller receives both signals and ListLessons combines
	// them. (Today optedOut=true short-circuits the
	// cross-fund branch in alphalesson regardless of the team
	// list, but if a future caller wanted to use the team
	// list for a different feature we shouldn't hide it.)
	if len(team) != 1 {
		t.Errorf("team list should still be returned, got %d entries", len(team))
	}
}

// TestAgentPortableTeamProvider_NilRepos covers the unit-test
// / smoke-deploy wiring where one or both repos are nil. The
// closure must return empty (no panic) so the legacy
// fund-only retrieval path stays intact.
func TestAgentPortableTeamProvider_NilRepos(t *testing.T) {
	cases := []struct {
		name string
		fund *repository.FundRepo
		team *repository.TeamRepo
	}{
		{name: "both nil", fund: nil, team: nil},
		{name: "fund nil, team set", fund: nil, team: stubTeamRepo(t)},
		{name: "team nil, fund set", fund: stubFundRepo(t), team: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("provider panicked with nil repo(s): %v", r)
				}
			}()
			provider := agentPortableTeamProvider(tc.fund, tc.team)
			team, _, _ := provider(context.Background(), "fund-A")
			// "team nil" path returns immediately. "fund nil
			// + team set" path will issue the team query so
			// we can't assert empty without a deeper mock —
			// covered by the happy-path tests already.
			if tc.team == nil && len(team) != 0 {
				t.Errorf("expected empty team when teamRepo nil, got %v", team)
			}
		})
	}
}

// TestAgentPortableTeamProvider_DBErrorDegrades verifies the
// "DB blip → degrade silently" contract. A team query that
// returns an error should produce an empty team rather than
// propagate (the prompt builder must not abort just because
// one DB roundtrip flickered).
func TestAgentPortableTeamProvider_DBErrorDegrades(t *testing.T) {
	teamDB, teamMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer teamDB.Close()
	teamRepo := repository.NewTeamRepo(teamDB)

	teamMock.ExpectQuery("FROM fund_team_members").
		WithArgs("fund-A").
		WillReturnError(errSyntheticDBBlip)

	provider := agentPortableTeamProvider(nil, teamRepo)
	team, regime, optedOut := provider(context.Background(), "fund-A")
	if len(team) != 0 {
		t.Errorf("DB error should yield empty team, got %v", team)
	}
	if regime != "" || optedOut {
		t.Errorf("DB error path should leave regime/optedOut at zero values, got regime=%q optedOut=%v", regime, optedOut)
	}
}

// stubTeamRepo / stubFundRepo construct a real *repository.TeamRepo /
// *repository.FundRepo over a sqlmock that's not given any
// expectations. Used by the nil-repos matrix above to confirm
// the closure handles missing queries without panicking.
// Calls that actually issue SQL would fail, but the relevant
// nil-safety paths return BEFORE issuing SQL.
func stubTeamRepo(t *testing.T) *repository.TeamRepo {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return repository.NewTeamRepo(db)
}

func stubFundRepo(t *testing.T) *repository.FundRepo {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return repository.NewFundRepo(db)
}

// errSyntheticDBBlip is a sentinel used by the DB-error
// degradation test. Defined here so the assertion is
// crystal-clear about what kind of failure it's simulating.
var errSyntheticDBBlip = &syntheticDBError{msg: "synthetic DB blip for AP10 test"}

type syntheticDBError struct{ msg string }

func (e *syntheticDBError) Error() string { return e.msg }
