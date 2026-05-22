package marketplace

import (
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// ListingPricing.Validate — auction shape
// ---------------------------------------------------------------------------

func TestListingPricingValidateAuctionHappyPath(t *testing.T) {
	now := time.Now()
	p := ListingPricing{
		Mode:          ModeAuction,
		AskPriceMinor: 1000,
		Currency:      "USD",
		Auction: &AuctionPricing{
			StartsAt:          now,
			EndsAt:            now.Add(time.Hour),
			MinIncrementMinor: 50,
			ReserveMinor:      2000,
			AntiSnipeSeconds:  30,
		},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("expected valid pricing, got %v", err)
	}
}

func TestListingPricingValidateAuctionRejectsZeroStartingPrice(t *testing.T) {
	now := time.Now()
	p := ListingPricing{
		Mode: ModeAuction,
		Auction: &AuctionPricing{
			StartsAt: now,
			EndsAt:   now.Add(time.Hour),
		},
	}
	if err := p.Validate(); !errors.Is(err, ErrAuctionStartPriceZero) {
		t.Fatalf("expected ErrAuctionStartPriceZero, got %v", err)
	}
}

func TestListingPricingValidateAuctionRejectsMissingBlock(t *testing.T) {
	p := ListingPricing{
		Mode:          ModeAuction,
		AskPriceMinor: 100,
	}
	if err := p.Validate(); !errors.Is(err, ErrMissingAuctionPricing) {
		t.Fatalf("expected ErrMissingAuctionPricing, got %v", err)
	}
}

func TestListingPricingValidateAuctionRejectsEndsBeforeStarts(t *testing.T) {
	now := time.Now()
	p := ListingPricing{
		Mode:          ModeAuction,
		AskPriceMinor: 100,
		Auction: &AuctionPricing{
			StartsAt: now,
			EndsAt:   now.Add(-time.Hour),
		},
	}
	if err := p.Validate(); !errors.Is(err, ErrAuctionEndsAtBeforeStart) {
		t.Fatalf("expected ErrAuctionEndsAtBeforeStart, got %v", err)
	}
}

func TestListingPricingValidateAuctionRejectsSubscribeFields(t *testing.T) {
	now := time.Now()
	p := ListingPricing{
		Mode:                   ModeAuction,
		AskPriceMinor:          100,
		SubscriptionPriceMinor: 50, // bleed-over from subscribe mode
		Auction: &AuctionPricing{
			StartsAt: now,
			EndsAt:   now.Add(time.Hour),
		},
	}
	if err := p.Validate(); !errors.Is(err, ErrUnexpectedSubscribeCols) {
		t.Fatalf("expected ErrUnexpectedSubscribeCols, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// MinNextBidMinor
// ---------------------------------------------------------------------------

func TestMinNextBidMinorReturnsStartingPriceWhenNoBidYet(t *testing.T) {
	if got := MinNextBidMinor(500, 0, 10); got != 500 {
		t.Fatalf("expected 500, got %d", got)
	}
}

func TestMinNextBidMinorAddsIncrement(t *testing.T) {
	if got := MinNextBidMinor(500, 800, 10); got != 810 {
		t.Fatalf("expected 810, got %d", got)
	}
}

func TestMinNextBidMinorClampsZeroIncrementToOne(t *testing.T) {
	// Defensive: a misconfigured min_increment of 0 must not allow
	// equal-amount bids (which would deadlock the auction).
	if got := MinNextBidMinor(500, 800, 0); got != 801 {
		t.Fatalf("expected 801 (incr clamped to 1), got %d", got)
	}
}

// ---------------------------------------------------------------------------
// ApplyAntiSnipe
// ---------------------------------------------------------------------------

func TestApplyAntiSnipeNoExtensionWhenBidIsEarly(t *testing.T) {
	endsAt := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	bidAt := endsAt.Add(-5 * time.Minute) // well before the snipe window
	got := ApplyAntiSnipe(endsAt, bidAt, 60)
	if !got.Equal(endsAt) {
		t.Fatalf("expected ends_at unchanged, got %s vs %s", got, endsAt)
	}
}

func TestApplyAntiSnipeExtendsWhenBidIsInSnipeWindow(t *testing.T) {
	endsAt := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	bidAt := endsAt.Add(-10 * time.Second) // inside the 60s snipe window
	got := ApplyAntiSnipe(endsAt, bidAt, 60)
	wantMin := bidAt.Add(60 * time.Second)
	if !got.Equal(wantMin) {
		t.Fatalf("expected ends_at extended to %s, got %s", wantMin, got)
	}
}

func TestApplyAntiSnipeNeverShortensEndTime(t *testing.T) {
	// If somehow the extension would land before the original ends_at
	// (e.g. clock skew), the original ends_at must win.
	endsAt := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	bidAt := endsAt.Add(-30 * time.Second)
	got := ApplyAntiSnipe(endsAt, bidAt, 30) // bid+30s == endsAt exactly
	if got.Before(endsAt) {
		t.Fatalf("anti-snipe must not shorten end time")
	}
}

func TestApplyAntiSnipeDisabledWhenSecondsIsZero(t *testing.T) {
	endsAt := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	bidAt := endsAt.Add(-1 * time.Second)
	if got := ApplyAntiSnipe(endsAt, bidAt, 0); !got.Equal(endsAt) {
		t.Fatalf("disabled anti-snipe must not extend; got %s", got)
	}
}
