// credit_packs.go — Phase C-1 SKU definitions for /advisor credit
// packs.
//
// SKUs are hard-coded (not DB-stored) because:
//   1. Pricing decisions go through code review anyway (we don't
//      want a PM running `UPDATE` on a production billing table).
//   2. The LemonSqueezy variant_id binding has to land in env
//      before the SKU can be sold; making the SKU itself code
//      keeps "what's available to buy" and "how it's priced" in
//      one place.
//   3. The set is small (3 tiers) so the DRY argument for a table
//      is weak; refactoring to DB-stored becomes worth it only
//      when we add tiered discounts / promo codes (out of scope
//      for Phase C).
//
// Each pack maps to one LemonSqueezy "variant" in the merchant
// account. The variant_id is read from env at boot — see
// lemonsqueezy.NewClientFromEnv. Without the env var the pack
// stays "configured" but the checkout endpoint returns 503.

package advisorbilling

import (
	"errors"
	"strings"
)

// CreditPack is one purchasable SKU.
type CreditPack struct {
	SKU               string
	LabelZh           string
	LabelEn           string
	DescriptionZh     string
	DescriptionEn     string
	DeepUnits         int
	QuickUnits        int
	PriceCentsUSD     int
	LemonSqueezyVariantEnvVar string
	// SortOrder controls how the SPA stacks the cards (smallest
	// first by default).
	SortOrder int
}

// CreditPackCatalog returns the read-only catalog. Returned slice
// is a fresh copy so callers can mutate it without polluting
// other goroutines.
//
// Pricing rationale ($1/unit deep, $0.5/unit quick) is a Phase C
// best guess; the actual LemonSqueezy variant price is what
// gets charged — these constants are only the "what we expect
// the variant to be priced at" annotation used by the SPA card.
// A mismatch surfaces as a Sentry warning at webhook time so
// ops can either re-bind the env or update the constant.
func CreditPackCatalog() []CreditPack {
	return []CreditPack{
		{
			SKU:           "advisor_credits_small",
			LabelZh:       "小包 · 50 单元",
			LabelEn:       "Small · 50 units",
			DescriptionZh: "30 次深度 + 200 次快速 · 适合每周用 1-2 次的用户",
			DescriptionEn: "30 deep + 200 quick · for users who consult a few times a week",
			DeepUnits:     30,
			QuickUnits:    200,
			PriceCentsUSD: 1900,
			LemonSqueezyVariantEnvVar: "LEMONSQUEEZY_VARIANT_CREDITS_SMALL",
			SortOrder:     10,
		},
		{
			SKU:           "advisor_credits_medium",
			LabelZh:       "中包 · 200 单元",
			LabelEn:       "Medium · 200 units",
			DescriptionZh: "120 次深度 + 800 次快速 · 适合每天用一次的用户",
			DescriptionEn: "120 deep + 800 quick · for daily consult users",
			DeepUnits:     120,
			QuickUnits:    800,
			PriceCentsUSD: 5900,
			LemonSqueezyVariantEnvVar: "LEMONSQUEEZY_VARIANT_CREDITS_MEDIUM",
			SortOrder:     20,
		},
		{
			SKU:           "advisor_credits_large",
			LabelZh:       "大包 · 1000 单元",
			LabelEn:       "Large · 1000 units",
			DescriptionZh: "600 次深度 + 4000 次快速 · 团队和专业交易员",
			DescriptionEn: "600 deep + 4000 quick · for teams and pro traders",
			DeepUnits:     600,
			QuickUnits:    4000,
			PriceCentsUSD: 19900,
			LemonSqueezyVariantEnvVar: "LEMONSQUEEZY_VARIANT_CREDITS_LARGE",
			SortOrder:     30,
		},
	}
}

// ErrUnknownPack is returned by LookupCreditPack when the SKU
// doesn't match any catalog row.
var ErrUnknownPack = errors.New("advisorbilling: unknown credit pack SKU")

// LookupCreditPack returns the pack with the given SKU.
func LookupCreditPack(sku string) (*CreditPack, error) {
	sku = strings.TrimSpace(sku)
	if sku == "" {
		return nil, ErrUnknownPack
	}
	for _, p := range CreditPackCatalog() {
		if p.SKU == sku {
			pack := p
			return &pack, nil
		}
	}
	return nil, ErrUnknownPack
}
