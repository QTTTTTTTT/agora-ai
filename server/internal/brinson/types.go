// Package brinson computes Brinson (1986 / Brinson-Hood-Beebower)
// performance attribution: decomposes a portfolio's active return
// versus a benchmark into three effects per bucket.
//
// Identity (per bucket k):
//
//	allocation_k  = (w_p[k] - w_b[k]) * r_b[k]
//	selection_k   = w_b[k]            * (r_p[k] - r_b[k])
//	interaction_k = (w_p[k] - w_b[k]) * (r_p[k] - r_b[k])
//
//	r_p - r_b     = sum_k allocation_k + selection_k + interaction_k
//
// where
//
//   w_p[k] = portfolio weight in bucket k (fraction; sum = 1)
//   w_b[k] = benchmark weight in bucket k (fraction; sum = 1)
//   r_p[k] = portfolio return in bucket k (signed fraction)
//   r_b[k] = benchmark return in bucket k (signed fraction)
//
// Buckets are typically GICS sectors, but the engine is bucket-
// agnostic — the caller picks the dimension (asset_class, market,
// or sector mapping) and provides matching keys on both sides.
//
// Buckets present in only one of the two compositions are still
// honoured: the missing side contributes a zero weight + zero
// return, so the identity stays exact.
package brinson

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// BucketDimension picks the aggregation key. The CHECK constraint
// on the DB column mirrors this enum.
type BucketDimension string

const (
	DimAssetClass BucketDimension = "asset_class"
	DimMarket     BucketDimension = "market"
	DimSector     BucketDimension = "sector"
)

var AllDimensions = []BucketDimension{DimAssetClass, DimMarket, DimSector}

func (d BucketDimension) IsValid() bool {
	switch d {
	case DimAssetClass, DimMarket, DimSector:
		return true
	}
	return false
}

func ParseBucketDimension(s string) (BucketDimension, bool) {
	d := BucketDimension(strings.TrimSpace(strings.ToLower(s)))
	if !d.IsValid() {
		return "", false
	}
	return d, true
}

// Bucket is one row of weights + returns in a Composition.
type Bucket struct {
	Key       string  `json:"key"`
	Weight    float64 `json:"weight"`
	ReturnPct float64 `json:"return_pct"`
}

// Validate enforces invariants needed for safe math.
func (b Bucket) Validate() error {
	if strings.TrimSpace(b.Key) == "" {
		return fmt.Errorf("brinson: bucket key required")
	}
	if math.IsNaN(b.Weight) || math.IsInf(b.Weight, 0) {
		return fmt.Errorf("brinson: bucket %q weight must be finite", b.Key)
	}
	if math.IsNaN(b.ReturnPct) || math.IsInf(b.ReturnPct, 0) {
		return fmt.Errorf("brinson: bucket %q return must be finite", b.Key)
	}
	if b.Weight < 0 {
		return fmt.Errorf("brinson: bucket %q weight cannot be negative", b.Key)
	}
	// Sanity: weight > 5 means someone passed a percentage (60)
	// instead of a fraction (0.60). Refuse.
	if b.Weight > 5 {
		return fmt.Errorf("brinson: bucket %q weight %f exceeds 5x cap (use fractions, not percentages)", b.Key, b.Weight)
	}
	return nil
}

// Composition is one side of the attribution input. Used for
// both benchmark (admin-managed, stored) and portfolio (live,
// derived from holdings).
type Composition struct {
	BenchmarkID string          `json:"benchmark_id,omitempty"`
	Dimension   BucketDimension `json:"dimension"`
	AsOf        time.Time       `json:"asof"`
	Buckets     []Bucket        `json:"buckets"`
	Note        string          `json:"note,omitempty"`
}

// Validate enforces invariants for safe math.
func (c Composition) Validate() error {
	if !c.Dimension.IsValid() {
		return fmt.Errorf("brinson: invalid dimension %q", c.Dimension)
	}
	if len(c.Buckets) == 0 {
		return fmt.Errorf("brinson: composition has no buckets")
	}
	seen := map[string]struct{}{}
	var sumW float64
	for _, b := range c.Buckets {
		if err := b.Validate(); err != nil {
			return err
		}
		key := strings.ToLower(strings.TrimSpace(b.Key))
		if _, dup := seen[key]; dup {
			return fmt.Errorf("brinson: duplicate bucket key %q", b.Key)
		}
		seen[key] = struct{}{}
		sumW += b.Weight
	}
	// Sum-of-weights tolerance: ±1% off perfect.
	if math.Abs(sumW-1.0) > 0.01 {
		return fmt.Errorf("brinson: weights sum to %f, expected ~1.0", sumW)
	}
	return nil
}

// BucketAttribution is the per-bucket output row of the engine.
type BucketAttribution struct {
	Key               string  `json:"key"`
	PortfolioWeight   float64 `json:"portfolio_weight"`
	BenchmarkWeight   float64 `json:"benchmark_weight"`
	PortfolioReturn   float64 `json:"portfolio_return"`
	BenchmarkReturn   float64 `json:"benchmark_return"`
	AllocationEffect  float64 `json:"allocation_effect"`
	SelectionEffect   float64 `json:"selection_effect"`
	InteractionEffect float64 `json:"interaction_effect"`
}

// Result is the engine's output.
type Result struct {
	FundID            string              `json:"fund_id,omitempty"`
	BenchmarkID       string              `json:"benchmark_id"`
	Dimension         BucketDimension     `json:"dimension"`
	GeneratedAt       time.Time           `json:"generated_at"`
	PortfolioReturn   float64             `json:"portfolio_return"`
	BenchmarkReturn   float64             `json:"benchmark_return"`
	ActiveReturn      float64             `json:"active_return"`
	AllocationTotal   float64             `json:"allocation_total"`
	SelectionTotal    float64             `json:"selection_total"`
	InteractionTotal  float64             `json:"interaction_total"`
	BucketCount       int                 `json:"bucket_count"`
	Buckets           []BucketAttribution `json:"buckets"`
}

// sortBucketsByMagnitude sorts the per-bucket rows by the total
// effect (allocation + selection + interaction) in absolute terms
// descending — biggest active-return contributor first.
func sortBucketsByMagnitude(rows []BucketAttribution) {
	sort.SliceStable(rows, func(i, j int) bool {
		mi := math.Abs(rows[i].AllocationEffect + rows[i].SelectionEffect + rows[i].InteractionEffect)
		mj := math.Abs(rows[j].AllocationEffect + rows[j].SelectionEffect + rows[j].InteractionEffect)
		return mi > mj
	})
}
