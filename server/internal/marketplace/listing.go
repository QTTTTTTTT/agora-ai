package marketplace

// Listing modes & snapshot redaction.
//
// This package owns the *domain* model for a marketplace listing, separate
// from the SQL repository (`internal/repository/marketplace_repo.go`) and
// the HTTP adapter (`cmd/server/marketplace_adapter.go`). Two responsibilities
// live here:
//
//   1. ListingMode: an enum + validation that distinguishes "buyout" (one-shot
//      ownership transfer) from "subscribe" (recurring pay-as-you-go access).
//   2. RedactSnapshot: a pure function that strips the seller's prompt /
//      policy / config from a listing snapshot before it is exposed to
//      anyone other than the seller. The legacy adapter returns the full
//      snapshot to every API caller — this is the IP-leak fix.
//
// Both pieces are intentionally side-effect-free so they can be unit-tested
// without a database.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Mode enum
// ---------------------------------------------------------------------------

// ListingMode classifies a listing's commercial structure.
type ListingMode string

const (
	// ModeBuyout transfers ownership of the agent to the buyer in exchange
	// for a single payment. The agent is cloned into the buyer's account
	// and the listing is closed.
	ModeBuyout ListingMode = "buyout"

	// ModeSubscribe gives the subscriber recurring access to the seller's
	// agent through the inference gateway. The agent stays with the seller;
	// the subscriber pays the listing price every period and can invoke
	// inference until they cancel or the period expires.
	ModeSubscribe ListingMode = "subscribe"

	// ModeAuction runs an English-ascending auction. Bidders' funds are
	// held in escrow on each new top bid; outbid bidders are refunded; the
	// winning bidder is settled (capture + agent clone) on close. Closing
	// time can be pushed out by anti-sniping rules — see ListingPricing.
	ModeAuction ListingMode = "auction"
)

// IsValid reports whether the mode is one of the recognised values.
func (m ListingMode) IsValid() bool {
	switch m {
	case ModeBuyout, ModeSubscribe, ModeAuction:
		return true
	}
	return false
}

// SubscriptionPeriod enumerates the supported billing cadences.
type SubscriptionPeriod string

const (
	PeriodDaily   SubscriptionPeriod = "daily"
	PeriodWeekly  SubscriptionPeriod = "weekly"
	PeriodMonthly SubscriptionPeriod = "monthly"
)

// IsValid reports whether the period is recognised.
func (p SubscriptionPeriod) IsValid() bool {
	switch p {
	case PeriodDaily, PeriodWeekly, PeriodMonthly:
		return true
	}
	return false
}

// Duration returns the wall-clock length of one period.
func (p SubscriptionPeriod) Duration() time.Duration {
	switch p {
	case PeriodDaily:
		return 24 * time.Hour
	case PeriodWeekly:
		return 7 * 24 * time.Hour
	case PeriodMonthly:
		// Calendar months are not constant; we use 30 days as a fixed
		// approximation. Callers that need real calendar arithmetic
		// should use AddPeriod instead.
		return 30 * 24 * time.Hour
	}
	return 0
}

// AddPeriod advances `t` by exactly one period of `p`. For monthly, it uses
// calendar arithmetic (AddDate(0,1,0)) so subscription renewals land on the
// same day-of-month rather than drifting by 0.5 days/month.
func (p SubscriptionPeriod) AddPeriod(t time.Time) time.Time {
	switch p {
	case PeriodDaily:
		return t.AddDate(0, 0, 1)
	case PeriodWeekly:
		return t.AddDate(0, 0, 7)
	case PeriodMonthly:
		return t.AddDate(0, 1, 0)
	}
	return t
}

// ---------------------------------------------------------------------------
// Listing input validation
// ---------------------------------------------------------------------------

// ListingPricing is the user-supplied price/mode bundle. One of three shapes:
//
//	{Mode: buyout,    AskPriceMinor: >0}
//	{Mode: subscribe, SubscriptionPriceMinor: >0, Period: <enum>}
//	{Mode: auction,   AskPriceMinor: starting_price>0, Auction: <AuctionPricing>}
//
// For the auction shape, AskPriceMinor doubles as the *starting bid*; any
// bid must be at least starting_price + min_increment. The auction
// metadata (timing, reserve, anti-snipe window) lives in the nested
// AuctionPricing block so callers that only care about buyout/subscribe
// can ignore it entirely.
type ListingPricing struct {
	Mode                   ListingMode
	AskPriceMinor          int64
	SubscriptionPriceMinor int64
	Period                 SubscriptionPeriod
	Currency               string
	Auction                *AuctionPricing
}

// AuctionPricing carries the timing and ratchet metadata that distinguishes
// an English auction from a fixed-price buyout.
type AuctionPricing struct {
	// StartsAt and EndsAt bracket the bidding window. StartsAt may be in
	// the past at create time (auction opens immediately) but EndsAt must
	// be strictly after StartsAt.
	StartsAt time.Time
	EndsAt   time.Time
	// MinIncrementMinor is the smallest delta between successive bids,
	// expressed in minor currency units. Defaults to 1 if zero.
	MinIncrementMinor int64
	// ReserveMinor, if non-zero, is the lowest price the seller will
	// accept. Bids below the reserve are still recorded but the auction
	// settles to "reserve not met" rather than "sold" if the reserve
	// remains unmet at close.
	ReserveMinor int64
	// AntiSnipeSeconds extends EndsAt by N seconds whenever a bid arrives
	// in the final N seconds. 0 disables the rule.
	AntiSnipeSeconds int
}

// Common validation errors.
var (
	ErrInvalidMode             = errors.New("marketplace: invalid mode")
	ErrInvalidPeriod           = errors.New("marketplace: invalid subscription period")
	ErrMissingBuyoutPrice      = errors.New("marketplace: buyout listing requires ask_price_minor > 0")
	ErrMissingSubscribePrice   = errors.New("marketplace: subscribe listing requires subscription_price_minor > 0")
	ErrMissingSubscribePeriod  = errors.New("marketplace: subscribe listing requires period")
	ErrUnexpectedSubscribeCols = errors.New("marketplace: buyout listing must not set subscription pricing")
	ErrUnexpectedBuyoutPrice   = errors.New("marketplace: subscribe listing must not set ask_price_minor")
	ErrMissingAuctionPricing   = errors.New("marketplace: auction listing requires auction pricing block")
	ErrAuctionStartPriceZero   = errors.New("marketplace: auction listing requires ask_price_minor > 0 (starting bid)")
	ErrAuctionEndsAtBeforeStart = errors.New("marketplace: auction ends_at must be strictly after starts_at")
	ErrAuctionIncrementInvalid = errors.New("marketplace: auction min_increment_minor must be >= 0")
	ErrAuctionReserveInvalid   = errors.New("marketplace: auction reserve_minor must be >= 0")
	ErrAuctionAntiSnipeInvalid = errors.New("marketplace: auction anti_snipe_seconds must be >= 0")
)

// Validate checks that the pricing block is internally consistent for its
// declared mode. It does not enforce currency presence — the repository
// layer can default that.
func (p ListingPricing) Validate() error {
	if !p.Mode.IsValid() {
		return fmt.Errorf("%w: %q", ErrInvalidMode, p.Mode)
	}
	switch p.Mode {
	case ModeBuyout:
		if p.AskPriceMinor <= 0 {
			return ErrMissingBuyoutPrice
		}
		if p.SubscriptionPriceMinor != 0 || p.Period != "" {
			return ErrUnexpectedSubscribeCols
		}
	case ModeSubscribe:
		if p.SubscriptionPriceMinor <= 0 {
			return ErrMissingSubscribePrice
		}
		if p.Period == "" {
			return ErrMissingSubscribePeriod
		}
		if !p.Period.IsValid() {
			return fmt.Errorf("%w: %q", ErrInvalidPeriod, p.Period)
		}
		if p.AskPriceMinor != 0 {
			return ErrUnexpectedBuyoutPrice
		}
	case ModeAuction:
		if p.AskPriceMinor <= 0 {
			return ErrAuctionStartPriceZero
		}
		if p.SubscriptionPriceMinor != 0 || p.Period != "" {
			return ErrUnexpectedSubscribeCols
		}
		if p.Auction == nil {
			return ErrMissingAuctionPricing
		}
		if !p.Auction.EndsAt.After(p.Auction.StartsAt) {
			return ErrAuctionEndsAtBeforeStart
		}
		if p.Auction.MinIncrementMinor < 0 {
			return ErrAuctionIncrementInvalid
		}
		if p.Auction.ReserveMinor < 0 {
			return ErrAuctionReserveInvalid
		}
		if p.Auction.AntiSnipeSeconds < 0 {
			return ErrAuctionAntiSnipeInvalid
		}
	}
	return nil
}

// MinNextBidMinor returns the smallest bid that would be accepted for an
// auction with the given current top bid (0 if none) and starting price.
// Centralised so the API, the service and the UI can stay in lock-step.
func MinNextBidMinor(startingPriceMinor, currentTopMinor, minIncrementMinor int64) int64 {
	if minIncrementMinor < 1 {
		minIncrementMinor = 1
	}
	if currentTopMinor <= 0 {
		// No bid yet — first bid only needs to meet the starting price.
		return startingPriceMinor
	}
	return currentTopMinor + minIncrementMinor
}

// ApplyAntiSnipe returns the (possibly extended) auction end time. If the
// bid arrived in the last `antiSnipeSeconds` of the window, the end is
// pushed out so the bid is followed by at least `antiSnipeSeconds` of
// further bidding opportunity. Otherwise the original endsAt is returned
// unchanged.
func ApplyAntiSnipe(currentEndsAt, bidAt time.Time, antiSnipeSeconds int) time.Time {
	if antiSnipeSeconds <= 0 {
		return currentEndsAt
	}
	window := time.Duration(antiSnipeSeconds) * time.Second
	deadline := currentEndsAt.Add(-window)
	if bidAt.Before(deadline) {
		return currentEndsAt
	}
	extended := bidAt.Add(window)
	if extended.After(currentEndsAt) {
		return extended
	}
	return currentEndsAt
}

// ---------------------------------------------------------------------------
// Snapshot redaction (black-box layer)
// ---------------------------------------------------------------------------

// publicAgentMeta is the subset of agent fields we are willing to expose to
// non-owners. Crucially it omits SystemPrompt, SkillConfig, DomainConfig and
// EvolutionConfig — the IP that the seller is paying us to protect.
type publicAgentMeta struct {
	Name  string `json:"name,omitempty"`
	Role  string `json:"role,omitempty"`
	Focus string `json:"focus,omitempty"`
	// LearningSummary is intentionally a free-form, human-curated blurb
	// from the seller; we treat it as marketing copy and pass it through.
	LearningSummary string `json:"learning_summary,omitempty"`
}

// publicSnapshot is the redacted view: agent metadata only, no memories.
type publicSnapshot struct {
	Agent publicAgentMeta `json:"agent"`
	// Mode and pricing summary are added so list views can render without
	// extra DB lookups.
	Mode     ListingMode        `json:"mode,omitempty"`
	Period   SubscriptionPeriod `json:"period,omitempty"`
	Redacted bool               `json:"redacted"`
}

// fullSnapshot is the raw shape stored in `agent_market_listings.snapshot_payload`.
// It mirrors `marketplaceSnapshot` in cmd/server/marketplace_adapter.go but is
// declared here too so the redactor can deserialize without circular imports.
type fullSnapshot struct {
	Agent struct {
		Name            string          `json:"name,omitempty"`
		Role            string          `json:"role,omitempty"`
		Focus           string          `json:"focus,omitempty"`
		LearningSummary string          `json:"learning_summary,omitempty"`
		SystemPrompt    string          `json:"system_prompt,omitempty"`
		SkillConfig     json.RawMessage `json:"skill_config,omitempty"`
		DomainConfig    json.RawMessage `json:"domain_config,omitempty"`
		EvolutionConfig json.RawMessage `json:"evolution_config,omitempty"`
	} `json:"agent"`
	Memories json.RawMessage `json:"memories,omitempty"`
}

// RedactSnapshot returns a JSON snapshot safe to expose to non-owners.
//
// If `viewerIsOwner` is true, the original payload is returned unchanged
// (the seller is allowed to see their own agent fully). Otherwise the
// redactor extracts only public metadata and tags the result `redacted: true`
// so downstream UIs can render an "IP-protected" badge.
//
// Robustness: an unparseable input is replaced with `{"redacted": true}`
// rather than propagated, so a single corrupt row cannot leak prior contents.
func RedactSnapshot(raw json.RawMessage, mode ListingMode, period SubscriptionPeriod, viewerIsOwner bool) json.RawMessage {
	if viewerIsOwner {
		if len(raw) == 0 {
			return json.RawMessage(`{}`)
		}
		return raw
	}

	pub := publicSnapshot{Mode: mode, Period: period, Redacted: true}

	if len(raw) > 0 {
		var full fullSnapshot
		if err := json.Unmarshal(raw, &full); err == nil {
			pub.Agent = publicAgentMeta{
				Name:            full.Agent.Name,
				Role:            full.Agent.Role,
				Focus:           full.Agent.Focus,
				LearningSummary: full.Agent.LearningSummary,
			}
		}
		// On unmarshal error we return a redacted-empty payload — the
		// safer of the two options.
	}

	out, err := json.Marshal(pub)
	if err != nil {
		// Marshal of a tiny known struct cannot realistically fail; if it
		// does, fall through to a hard-coded redacted marker.
		return json.RawMessage(`{"redacted":true}`)
	}
	return out
}

// ---------------------------------------------------------------------------
// Inference contract
// ---------------------------------------------------------------------------

// InferenceRequest is what a subscriber sends to the gateway. Free-form
// payload is allowed to keep the gateway agnostic to specific agent kinds.
type InferenceRequest struct {
	SubscriptionID string          `json:"subscription_id"`
	Payload        json.RawMessage `json:"payload"`
}

// InferenceResponse is what the gateway returns. The `Output` is the
// agent's structured signal/recommendation; `Trace` is optional debug info
// the seller chose to expose. Crucially neither field carries the raw
// prompt that produced it.
type InferenceResponse struct {
	Output   json.RawMessage `json:"output"`
	Trace    json.RawMessage `json:"trace,omitempty"`
	Latency  time.Duration   `json:"latency_ns"`
	IssuedAt time.Time       `json:"issued_at"`
}

// InferenceGateway is the seam between HTTP layer and the actual LLM
// invocation. The cmd/server adapter implements this against the seller's
// agent runtime, so the buyer never directly addresses the LLM.
type InferenceGateway interface {
	Invoke(req InferenceRequest) (InferenceResponse, error)
}
