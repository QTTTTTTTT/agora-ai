package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fundai/server/internal/api"
)

// AuctionSettlementLeaseName is the scheduler_leases row id used to gate
// the auction settlement loop. Only one server in the pool runs it.
const AuctionSettlementLeaseName = "auction-settlement"

// auctionSettlementLoop drives api.MarketplaceAuctionService.SettleDueAuctions
// on a fixed cadence. Each pass picks listings whose end-time has passed
// and settles them inside their own transactions, so a slow or poison
// auction does not block the rest.
//
// The cadence is intentionally short (5s) because anti-sniping decisions
// have already pushed the close time out before this loop fires — so by
// the time we run, the listing is genuinely done bidding and should
// settle as quickly as possible to release escrow back to losing bidders.
type auctionSettlementLoop struct {
	auctions api.MarketplaceAuctionService
	leader   leaderChecker
	interval time.Duration
	limit    int

	stopCh  chan struct{}
	wg      sync.WaitGroup
	mu      sync.Mutex
	started bool
}

func newAuctionSettlementLoop(auctions api.MarketplaceAuctionService) *auctionSettlementLoop {
	return &auctionSettlementLoop{
		auctions: auctions,
		interval: 5 * time.Second,
		limit:    25,
		stopCh:   make(chan struct{}),
	}
}

func (l *auctionSettlementLoop) SetLeaderChecker(checker leaderChecker) {
	if l == nil {
		return
	}
	l.leader = checker
}

func (l *auctionSettlementLoop) isLeader() bool {
	if l == nil || l.leader == nil {
		return true
	}
	return l.leader.IsLeader(AuctionSettlementLeaseName)
}

func (l *auctionSettlementLoop) Start() {
	if l == nil || l.auctions == nil {
		return
	}
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	if l.stopCh == nil {
		l.stopCh = make(chan struct{})
	}
	stopCh := l.stopCh
	l.started = true
	l.wg.Add(1)
	l.mu.Unlock()

	go func() {
		defer l.wg.Done()
		ticker := time.NewTicker(l.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				if !l.isLeader() {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				results, err := l.auctions.SettleDueAuctions(ctx, time.Now(), l.limit)
				cancel()
				if err != nil {
					slog.Error("auction settlement pass failed", "error", err)
					continue
				}
				if len(results) == 0 {
					continue
				}
				sold := 0
				unsold := 0
				for _, r := range results {
					switch r.Outcome {
					case "sold":
						sold++
					default:
						unsold++
					}
				}
				slog.Info(
					"auction settlement pass",
					"settled", len(results),
					"sold", sold,
					"unsold", unsold,
				)
			}
		}
	}()
}

func (l *auctionSettlementLoop) Stop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.started {
		l.mu.Unlock()
		return
	}
	stopCh := l.stopCh
	l.stopCh = nil
	l.started = false
	l.mu.Unlock()
	close(stopCh)
	l.wg.Wait()
}
