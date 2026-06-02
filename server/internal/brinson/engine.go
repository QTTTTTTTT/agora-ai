package brinson

import (
	"strings"
	"time"
)

// Engine is stateless; safe for concurrent use.
type Engine struct {
	Now func() time.Time
}

func NewEngine() *Engine {
	return &Engine{Now: func() time.Time { return time.Now().UTC() }}
}

// Compute runs the three-effect decomposition on the two
// compositions. Keys are matched case-insensitively to forgive
// "Equity" vs "equity" mismatches between the holdings table
// (which uses whatever the upstream broker returned) and the
// benchmark composition (which the admin curates).
//
// Buckets that appear on only one side still contribute to the
// identity (the missing side has w=0, r=0). This is the "honest"
// reconciliation behaviour: if the benchmark has a Healthcare
// bucket but the portfolio has zero healthcare holdings, the
// missing portfolio side contributes a small negative selection
// effect, and the engine will surface that.
func (e *Engine) Compute(portfolio, benchmark Composition) Result {
	now := time.Now().UTC()
	if e != nil && e.Now != nil {
		now = e.Now()
	}
	bm := buildIndex(benchmark.Buckets)
	pm := buildIndex(portfolio.Buckets)

	keys := make(map[string]string) // lower → preferred display key
	for k := range bm {
		keys[k] = bm[k].Key
	}
	for k, b := range pm {
		if _, ok := keys[k]; !ok {
			keys[k] = b.Key
		}
	}

	res := Result{
		BenchmarkID: benchmark.BenchmarkID,
		Dimension:   benchmark.Dimension,
		GeneratedAt: now,
		BucketCount: len(keys),
		Buckets:     make([]BucketAttribution, 0, len(keys)),
	}

	var pr, br float64
	for lowerKey, display := range keys {
		p, pHas := pm[lowerKey]
		b, bHas := bm[lowerKey]
		var pw, bw, prk, brk float64
		if pHas {
			pw = p.Weight
			prk = p.ReturnPct
		}
		if bHas {
			bw = b.Weight
			brk = b.ReturnPct
		}
		alloc := (pw - bw) * brk
		sel := bw * (prk - brk)
		inter := (pw - bw) * (prk - brk)
		res.Buckets = append(res.Buckets, BucketAttribution{
			Key:               display,
			PortfolioWeight:   pw,
			BenchmarkWeight:   bw,
			PortfolioReturn:   prk,
			BenchmarkReturn:   brk,
			AllocationEffect:  alloc,
			SelectionEffect:   sel,
			InteractionEffect: inter,
		})
		pr += pw * prk
		br += bw * brk
		res.AllocationTotal += alloc
		res.SelectionTotal += sel
		res.InteractionTotal += inter
	}
	res.PortfolioReturn = pr
	res.BenchmarkReturn = br
	res.ActiveReturn = pr - br
	sortBucketsByMagnitude(res.Buckets)
	return res
}

func buildIndex(buckets []Bucket) map[string]Bucket {
	out := make(map[string]Bucket, len(buckets))
	for _, b := range buckets {
		key := strings.ToLower(strings.TrimSpace(b.Key))
		out[key] = b
	}
	return out
}

// PortfolioFromHoldings builds a Composition from a slice of
// holdings annotated with bucket key (asset_class or market) and
// per-holding return. Returns are aggregated as
// MV-weighted means within each bucket. Buckets with zero MV are
// silently dropped (they can't contribute to active return
// anyway).
//
// Callers are expected to normalise nullable strings (lowercase
// trim) before passing in.
func PortfolioFromHoldings(dim BucketDimension, holdings []HoldingInput, asof time.Time) Composition {
	type agg struct {
		MV  float64
		WPR float64 // sum of MV * return
	}
	buckets := map[string]*agg{}
	keysOrder := []string{}
	keyDisplay := map[string]string{}
	var totalMV float64
	for _, h := range holdings {
		bucket := strings.TrimSpace(h.Bucket)
		if bucket == "" {
			bucket = "uncategorised"
		}
		lower := strings.ToLower(bucket)
		if _, ok := buckets[lower]; !ok {
			buckets[lower] = &agg{}
			keysOrder = append(keysOrder, lower)
			keyDisplay[lower] = bucket
		}
		mv := h.MarketValue
		if mv < 0 {
			// Shorts contribute |MV| to bucket notional but
			// their return is sign-flipped: if the position
			// dropped 1% the short leg gains 1%.
			mv = -mv
		}
		buckets[lower].MV += mv
		buckets[lower].WPR += mv * h.ReturnPct
		totalMV += mv
	}
	out := Composition{Dimension: dim, AsOf: asof, Buckets: []Bucket{}}
	if totalMV == 0 {
		return out
	}
	for _, key := range keysOrder {
		a := buckets[key]
		if a.MV == 0 {
			continue
		}
		out.Buckets = append(out.Buckets, Bucket{
			Key:       keyDisplay[key],
			Weight:    a.MV / totalMV,
			ReturnPct: a.WPR / a.MV,
		})
	}
	return out
}

// HoldingInput is the minimum view of a position the portfolio-
// composition helper needs. Bucket = the chosen dimension's value.
type HoldingInput struct {
	Bucket      string
	MarketValue float64
	ReturnPct   float64
}
