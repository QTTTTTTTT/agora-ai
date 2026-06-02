package main

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/wsfeed"
	wsfeedprovider "github.com/fundai/server/internal/wsfeed/provider"
)

// expectHeldSymbolsQuery wires up the sqlmock expectation for
// the bridge's heldSymbols query.
func expectHeldSymbolsQuery(mock sqlmock.Sqlmock, rows [][]string) {
	r := sqlmock.NewRows([]string{"sym_key", "symbol", "market"})
	for _, row := range rows {
		r.AddRow(row[0], row[1], row[2])
	}
	mock.ExpectQuery("FROM holding_positions").
		WillReturnRows(r)
}

func TestWSFeedBridgeReconcileAddsHeldSymbols(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mgr := wsfeed.NewManager(wsfeed.ManagerConfig{})
	prov := wsfeedprovider.NewMock("mock")
	if err := mgr.AddProvider(prov); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()
	for i := 0; i < 100 && prov.State() != wsfeed.StateConnected; i++ {
		time.Sleep(2 * time.Millisecond)
	}

	bridge := newWSFeedSubscriptionBridge(db, mgr, wsFeedConfig{
		SubscriptionReconcileInterval: time.Minute,
	}, newFakeWSMetrics())

	expectHeldSymbolsQuery(mock, [][]string{
		{"AAPL", "AAPL", "US"},
		{"MSFT", "MSFT", "US"},
	})
	bridge.reconcileOnce(context.Background())

	syms := prov.SubscribedSymbols()
	if len(syms) != 2 {
		t.Fatalf("expected 2 subs, got %d (%v)", len(syms), syms)
	}
}

func TestWSFeedBridgeReconcileRemovesNoLongerHeld(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mgr := wsfeed.NewManager(wsfeed.ManagerConfig{})
	prov := wsfeedprovider.NewMock("mock")
	if err := mgr.AddProvider(prov); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()
	for i := 0; i < 100 && prov.State() != wsfeed.StateConnected; i++ {
		time.Sleep(2 * time.Millisecond)
	}

	bridge := newWSFeedSubscriptionBridge(db, mgr, wsFeedConfig{
		SubscriptionReconcileInterval: time.Minute,
	}, newFakeWSMetrics())

	expectHeldSymbolsQuery(mock, [][]string{{"AAPL", "AAPL", "US"}, {"MSFT", "MSFT", "US"}})
	bridge.reconcileOnce(context.Background())
	if got := len(prov.SubscribedSymbols()); got != 2 {
		t.Fatalf("after first reconcile got %d subs", got)
	}

	// Second reconcile: MSFT no longer held → should unsubscribe.
	expectHeldSymbolsQuery(mock, [][]string{{"AAPL", "AAPL", "US"}})
	bridge.reconcileOnce(context.Background())
	syms := prov.SubscribedSymbols()
	if len(syms) != 1 || syms[0] != "AAPL" {
		t.Fatalf("after MSFT removal expected [AAPL], got %v", syms)
	}
}

func TestWSFeedBridgeReconcileSurvivesQueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mgr := wsfeed.NewManager(wsfeed.ManagerConfig{})
	prov := wsfeedprovider.NewMock("mock")
	_ = mgr.AddProvider(prov)
	_ = mgr.Start(context.Background())
	defer mgr.Stop()
	for i := 0; i < 100 && prov.State() != wsfeed.StateConnected; i++ {
		time.Sleep(2 * time.Millisecond)
	}

	metrics := newFakeWSMetrics()
	bridge := newWSFeedSubscriptionBridge(db, mgr, wsFeedConfig{
		SubscriptionReconcileInterval: time.Minute,
	}, metrics)

	mock.ExpectQuery("FROM holding_positions").
		WillReturnError(context.DeadlineExceeded)
	bridge.reconcileOnce(context.Background())

	if metrics.events["reconcile_query_err"] != 1 {
		t.Fatalf("expected reconcile_query_err metric, got %+v", metrics.events)
	}
	if len(prov.SubscribedSymbols()) != 0 {
		t.Fatalf("no subscriptions should have been added on query error")
	}
}
