package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	lineagegraph "github.com/fundai/server/internal/lineage"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/subscription"
)

type marketplaceSnapshot struct {
	Agent       marketplaceSnapshotAgent         `json:"agent"`
	Learning    *marketplaceSnapshotLearning     `json:"learning,omitempty"`
	ModelConfig *api.UserModelConfig             `json:"modelConfig,omitempty"`
	Memories    []marketplaceSnapshotMemoryEntry `json:"memories,omitempty"`
}

type marketplaceSnapshotAgent struct {
	Name            string          `json:"name"`
	Role            string          `json:"role"`
	Focus           string          `json:"focus,omitempty"`
	SystemPrompt    string          `json:"systemPrompt,omitempty"`
	SkillConfig     json.RawMessage `json:"skillConfig,omitempty"`
	DomainConfig    json.RawMessage `json:"domainConfig,omitempty"`
	EvolutionConfig json.RawMessage `json:"evolutionConfig,omitempty"`
}

type marketplaceSnapshotLearning struct {
	Summary     string    `json:"summary,omitempty"`
	CreatedAt   time.Time `json:"createdAt,omitempty"`
	DailyReturn *float64  `json:"dailyReturn,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
}

type marketplaceSnapshotMemoryEntry struct {
	Layer       string    `json:"layer"`
	Title       string    `json:"title,omitempty"`
	Content     string    `json:"content"`
	TradingDate string    `json:"tradingDate,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

func (s *marketplaceServiceAdapter) ListListings(userID string, limit, offset int) ([]api.MarketplaceListing, error) {
	listings, err := s.marketplaceRepo.ListActiveListings(context.Background(), limit, offset)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	result := make([]api.MarketplaceListing, 0, len(listings))
	for i := range listings {
		result = append(result, convertMarketplaceListing(&listings[i]))
	}
	return result, nil
}

func (s *marketplaceServiceAdapter) ListMyListings(userID string, limit, offset int) ([]api.MarketplaceListing, error) {
	listings, err := s.marketplaceRepo.ListListingsBySeller(context.Background(), userID, limit, offset)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	result := make([]api.MarketplaceListing, 0, len(listings))
	for i := range listings {
		result = append(result, convertMarketplaceListing(&listings[i]))
	}
	return result, nil
}

func (s *marketplaceServiceAdapter) CreateListing(userID string, input api.CreateMarketplaceListingInput) (*api.MarketplaceListing, error) {
	if input.AskPriceMinor <= 0 {
		return nil, api.ErrBadInput
	}
	fund, err := authorizeFundAccess(context.Background(), s.fundRepo, s.companyRepo, userID, input.FundID)
	if err != nil {
		return nil, err
	}
	member, err := s.teamRepo.GetMember(context.Background(), fund.ID, strings.TrimSpace(input.AgentID))
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	agent, err := s.agentRepo.GetByID(context.Background(), member.AgentID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	snapshotPayload, latestSummary, err := s.buildSnapshotPayload(context.Background(), userID, fund.ID, member, agent)
	if err != nil {
		return nil, err
	}
	listing, err := s.marketplaceRepo.CreateListing(context.Background(), repository.CreateAgentMarketListingParams{
		SellerUserID:          userID,
		SourceFundID:          fund.ID,
		SourceAgentID:         member.AgentID,
		AgentName:             agent.Name,
		AgentRole:             agent.Role,
		AgentFocus:            member.Focus.String,
		LatestLearningSummary: latestSummary,
		AskPriceMinor:         input.AskPriceMinor,
		Currency:              input.Currency,
		SnapshotPayload:       snapshotPayload,
	})
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	converted := convertMarketplaceListing(listing)
	return &converted, nil
}

func (s *marketplaceServiceAdapter) CancelListing(userID, listingID string) error {
	listing, err := s.marketplaceRepo.GetListingByID(context.Background(), listingID)
	if err != nil {
		return mapRepositoryError(err)
	}
	if strings.TrimSpace(listing.SellerUserID) != strings.TrimSpace(userID) {
		return api.ErrForbidden
	}
	return mapRepositoryError(s.marketplaceRepo.CancelListing(context.Background(), listingID, userID))
}

func (s *marketplaceServiceAdapter) ListBids(userID, listingID string, limit, offset int) ([]api.MarketplaceBid, error) {
	listing, err := s.marketplaceRepo.GetListingByID(context.Background(), listingID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if strings.TrimSpace(listing.SellerUserID) != strings.TrimSpace(userID) {
		return nil, api.ErrForbidden
	}
	bids, err := s.marketplaceRepo.ListBidsByListing(context.Background(), listingID, limit, offset)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	result := make([]api.MarketplaceBid, 0, len(bids))
	for i := range bids {
		result = append(result, convertMarketplaceBid(&bids[i]))
	}
	return result, nil
}

func (s *marketplaceServiceAdapter) CreateBid(userID string, input api.CreateMarketplaceBidInput) (*api.MarketplaceBid, error) {
	if input.BidPriceMinor <= 0 {
		return nil, api.ErrBadInput
	}
	listing, err := s.marketplaceRepo.GetListingByID(context.Background(), input.ListingID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if listing.Status != "active" {
		return nil, api.ErrConflict
	}
	if strings.TrimSpace(listing.SellerUserID) == strings.TrimSpace(userID) {
		return nil, api.ErrForbidden
	}
	bid, err := s.marketplaceRepo.CreateBid(context.Background(), repository.CreateAgentMarketBidParams{
		ListingID:     input.ListingID,
		BidderUserID:  userID,
		BidPriceMinor: input.BidPriceMinor,
		Currency:      firstNonEmptyValue(input.Currency, listing.Currency),
	})
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	converted := convertMarketplaceBid(bid)
	return &converted, nil
}

func (s *marketplaceServiceAdapter) PurchaseListing(userID string, input api.PurchaseMarketplaceListingInput) (*api.MarketplaceOrder, error) {
	ctx := context.Background()

	// Cheap pre-flight: read the listing without locking just to validate
	// the request and resolve the buyer fund (which involves multiple
	// reads). The authoritative re-check happens under FOR UPDATE inside
	// the transaction below.
	listingPreview, err := s.marketplaceRepo.GetListingByID(ctx, input.ListingID)
	if err != nil {
		return nil, mapRepositoryError(err)
	}
	if listingPreview.Status != "active" {
		return nil, api.ErrConflict
	}
	if strings.TrimSpace(listingPreview.SellerUserID) == strings.TrimSpace(userID) {
		return nil, api.ErrForbidden
	}

	bindAdapter := &teamServiceAdapter{
		agentRepo:           s.agentRepo,
		teamRepo:            s.teamRepo,
		memoryRepo:          s.memoryRepo,
		subscriptionService: s.subscriptionService,
	}

	buyerFundID := ""
	if trimmedFundID := strings.TrimSpace(input.BuyerFundID); trimmedFundID != "" {
		buyerFund, err := authorizeFundAccess(ctx, s.fundRepo, s.companyRepo, userID, trimmedFundID)
		if err != nil {
			return nil, err
		}
		if err := bindAdapter.checkFundAgentQuota(ctx, userID, buyerFund.ID); err != nil {
			return nil, err
		}
		buyerFundID = buyerFund.ID
	}

	// Idempotency key: deterministic per (buyer, listing). Retried calls
	// from the same buyer for the same listing land on the same pending
	// order and the same wallet ledger entries — no double-charge.
	idemKey := buildPurchaseIdempotencyKey(userID, listingPreview.ID)

	var (
		order            *repository.AgentMarketOrder
		deliveredAgentID string
		clonedSnapshot   marketplaceSnapshot
	)

	txErr := s.uow.WithinTx(ctx, func(tx *sql.Tx) error {
		// Re-read under FOR UPDATE so two concurrent buyers serialise.
		listing, err := s.marketplaceRepo.LockListingForUpdate(ctx, tx, listingPreview.ID)
		if err != nil {
			return mapRepositoryError(err)
		}
		if listing.Status != "active" {
			return api.ErrConflict
		}

		// Reserve the order row first. If a previous attempt already got
		// past this point we short-circuit and return the existing row.
		pending, created, err := s.marketplaceRepo.CreatePendingOrderWithTx(ctx, tx, repository.CreateAgentMarketOrderParams{
			ListingID:      listing.ID,
			SellerUserID:   listing.SellerUserID,
			BuyerUserID:    userID,
			BuyerFundID:    buyerFundID,
			SourceAgentID:  listing.SourceAgentID,
			AmountMinor:    listing.AskPriceMinor,
			Currency:       listing.Currency,
			IdempotencyKey: idemKey,
		})
		if err != nil {
			return mapRepositoryError(err)
		}
		if !created && pending.Status == "completed" {
			// Idempotent replay: the previous call already finished.
			order = pending
			deliveredAgentID = strings.TrimSpace(pending.DeliveredAgentID)
			return errReplayCompleted
		}

		// Clone agent inside the same transaction — if the wallet
		// transfer fails the new agent row rolls back too.
		newAgentID, snapshot, err := s.cloneMarketplaceAgentTx(ctx, tx, userID, listing)
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

		if _, _, _, _, err := s.walletRepo.TransferWithTx(ctx, tx, repository.WalletTransferParams{
			FromUserID:        userID,
			ToUserID:          listing.SellerUserID,
			AmountMinor:       listing.AskPriceMinor,
			Currency:          listing.Currency,
			DebitEntryType:    "marketplace_purchase",
			CreditEntryType:   "marketplace_sale",
			ReferenceType:     "agent_market_order",
			ReferenceID:       pending.ID,
			CreatedByUserID:   userID,
			DebitMetadata:     json.RawMessage(`{"flow":"agent_marketplace_purchase"}`),
			CreditMetadata:    json.RawMessage(`{"flow":"agent_marketplace_sale"}`),
			DebitIdempotency:  idemKey + ":debit",
			CreditIdempotency: idemKey + ":credit",
		}); err != nil {
			if err == repository.ErrInsufficientBalance {
				return api.ErrConflict
			}
			if err == repository.ErrIdempotencyConflict {
				// Another concurrent call already booked the ledger; the
				// pending order will be reconciled — surface as conflict.
				return api.ErrConflict
			}
			return mapRepositoryError(err)
		}

		completed, err := s.marketplaceRepo.CompleteOrderWithTx(ctx, tx, pending.ID, deliveredAgentID)
		if err != nil {
			return mapRepositoryError(err)
		}
		if err := s.marketplaceRepo.MarkListingSoldWithTx(ctx, tx, listing.ID, userID); err != nil {
			return mapRepositoryError(err)
		}
		order = completed
		return nil
	})
	if txErr != nil && txErr != errReplayCompleted {
		return nil, txErr
	}

	// Post-commit best-effort steps. These are intentionally outside the
	// money tx because they call services (subscriptions, llmRuntime) that
	// have their own consistency models. They are idempotent and the
	// reconcile cron flags any divergence.
	if txErr == nil {
		if err := s.applyPostCommitBindings(ctx, userID, buyerFundID, deliveredAgentID, clonedSnapshot, bindAdapter); err != nil {
			// Money has already moved; surface a non-fatal warning by
			// recording a reconcile finding instead of failing the call.
			detail, _ := json.Marshal(map[string]string{"reason": err.Error(), "stage": "post_commit_bindings"})
			_ = s.marketplaceRepo.RecordReconcileFinding(ctx, order.ID, order.ListingID, "post_commit_warning", detail, false)
		}
	}

	converted := convertMarketplaceOrder(order)
	return &converted, nil
}

// errReplayCompleted is an internal sentinel that lets the WithinTx body
// signal "this is a replay — commit nothing new but return the prior
// order". We swallow it after WithinTx returns.
var errReplayCompleted = errorString("marketplace: idempotent replay")

type errorString string

func (e errorString) Error() string { return string(e) }

func buildPurchaseIdempotencyKey(buyerUserID, listingID string) string {
	return "marketplace:purchase:" + strings.TrimSpace(listingID) + ":" + strings.TrimSpace(buyerUserID)
}

func (s *marketplaceServiceAdapter) buildSnapshotPayload(ctx context.Context, userID, fundID string, member *repository.TeamMember, agent *repository.Agent) (json.RawMessage, string, error) {
	memories, err := s.memoryRepo.GetByAgent(ctx, fundID, member.AgentID)
	if err != nil {
		return nil, "", mapRepositoryError(err)
	}
	snapshot := marketplaceSnapshot{
		Agent: marketplaceSnapshotAgent{
			Name:            strings.TrimSpace(agent.Name),
			Role:            strings.TrimSpace(agent.Role),
			Focus:           strings.TrimSpace(member.Focus.String),
			SystemPrompt:    strings.TrimSpace(agent.SystemPrompt.String),
			SkillConfig:     cloneRawJSON(agent.SkillConfig, `{}`),
			DomainConfig:    cloneRawJSON(agent.DomainConfig, `{}`),
			EvolutionConfig: cloneRawJSON(agent.EvolutionConfig, `{}`),
		},
		Memories: make([]marketplaceSnapshotMemoryEntry, 0, len(memories)),
	}
	latest := latestLearningMemory(memories)
	latestSummary := ""
	if latest != nil {
		latestSummary = extractLearningSummary(latest.Content)
		snapshot.Learning = &marketplaceSnapshotLearning{
			Summary:   latestSummary,
			CreatedAt: latest.CreatedAt,
			Tags:      append([]string(nil), latest.Tags...),
		}
		if dailyReturn, ok := extractLearningDailyReturn(latest.Content); ok {
			snapshot.Learning.DailyReturn = &dailyReturn
		}
	}
	for _, memory := range memories {
		// Only include memories explicitly marked for marketplace visibility and public sensitivity
		if memory.Visibility != "marketplace" || memory.Sensitivity != "public" {
			continue
		}
		entry := marketplaceSnapshotMemoryEntry{
			Layer:     memory.Layer,
			Title:     strings.TrimSpace(memory.Title.String),
			Content:   strings.TrimSpace(memory.Content),
			Tags:      append([]string(nil), memory.Tags...),
			CreatedAt: memory.CreatedAt,
		}
		if memory.TradingDate.Valid {
			entry.TradingDate = memory.TradingDate.Time.Format("2006-01-02")
		}
		snapshot.Memories = append(snapshot.Memories, entry)
	}
	if s.modelConfigs != nil {
		configs, err := s.modelConfigs.GetUserConfigs(ctx, userID)
		if err == nil {
			for _, cfg := range configs {
				if cfg == nil || !cfg.IsActive || cfg.ConfigType != "agent_default" || cfg.AgentID == nil {
					continue
				}
				if strings.TrimSpace(*cfg.AgentID) != strings.TrimSpace(member.AgentID) {
					continue
				}
				snapshot.ModelConfig = &api.UserModelConfig{
					ID:              cfg.ID,
					UserID:          cfg.UserID,
					AgentID:         cfg.AgentID,
					ConfigType:      cfg.ConfigType,
					Tier:            cfg.Tier,
					Provider:        cfg.Provider,
					ModelName:       cfg.ModelName,
					BaseURL:         cfg.BaseURL,
					APIKeyEncrypted: cfg.APIKeyEncrypted,
					IsActive:        cfg.IsActive,
					CreatedAt:       cfg.CreatedAt,
					UpdatedAt:       cfg.UpdatedAt,
				}
				break
			}
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, "", err
	}
	return encoded, latestSummary, nil
}

// cloneMarketplaceAgentTx inserts the cloned agent inside the caller's
// transaction and returns the new agent id together with the parsed
// snapshot. The snapshot is reused by the post-commit step that wires up
// the model config and team membership.
func (s *marketplaceServiceAdapter) cloneMarketplaceAgentTx(ctx context.Context, tx *sql.Tx, buyerUserID string, listing *repository.AgentMarketListing) (string, marketplaceSnapshot, error) {
	var snapshot marketplaceSnapshot
	if err := json.Unmarshal(listing.SnapshotPayload, &snapshot); err != nil {
		return "", snapshot, api.ErrBadInput
	}
	createdID, err := s.agentRepo.CreateWithTx(ctx, tx, &repository.Agent{
		UserID:                     strings.TrimSpace(buyerUserID),
		Name:                       firstNonEmptyValue(snapshot.Agent.Name, listing.AgentName),
		Role:                       firstNonEmptyValue(snapshot.Agent.Role, listing.AgentRole),
		Focus:                      nullString(firstNonEmptyValue(snapshot.Agent.Focus, listing.AgentFocus.String)),
		LLMModel:                   sql.NullString{},
		ModelProvider:              sql.NullString{},
		ModelName:                  sql.NullString{},
		SystemPrompt:               nullString(snapshot.Agent.SystemPrompt),
		SkillConfig:                cloneRawJSON(snapshot.Agent.SkillConfig, `{}`),
		DomainConfig:               cloneRawJSON(snapshot.Agent.DomainConfig, `{}`),
		EvolutionConfig:            cloneRawJSON(snapshot.Agent.EvolutionConfig, `{}`),
		PendingMarketplaceSnapshot: cloneRawJSON(listing.SnapshotPayload, `{}`),
		Status:                     "active",
	})
	if err != nil {
		return "", snapshot, mapRepositoryError(err)
	}
	return createdID, snapshot, nil
}

// applyPostCommitBindings runs the steps that depend on services with
// their own consistency boundaries (subscription, llmRuntime, team
// membership). They are idempotent on the (userID, agentID) tuple, so a
// retry by the reconcile cron will converge.
func (s *marketplaceServiceAdapter) applyPostCommitBindings(ctx context.Context, buyerUserID, buyerFundID, agentID string, snapshot marketplaceSnapshot, bindAdapter *teamServiceAdapter) error {
	if strings.TrimSpace(agentID) == "" {
		return nil
	}
	if snapshot.ModelConfig != nil && s.modelConfigs != nil {
		cfg := *snapshot.ModelConfig
		cfg.ID = ""
		cfg.UserID = buyerUserID
		cfg.AgentID = optionalString(agentID)
		cfg.CreatedAt = time.Time{}
		cfg.UpdatedAt = time.Time{}
		if err := s.modelConfigs.SaveConfig(ctx, &subscription.UserModelConfig{
			ID:              cfg.ID,
			UserID:          cfg.UserID,
			AgentID:         cfg.AgentID,
			ConfigType:      cfg.ConfigType,
			Tier:            cfg.Tier,
			Provider:        cfg.Provider,
			ModelName:       cfg.ModelName,
			BaseURL:         cfg.BaseURL,
			APIKeyEncrypted: cfg.APIKeyEncrypted,
			IsActive:        cfg.IsActive,
			CreatedAt:       cfg.CreatedAt,
			UpdatedAt:       cfg.UpdatedAt,
		}); err != nil {
			return err
		}
		if s.llmRuntime != nil {
			if err := s.llmRuntime.SyncUser(ctx, buyerUserID); err != nil {
				return err
			}
		}
	}
	if bindAdapter != nil && strings.TrimSpace(buyerFundID) != "" {
		if _, err := bindAdapter.bindOwnedAgent(ctx, buyerUserID, buyerFundID, agentID); err != nil {
			return err
		}
	}
	return nil
}

func convertMarketplaceListing(listing *repository.AgentMarketListing) api.MarketplaceListing {
	result := api.MarketplaceListing{}
	if listing == nil {
		return result
	}
	result.ID = listing.ID
	result.SellerUserID = listing.SellerUserID
	result.SourceFundID = listing.SourceFundID
	result.SourceAgentID = listing.SourceAgentID
	result.AgentName = listing.AgentName
	result.AgentRole = listing.AgentRole
	result.AgentFocus = listing.AgentFocus.String
	result.LatestLearningSummary = listing.LatestLearningSummary.String
	result.AskPriceMinor = listing.AskPriceMinor
	result.Currency = listing.Currency
	result.Status = listing.Status
	result.SnapshotPayload = cloneRawJSON(listing.SnapshotPayload, `{}`)
	result.Trust = buildMarketplaceTrustSignals(listing)
	result.SoldToUserID = listing.SoldToUserID.String
	if listing.SoldAt.Valid {
		timeValue := listing.SoldAt.Time
		result.SoldAt = &timeValue
	}
	result.CreatedAt = listing.CreatedAt
	result.UpdatedAt = listing.UpdatedAt
	return result
}

func buildMarketplaceTrustSignals(listing *repository.AgentMarketListing) *api.MarketplaceTrustSignals {
	if listing == nil {
		return nil
	}
	var snapshot marketplaceSnapshot
	if len(listing.SnapshotPayload) > 0 && string(listing.SnapshotPayload) != "null" {
		_ = json.Unmarshal(listing.SnapshotPayload, &snapshot)
	}
	trust := &api.MarketplaceTrustSignals{
		LearningRecords:     countSnapshotLearningRecords(snapshot),
		PublicMemoryRecords: len(snapshot.Memories),
		ModelConfigured:     snapshot.ModelConfig != nil,
		ProfileCompleteness: marketplaceProfileCompleteness(listing, snapshot),
		ListingAgeDays:      int(time.Since(listing.CreatedAt).Hours() / 24),
	}
	if snapshot.Learning != nil {
		trust.LastLearningAt = snapshot.Learning.CreatedAt
		trust.LastDailyReturn = snapshot.Learning.DailyReturn
	}
	trust.Score, trust.Badges, trust.Evidence = scoreMarketplaceTrustSignals(trust, listing, snapshot)
	trust.Level = marketplaceTrustLevel(trust.Score)
	return trust
}

func countSnapshotLearningRecords(snapshot marketplaceSnapshot) int {
	count := 0
	if snapshot.Learning != nil && strings.TrimSpace(snapshot.Learning.Summary) != "" {
		count++
	}
	for _, memory := range snapshot.Memories {
		for _, tag := range memory.Tags {
			if strings.EqualFold(strings.TrimSpace(tag), "self_learning") {
				count++
				break
			}
		}
	}
	return count
}

func marketplaceProfileCompleteness(listing *repository.AgentMarketListing, snapshot marketplaceSnapshot) float64 {
	total := 7.0
	complete := 0.0
	if strings.TrimSpace(listing.AgentName) != "" || strings.TrimSpace(snapshot.Agent.Name) != "" {
		complete++
	}
	if strings.TrimSpace(listing.AgentRole) != "" || strings.TrimSpace(snapshot.Agent.Role) != "" {
		complete++
	}
	if strings.TrimSpace(listing.AgentFocus.String) != "" || strings.TrimSpace(snapshot.Agent.Focus) != "" {
		complete++
	}
	if strings.TrimSpace(listing.LatestLearningSummary.String) != "" || (snapshot.Learning != nil && strings.TrimSpace(snapshot.Learning.Summary) != "") {
		complete++
	}
	if strings.TrimSpace(snapshot.Agent.SystemPrompt) != "" {
		complete++
	}
	if len(snapshot.Agent.DomainConfig) > 0 && string(snapshot.Agent.DomainConfig) != "{}" && string(snapshot.Agent.DomainConfig) != "null" {
		complete++
	}
	if len(snapshot.Agent.EvolutionConfig) > 0 && string(snapshot.Agent.EvolutionConfig) != "{}" && string(snapshot.Agent.EvolutionConfig) != "null" {
		complete++
	}
	return complete / total
}

func scoreMarketplaceTrustSignals(trust *api.MarketplaceTrustSignals, listing *repository.AgentMarketListing, snapshot marketplaceSnapshot) (int, []string, []string) {
	score := 20
	badges := []string{}
	evidence := []string{}
	if trust == nil || listing == nil {
		return 0, nil, nil
	}
	if strings.TrimSpace(listing.LatestLearningSummary.String) != "" || (snapshot.Learning != nil && strings.TrimSpace(snapshot.Learning.Summary) != "") {
		score += 20
		badges = append(badges, "learning_summary")
		evidence = append(evidence, "Listing includes a self-learning summary")
	}
	if trust.LearningRecords > 1 {
		score += 10
		badges = append(badges, "learning_history")
		evidence = append(evidence, "Multiple learning records are attached")
	}
	if !trust.LastLearningAt.IsZero() && time.Since(trust.LastLearningAt) <= 30*24*time.Hour {
		score += 15
		badges = append(badges, "recent_learning")
		evidence = append(evidence, "Learning evidence was updated within 30 days")
	}
	if trust.LastDailyReturn != nil {
		score += 10
		badges = append(badges, "return_trace")
		evidence = append(evidence, "Latest learning includes a daily return trace")
	}
	if trust.PublicMemoryRecords > 0 {
		score += 10
		badges = append(badges, "public_memory")
		evidence = append(evidence, "Public marketplace memories are available for inspection")
	}
	if trust.ModelConfigured {
		score += 5
		badges = append(badges, "model_configured")
		evidence = append(evidence, "Agent has an explicit model configuration snapshot")
	}
	if trust.ProfileCompleteness >= 0.7 {
		score += 10
		badges = append(badges, "complete_profile")
		evidence = append(evidence, "Agent profile contains role, focus, prompt/config, and learning context")
	}
	if score > 100 {
		score = 100
	}
	return score, uniqueNonEmpty(badges), uniqueNonEmpty(evidence)
}

func marketplaceTrustLevel(score int) string {
	switch {
	case score >= 80:
		return "high"
	case score >= 55:
		return "medium"
	default:
		return "low"
	}
}

func convertMarketplaceBid(bid *repository.AgentMarketBid) api.MarketplaceBid {
	result := api.MarketplaceBid{}
	if bid == nil {
		return result
	}
	result.ID = bid.ID
	result.ListingID = bid.ListingID
	result.BidderUserID = bid.BidderUserID
	result.BidPriceMinor = bid.BidPriceMinor
	result.Currency = bid.Currency
	result.Status = bid.Status
	result.CreatedAt = bid.CreatedAt
	result.UpdatedAt = bid.UpdatedAt
	return result
}

func convertMarketplaceOrder(order *repository.AgentMarketOrder) api.MarketplaceOrder {
	result := api.MarketplaceOrder{}
	if order == nil {
		return result
	}
	result.ID = order.ID
	result.ListingID = order.ListingID
	result.SellerUserID = order.SellerUserID
	result.BuyerUserID = order.BuyerUserID
	if order.BuyerFundID.Valid {
		result.BuyerFundID = order.BuyerFundID.String
	}
	result.SourceAgentID = order.SourceAgentID
	result.DeliveredAgentID = order.DeliveredAgentID
	result.AmountMinor = order.AmountMinor
	result.Currency = order.Currency
	result.Status = order.Status
	result.CreatedAt = order.CreatedAt
	return result
}

func cloneRawJSON(raw json.RawMessage, fallback string) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(fallback)
	}
	return append(json.RawMessage(nil), raw...)
}

func parseMarketplaceTradingDate(value string) sql.NullTime {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return sql.NullTime{}
	}
	parsed, err := time.Parse("2006-01-02", trimmed)
	if err != nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: parsed, Valid: true}
}
