package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	lineagegraph "github.com/fundai/server/internal/lineage"
	"github.com/fundai/server/internal/marketplace"
	"github.com/fundai/server/internal/repository"
)

// English-ascending auctions with wallet-hold escrow + anti-sniping.
//
// State machine (per listing, mode='auction'):
//
//   created          (CreateAuction)
//   ↓
//   active (open)    bids accepted; each new top bid:
//                       1. holds bidder funds
//                       2. releases the prior top bidder's hold
//                       3. updates listing.current_bid + (maybe) ends_at
//   ↓
//   active (closed)  ends_at has passed but no settlement yet — the
//                    settlement worker picks it up
//   ↓
//   sold OR cancelled
//      sold       : reserve met → CaptureHold(winner → seller),
//                   agent clone, listing.status='sold'
//      cancelled  : reserve unmet (or no bids) → release winning hold
//                   (if any), listing.status='cancelled'
//
// Locking: the listing row is taken FOR UPDATE on every bid + on
// settlement, so bids on the same auction serialise even across nodes.

// auctionClock lets tests pin time; production wiring passes time.Now.
type auctionClock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// marketplaceAuctionAdapter implements api.MarketplaceAuctionService. It
// shares dependencies with marketplaceServiceAdapter (so the post-commit
// agent clone reuses the same code path the buyout flow exercises) but
// keeps a dedicated struct because the auction surface has its own
// lifecycle hooks (settlement worker, hold management).
type marketplaceAuctionAdapter struct {
	*marketplaceServiceAdapter

	clock auctionClock
}

func newMarketplaceAuctionAdapter(base *marketplaceServiceAdapter) *marketplaceAuctionAdapter {
	return &marketplaceAuctionAdapter{
		marketplaceServiceAdapter: base,
		clock:                     realClock{},
	}
}

// ---------------------------------------------------------------------------
// CreateAuction
// ---------------------------------------------------------------------------

func (s *marketplaceAuctionAdapter) CreateAuction(userID string, input api.CreateAuctionListingInput) (*api.AuctionListing, error) {
	ctx := context.Background()

	pricing := marketplace.ListingPricing{
		Mode:          marketplace.ModeAuction,
		AskPriceMinor: input.StartingPriceMinor,
		Currency:      input.Currency,
		Auction: &marketplace.AuctionPricing{
			StartsAt:          input.StartsAt,
			EndsAt:            input.EndsAt,
			MinIncrementMinor: input.MinIncrementMinor,
			ReserveMinor:      input.ReserveMinor,
			AntiSnipeSeconds:  input.AntiSnipeSeconds,
		},
	}
	if err := pricing.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %s", api.ErrBadInput, err.Error())
	}

	fund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, input.FundID)
	if err != nil {
		return nil, err
	}
	member, err := s.teamRepo.GetMember(ctx, fund.ID, strings.TrimSpace(input.AgentID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	agent, err := s.agentRepo.GetByID(ctx, member.AgentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	snapshotPayload, latestSummary, err := s.buildSnapshotPayload(ctx, userID, fund.ID, member, agent)
	if err != nil {
		return nil, err
	}

	startsAt := input.StartsAt
	if startsAt.IsZero() {
		// Auctions that don't specify a start time open immediately.
		startsAt = s.clock.Now()
	}
	listing, err := s.marketplaceRepo.CreateAuctionListing(ctx, repository.CreateAgentMarketAuctionParams{
		CreateAgentMarketListingParams: repository.CreateAgentMarketListingParams{
			SellerUserID:          userID,
			SourceFundID:          fund.ID,
			SourceAgentID:         member.AgentID,
			AgentName:             agent.Name,
			AgentRole:             agent.Role,
			AgentFocus:            member.Focus.String,
			LatestLearningSummary: latestSummary,
			AskPriceMinor:         input.StartingPriceMinor,
			Currency:              input.Currency,
			SnapshotPayload:       snapshotPayload,
		},
		StartingPriceMinor: input.StartingPriceMinor,
		StartsAt:           startsAt,
		EndsAt:             input.EndsAt,
		ReserveMinor:       input.ReserveMinor,
		MinIncrementMinor:  input.MinIncrementMinor,
		AntiSnipeSeconds:   input.AntiSnipeSeconds,
	})
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	converted := convertAuctionListing(listing, true)
	return &converted, nil
}

// ---------------------------------------------------------------------------
// PlaceBid
// ---------------------------------------------------------------------------

var (
	errAuctionNotOpen     = errors.New("auction is not open for bids")
	errAuctionEnded       = errors.New("auction has already ended")
	errAuctionBidTooLow   = errors.New("bid amount is below the minimum next bid")
	errAuctionBidder      = errors.New("seller cannot bid on their own auction")
)

func (s *marketplaceAuctionAdapter) PlaceBid(userID string, input api.PlaceAuctionBidInput) (*api.AuctionBid, *api.AuctionListing, error) {
	ctx := context.Background()

	listingID := strings.TrimSpace(input.ListingID)
	if listingID == "" {
		return nil, nil, api.ErrBadInput
	}

	// Fast-path read: confirm the listing exists and the caller isn't the
	// seller before we open a tx and acquire the row lock.
	preview, err := s.marketplaceRepo.GetAuctionByID(ctx, listingID)
	if err != nil {
		return nil, nil, mapRepositoryError(err)
	}
	if strings.TrimSpace(preview.SellerUserID) == strings.TrimSpace(userID) {
		return nil, nil, fmt.Errorf("%w: %s", api.ErrForbidden, errAuctionBidder.Error())
	}

	var (
		insertedBid    *repository.AgentMarketAuctionBid
		updatedListing *repository.AgentMarketAuctionListing
	)
	txErr := s.uow.WithinTx(ctx, func(tx *sql.Tx) error {
		listing, err := s.marketplaceRepo.LockAuctionForUpdate(ctx, tx, listingID)
		if err != nil {
			return mapRepositoryError(err)
		}
		if listing.Status != "active" {
			return fmt.Errorf("%w: %s", api.ErrConflict, errAuctionNotOpen.Error())
		}
		now := s.clock.Now()
		if listing.AuctionStartedAt.Valid && now.Before(listing.AuctionStartedAt.Time) {
			return fmt.Errorf("%w: %s", api.ErrConflict, errAuctionNotOpen.Error())
		}
		if !listing.AuctionEndsAt.Valid || !now.Before(listing.AuctionEndsAt.Time) {
			return fmt.Errorf("%w: %s", api.ErrConflict, errAuctionEnded.Error())
		}

		currentTop := int64(0)
		if listing.AuctionCurrentBidMinor.Valid {
			currentTop = listing.AuctionCurrentBidMinor.Int64
		}
		minNext := marketplace.MinNextBidMinor(listing.AskPriceMinor, currentTop, listing.AuctionMinIncrementMinor)
		if input.BidPriceMinor < minNext {
			return fmt.Errorf("%w: %s (need >= %d)", api.ErrBadInput, errAuctionBidTooLow.Error(), minNext)
		}
		// Same-user re-bid: allowed (raise their own bid) but we still
		// release the previous hold and create a fresh hold so the
		// accounting stays one-active-hold-per-bidder.
		currency := listing.Currency
		if trimmed := strings.TrimSpace(input.Currency); trimmed != "" {
			currency = trimmed
		}

		// Step 1: hold the new bidder's funds before any other state
		// change so an insufficient balance aborts cleanly without
		// touching the listing or refunding the prior bidder.
		holdIdem := fmt.Sprintf("auction:hold:%s:%s:%d:%d", listing.ID, userID, input.BidPriceMinor, now.UnixNano())
		hold, _, err := s.walletRepo.HoldFundsWithTx(ctx, tx, repository.WalletHoldParams{
			UserID:         userID,
			AmountMinor:    input.BidPriceMinor,
			Currency:       currency,
			ReferenceType:  "agent_market_auction",
			ReferenceID:    listing.ID,
			Metadata:       json.RawMessage(`{"flow":"auction_bid_hold"}`),
			IdempotencyKey: holdIdem,
		})
		if err != nil {
			if errors.Is(err, repository.ErrInsufficientBalance) {
				return api.ErrConflict
			}
			return mapRepositoryError(err)
		}

		// Step 2: insert the bid row linked to the hold.
		bid, err := s.marketplaceRepo.CreateAuctionBidWithTx(ctx, tx, repository.CreateAuctionBidWithTxParams{
			ListingID:     listing.ID,
			BidderUserID:  userID,
			BidPriceMinor: input.BidPriceMinor,
			Currency:      currency,
			HoldID:        hold.ID,
		})
		if err != nil {
			return mapRepositoryError(err)
		}

		// Step 3: release the prior top bidder's hold (refund) and mark
		// their bid as outbid. Skip if there was no prior bid.
		if listing.AuctionCurrentBidID.Valid && strings.TrimSpace(listing.AuctionCurrentBidID.String) != "" {
			prevBidID := strings.TrimSpace(listing.AuctionCurrentBidID.String)
			prevBid, err := s.marketplaceRepo.GetAuctionBidByIDWithTx(ctx, tx, prevBidID)
			if err != nil && !errors.Is(err, repository.ErrNotFound) {
				return mapRepositoryError(err)
			}
			if prevBid != nil {
				if prevBid.HoldID.Valid && strings.TrimSpace(prevBid.HoldID.String) != "" {
					if _, _, err := s.walletRepo.ReleaseHoldWithTx(ctx, tx, prevBid.HoldID.String, "outbid"); err != nil {
						// If the previous hold is already released (e.g.
						// reconciled) we treat that as a no-op — the
						// invariant "all losing bidders are refunded by
						// auction close" still holds.
						if !errors.Is(err, repository.ErrHoldNotActive) {
							return mapRepositoryError(err)
						}
					}
				}
				if err := s.marketplaceRepo.MarkBidStatusWithTx(ctx, tx, prevBid.ID, "outbid"); err != nil {
					return mapRepositoryError(err)
				}
			}
		}

		// Step 4: apply anti-sniping and persist the new top bid.
		newEndsAt := marketplace.ApplyAntiSnipe(listing.AuctionEndsAt.Time, now, listing.AuctionAntiSnipeSeconds)
		if err := s.marketplaceRepo.UpdateAuctionTopBidWithTx(ctx, tx, listing.ID, bid.ID, userID, input.BidPriceMinor, newEndsAt); err != nil {
			return mapRepositoryError(err)
		}

		// Refresh the listing snapshot inside the tx so the caller sees
		// the new top bid + (possibly extended) end time.
		refreshed, err := s.marketplaceRepo.LockAuctionForUpdate(ctx, tx, listing.ID)
		if err != nil {
			return mapRepositoryError(err)
		}
		insertedBid = bid
		updatedListing = refreshed
		return nil
	})
	if txErr != nil {
		return nil, nil, txErr
	}

	bidView := convertAuctionBid(insertedBid)
	listingView := convertAuctionListing(updatedListing, false)
	return &bidView, &listingView, nil
}

// ---------------------------------------------------------------------------
// SettleAuction (admin/cron + on-demand)
// ---------------------------------------------------------------------------

func (s *marketplaceAuctionAdapter) SettleAuction(userID, listingID string) (*api.AuctionSettlementResult, error) {
	ctx := context.Background()
	listingID = strings.TrimSpace(listingID)
	if listingID == "" {
		return nil, api.ErrBadInput
	}
	// Authorise: only the seller may force-settle their own auction
	// outside the cron path. Listing must exist + be an auction.
	preview, err := s.marketplaceRepo.GetAuctionByID(ctx, listingID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if strings.TrimSpace(preview.SellerUserID) != strings.TrimSpace(userID) {
		return nil, api.ErrForbidden
	}
	return s.settleAuctionTx(ctx, listingID)
}

// SettleDueAuctions is the cron entry point: it pulls every listing whose
// end_time has passed and settles each in its own transaction so a single
// poison-pill auction can't block the rest.
func (s *marketplaceAuctionAdapter) SettleDueAuctions(ctx context.Context, now time.Time, limit int) ([]api.AuctionSettlementResult, error) {
	due, err := s.marketplaceRepo.ListExpiredActiveAuctions(ctx, now, limit)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	results := make([]api.AuctionSettlementResult, 0, len(due))
	for i := range due {
		result, err := s.settleAuctionTx(ctx, due[i].ID)
		if err != nil {
			// Record the divergence but keep settling the rest.
			detail, _ := json.Marshal(map[string]string{"reason": err.Error(), "stage": "auction_settlement"})
			_ = s.marketplaceRepo.RecordReconcileFinding(ctx, "", due[i].ID, "auction_settlement_failed", detail, false)
			continue
		}
		if result != nil {
			results = append(results, *result)
		}
	}
	return results, nil
}

// settleAuctionTx is the shared body for admin-triggered + cron-triggered
// settlement. It serialises on the listing row, decides between "sold"
// and "reserve_not_met / no_bids", and performs the corresponding wallet
// + agent-clone steps inside one tx.
func (s *marketplaceAuctionAdapter) settleAuctionTx(ctx context.Context, listingID string) (*api.AuctionSettlementResult, error) {
	bindAdapter := &teamServiceAdapter{
		agentRepo:           s.agentRepo,
		teamRepo:            s.teamRepo,
		memoryRepo:          s.memoryRepo,
		subscriptionService: s.subscriptionService,
	}

	var (
		settlementResult *api.AuctionSettlementResult
		deliveredAgentID string
		clonedSnapshot   marketplaceSnapshot
		winnerUserID     string
		winnerBidID      string
		winnerBidAmount  int64
	)

	txErr := s.uow.WithinTx(ctx, func(tx *sql.Tx) error {
		listing, err := s.marketplaceRepo.LockAuctionForUpdate(ctx, tx, listingID)
		if err != nil {
			return mapRepositoryError(err)
		}
		if listing.Status != "active" {
			// Idempotent — already settled. Return a result reflecting
			// the prior outcome so the caller (cron) can carry on.
			settlementResult = &api.AuctionSettlementResult{
				ListingID: listing.ID,
				Outcome:   listing.Status,
			}
			if listing.AuctionWinningBidID.Valid {
				settlementResult.WinningBidID = listing.AuctionWinningBidID.String
			}
			if listing.SoldToUserID.Valid {
				settlementResult.WinnerUserID = listing.SoldToUserID.String
			}
			return nil
		}

		now := s.clock.Now()
		if listing.AuctionEndsAt.Valid && now.Before(listing.AuctionEndsAt.Time) {
			// Not yet due — refuse to early-settle from the cron path.
			// (Admin-triggered SettleAuction calls land here too; we
			// signal conflict so they don't bypass anti-sniping.)
			return fmt.Errorf("%w: auction has not yet ended", api.ErrConflict)
		}

		// Resolve the winning bid + reserve.
		topBidID := ""
		topAmount := int64(0)
		topBidder := ""
		if listing.AuctionCurrentBidID.Valid {
			topBidID = strings.TrimSpace(listing.AuctionCurrentBidID.String)
		}
		if listing.AuctionCurrentBidMinor.Valid {
			topAmount = listing.AuctionCurrentBidMinor.Int64
		}
		if listing.AuctionCurrentBidderUserID.Valid {
			topBidder = strings.TrimSpace(listing.AuctionCurrentBidderUserID.String)
		}
		reserveMet := true
		if listing.AuctionReserveMinor.Valid && listing.AuctionReserveMinor.Int64 > 0 {
			reserveMet = topAmount >= listing.AuctionReserveMinor.Int64
		}

		if topBidID == "" || topBidder == "" || !reserveMet {
			// No winner: release every still-active hold and close the
			// listing as cancelled.
			active, err := s.marketplaceRepo.ListActiveBidsForAuctionWithTx(ctx, tx, listing.ID)
			if err != nil {
				return mapRepositoryError(err)
			}
			for i := range active {
				bid := active[i]
				if bid.HoldID.Valid && strings.TrimSpace(bid.HoldID.String) != "" {
					if _, _, err := s.walletRepo.ReleaseHoldWithTx(ctx, tx, bid.HoldID.String, "auction_closed_unsold"); err != nil {
						if !errors.Is(err, repository.ErrHoldNotActive) {
							return mapRepositoryError(err)
						}
					}
				}
				if err := s.marketplaceRepo.MarkBidStatusWithTx(ctx, tx, bid.ID, "refunded"); err != nil {
					return mapRepositoryError(err)
				}
			}
			if err := s.marketplaceRepo.MarkAuctionClosedUnsoldWithTx(ctx, tx, listing.ID); err != nil {
				return mapRepositoryError(err)
			}
			outcome := "no_bids"
			if topBidID != "" && !reserveMet {
				outcome = "reserve_not_met"
			}
			settlementResult = &api.AuctionSettlementResult{
				ListingID: listing.ID,
				Outcome:   outcome,
			}
			return nil
		}

		// Winner found. Load the winning bid to access its hold.
		winningBid, err := s.marketplaceRepo.GetAuctionBidByIDWithTx(ctx, tx, topBidID)
		if err != nil {
			return mapRepositoryError(err)
		}
		if !winningBid.HoldID.Valid || strings.TrimSpace(winningBid.HoldID.String) == "" {
			// Defensive: a top bid without a hold is a torn-write bug.
			return fmt.Errorf("%w: winning bid is missing wallet hold", api.ErrConflict)
		}

		// Reserve a `pending` order keyed by listing+winner so a retried
		// settlement does not double-clone or double-charge.
		idemKey := fmt.Sprintf("auction:settle:%s:%s", listing.ID, topBidder)
		pending, _, err := s.marketplaceRepo.CreatePendingOrderWithTx(ctx, tx, repository.CreateAgentMarketOrderParams{
			ListingID:      listing.ID,
			SellerUserID:   listing.SellerUserID,
			BuyerUserID:    topBidder,
			BuyerFundID:    "",
			SourceAgentID:  listing.SourceAgentID,
			AmountMinor:    topAmount,
			Currency:       listing.Currency,
			IdempotencyKey: idemKey,
		})
		if err != nil {
			return mapRepositoryError(err)
		}

		// Clone the agent inside the same tx so a failed wallet capture
		// rolls the clone back too.
		newAgentID, snapshot, err := s.cloneMarketplaceAgentTx(ctx, tx, topBidder, &listing.AgentMarketListing)
		if err != nil {
			return err
		}
		deliveredAgentID = newAgentID
		clonedSnapshot = snapshot
		if s.lineageRepo != nil {
			if err := s.lineageRepo.AddEdgeWithTx(ctx, tx, lineagegraph.Edge{ChildAgentID: deliveredAgentID, ParentAgentID: listing.SourceAgentID, Via: lineagegraph.ViaBuyout, SourceListingID: listing.ID}); err != nil {
				return err
			}
		}

		// Capture the winning bidder's hold into a buyer→seller transfer.
		if _, _, _, _, _, err := s.walletRepo.CaptureHoldWithTx(ctx, tx, repository.WalletCaptureParams{
			HoldID:             winningBid.HoldID.String,
			ToUserID:           listing.SellerUserID,
			ReferenceType:      "agent_market_order",
			ReferenceID:        pending.ID,
			DebitEntryType:     "marketplace_purchase",
			CreditEntryType:    "marketplace_sale",
			CreatedByUserID:    topBidder,
			DebitMetadata:      json.RawMessage(`{"flow":"auction_settlement_purchase"}`),
			CreditMetadata:     json.RawMessage(`{"flow":"auction_settlement_sale"}`),
			IdempotencyKeyBase: idemKey,
		}); err != nil {
			return mapRepositoryError(err)
		}

		// Refund every other still-active bidder (defence-in-depth — the
		// PlaceBid path already releases prior holds on each new top
		// bid, but a slow reconcile or a concurrent insert may have
		// left stragglers).
		active, err := s.marketplaceRepo.ListActiveBidsForAuctionWithTx(ctx, tx, listing.ID)
		if err != nil {
			return mapRepositoryError(err)
		}
		for i := range active {
			bid := active[i]
			if bid.ID == winningBid.ID {
				continue
			}
			if bid.HoldID.Valid && strings.TrimSpace(bid.HoldID.String) != "" {
				if _, _, err := s.walletRepo.ReleaseHoldWithTx(ctx, tx, bid.HoldID.String, "auction_settled"); err != nil {
					if !errors.Is(err, repository.ErrHoldNotActive) {
						return mapRepositoryError(err)
					}
				}
			}
			if err := s.marketplaceRepo.MarkBidStatusWithTx(ctx, tx, bid.ID, "refunded"); err != nil {
				return mapRepositoryError(err)
			}
		}

		if err := s.marketplaceRepo.MarkBidStatusWithTx(ctx, tx, winningBid.ID, "won"); err != nil {
			return mapRepositoryError(err)
		}
		completedOrder, err := s.marketplaceRepo.CompleteOrderWithTx(ctx, tx, pending.ID, deliveredAgentID)
		if err != nil {
			return mapRepositoryError(err)
		}
		if err := s.marketplaceRepo.MarkAuctionSettledWithTx(ctx, tx, listing.ID, topBidder, winningBid.ID); err != nil {
			return mapRepositoryError(err)
		}

		orderView := convertMarketplaceOrder(completedOrder)
		settlementResult = &api.AuctionSettlementResult{
			ListingID:     listing.ID,
			Outcome:       "sold",
			WinningBidID:  winningBid.ID,
			WinnerUserID:  topBidder,
			FinalBidMinor: topAmount,
			Order:         &orderView,
		}
		winnerUserID = topBidder
		winnerBidID = winningBid.ID
		winnerBidAmount = topAmount
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}

	// Post-commit bindings (model config + team binding) — best effort.
	if settlementResult != nil && settlementResult.Outcome == "sold" {
		if err := s.applyPostCommitBindings(ctx, winnerUserID, "", deliveredAgentID, clonedSnapshot, bindAdapter); err != nil {
			detail, _ := json.Marshal(map[string]string{
				"reason": err.Error(),
				"stage":  "auction_post_commit_bindings",
			})
			_ = s.marketplaceRepo.RecordReconcileFinding(ctx, "", listingID, "auction_post_commit_warning", detail, false)
		}
	}
	_ = winnerBidID
	_ = winnerBidAmount
	return settlementResult, nil
}

// ---------------------------------------------------------------------------
// ListAuctions / GetAuction (read paths)
// ---------------------------------------------------------------------------

func (s *marketplaceAuctionAdapter) ListAuctions(userID string, limit, offset int) ([]api.AuctionListing, error) {
	ctx := context.Background()
	now := s.clock.Now()
	listings, err := s.marketplaceRepo.ListExpiredActiveAuctions(ctx, now.Add(365*24*time.Hour), limitOrDefault(limit, 50))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	// We pull the full set (active + open) by passing a far-future
	// cutoff. Filter to active so closed auctions don't pollute the list
	// — callers can hit GET /api/marketplace/auctions/{id} for history.
	out := make([]api.AuctionListing, 0, len(listings))
	for i := range listings {
		if listings[i].Status != "active" {
			continue
		}
		isOwner := strings.TrimSpace(listings[i].SellerUserID) == strings.TrimSpace(userID)
		out = append(out, convertAuctionListing(&listings[i], isOwner))
		if len(out) >= limitOrDefault(limit, 50) {
			break
		}
	}
	_ = offset // offset support is not required for the v1 surface; pagination via end time.
	return out, nil
}

func (s *marketplaceAuctionAdapter) GetAuction(userID, listingID string) (*api.AuctionListing, error) {
	listing, err := s.marketplaceRepo.GetAuctionByID(context.Background(), listingID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	isOwner := strings.TrimSpace(listing.SellerUserID) == strings.TrimSpace(userID)
	view := convertAuctionListing(listing, isOwner)
	return &view, nil
}

// ---------------------------------------------------------------------------
// View projections
// ---------------------------------------------------------------------------

func convertAuctionListing(listing *repository.AgentMarketAuctionListing, viewerIsOwner bool) api.AuctionListing {
	if listing == nil {
		return api.AuctionListing{}
	}
	out := api.AuctionListing{
		ID:                    listing.ID,
		SellerUserID:          listing.SellerUserID,
		SourceFundID:          listing.SourceFundID,
		SourceAgentID:         listing.SourceAgentID,
		AgentName:             listing.AgentName,
		AgentRole:             listing.AgentRole,
		AgentFocus:            listing.AgentFocus.String,
		LatestLearningSummary: listing.LatestLearningSummary.String,
		Mode:                  listing.Mode,
		Status:                listing.Status,
		Currency:              listing.Currency,
		StartingPriceMinor:    listing.AskPriceMinor,
		MinIncrementMinor:     listing.AuctionMinIncrementMinor,
		AntiSnipeSeconds:      listing.AuctionAntiSnipeSeconds,
		CreatedAt:             listing.CreatedAt,
		UpdatedAt:             listing.UpdatedAt,
	}
	if listing.AuctionReserveMinor.Valid && viewerIsOwner {
		v := listing.AuctionReserveMinor.Int64
		out.ReserveMinor = &v
	}
	if listing.AuctionCurrentBidMinor.Valid {
		v := listing.AuctionCurrentBidMinor.Int64
		out.CurrentBidMinor = &v
	}
	if listing.AuctionCurrentBidderUserID.Valid && viewerIsOwner {
		out.CurrentBidderUserID = listing.AuctionCurrentBidderUserID.String
	}
	if listing.AuctionCurrentBidID.Valid {
		out.CurrentBidID = listing.AuctionCurrentBidID.String
	}
	currentTop := int64(0)
	if out.CurrentBidMinor != nil {
		currentTop = *out.CurrentBidMinor
	}
	out.MinNextBidMinor = marketplace.MinNextBidMinor(listing.AskPriceMinor, currentTop, listing.AuctionMinIncrementMinor)
	if listing.AuctionStartedAt.Valid {
		t := listing.AuctionStartedAt.Time
		out.StartsAt = &t
	}
	if listing.AuctionEndsAt.Valid {
		t := listing.AuctionEndsAt.Time
		out.EndsAt = &t
	}
	if listing.AuctionSettledAt.Valid {
		t := listing.AuctionSettledAt.Time
		out.SettledAt = &t
	}
	if listing.AuctionWinningBidID.Valid {
		out.WinningBidID = listing.AuctionWinningBidID.String
	}
	if listing.SoldToUserID.Valid {
		out.WinnerUserID = listing.SoldToUserID.String
	}
	// Snapshot redaction: same rules as buyout listing — non-owners get
	// the redacted snapshot, never the seller's prompt/policy.
	out.SnapshotPayload = marketplace.RedactSnapshot(
		listing.SnapshotPayload,
		marketplace.ModeAuction,
		marketplace.SubscriptionPeriod(""),
		viewerIsOwner,
	)
	out.Trust = buildMarketplaceTrustSignals(&listing.AgentMarketListing)
	return out
}

func convertAuctionBid(bid *repository.AgentMarketAuctionBid) api.AuctionBid {
	if bid == nil {
		return api.AuctionBid{}
	}
	return api.AuctionBid{
		ID:            bid.ID,
		ListingID:     bid.ListingID,
		BidderUserID:  bid.BidderUserID,
		BidPriceMinor: bid.BidPriceMinor,
		Currency:      bid.Currency,
		Status:        bid.Status,
		CreatedAt:     bid.CreatedAt,
		UpdatedAt:     bid.UpdatedAt,
	}
}

func limitOrDefault(limit, fallback int) int {
	if limit <= 0 {
		return fallback
	}
	if limit > 500 {
		return 500
	}
	return limit
}
