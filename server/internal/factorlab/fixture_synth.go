package factorlab

import (
	"math"
	"math/rand"
	"time"
)

// SynthFixture builds a deterministic synthetic OHLC fixture.
// Each symbol has its own (annualised) drift + volatility + beta
// against a synthetic benchmark, so cross-sectional factor
// strategies (momentum, low-beta, low-vol) produce meaningful
// signal at backtest time WITHOUT touching the network.
//
// Use this for unit tests + sanity-check runs. Production-grade
// backtests should load real frozen-CSV fixtures via LoadFixture.
//
// The generated paths follow a simple market-model:
//
//	r_market_t       = N(driftMarket/252, volMarket/sqrt(252))
//	r_symbol_t       = alpha/252 + beta * r_market_t + idio_t
//	idio_t           = N(0, idioVol/sqrt(252))
//
// where (alpha, beta, idioVol) is the symbol's profile. The
// resulting price path obeys log-normal dynamics around the
// market, just like a real factor universe.
type SynthProfile struct {
	Symbol   string
	Alpha    float64 // annual drift premium vs market (0.05 = 5% / yr)
	Beta     float64 // CAPM beta (1.0 = market-like)
	IdioVol  float64 // annualised idiosyncratic vol (0.20 = 20%)
	StartPx  float64 // starting price (default 100)
}

// DefaultSynthProfiles returns a 5-name universe with a wide
// cross-section so each MVP factor has something to chew on:
//   - HI_MOM: high alpha + high beta → momentum should pick it
//   - LOW_BETA: low beta + decent alpha → low-beta should pick it
//   - HI_VOL: zero alpha + high idio vol → low-vol should AVOID
//   - DRIFTER: positive alpha + low idio vol → both momentum +
//              low-vol should pick it (the "double-overlap" tell)
//   - DOG: negative alpha + average beta → every factor should
//          AVOID
func DefaultSynthProfiles() []SynthProfile {
	return []SynthProfile{
		{Symbol: "HI_MOM", Alpha: 0.18, Beta: 1.3, IdioVol: 0.22, StartPx: 100},
		{Symbol: "LOW_BETA", Alpha: 0.08, Beta: 0.5, IdioVol: 0.15, StartPx: 100},
		{Symbol: "HI_VOL", Alpha: 0.00, Beta: 1.0, IdioVol: 0.45, StartPx: 100},
		{Symbol: "DRIFTER", Alpha: 0.12, Beta: 0.8, IdioVol: 0.12, StartPx: 100},
		{Symbol: "DOG", Alpha: -0.08, Beta: 1.0, IdioVol: 0.25, StartPx: 100},
	}
}

// SynthOptions tunes the market path + sample length.
type SynthOptions struct {
	Seed         int64   // PRNG seed for reproducibility (default 42)
	DriftMarket  float64 // annualised market drift (default 0.07)
	VolMarket    float64 // annualised market vol (default 0.16)
	Profiles     []SynthProfile
	StartDate    time.Time // default = 2y ago, last Monday
	Days         int       // number of business days (default 504 = ~2y)
}

func (o SynthOptions) withDefaults() SynthOptions {
	if o.Seed == 0 {
		o.Seed = 42
	}
	if o.DriftMarket == 0 {
		o.DriftMarket = 0.07
	}
	if o.VolMarket == 0 {
		o.VolMarket = 0.16
	}
	if len(o.Profiles) == 0 {
		o.Profiles = DefaultSynthProfiles()
	}
	if o.Days <= 0 {
		o.Days = 504
	}
	if o.StartDate.IsZero() {
		// Anchor on a deterministic-feeling date so tests don't
		// shift with time.Now().
		o.StartDate = time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	}
	return o
}

// BuildSynthFixture generates the synthetic Fixture. Pure /
// deterministic given the (seed, profiles, options).
func BuildSynthFixture(opts SynthOptions) *Fixture {
	opts = opts.withDefaults()
	rng := rand.New(rand.NewSource(opts.Seed))
	dailyMu := opts.DriftMarket / 252.0
	dailySigma := opts.VolMarket / math.Sqrt(252.0)

	dates := businessDays(opts.StartDate, opts.Days)
	marketReturns := make([]float64, opts.Days)
	for i := range marketReturns {
		marketReturns[i] = dailyMu + dailySigma*rng.NormFloat64()
	}

	bench := buildBenchmark("MKT_SYNTH", dates, marketReturns, 100.0)

	fixture := &Fixture{
		Benchmark: bench,
		Start:     dates[0],
		End:       dates[len(dates)-1],
	}

	for _, p := range opts.Profiles {
		hist := buildSymbolPath(p, dates, marketReturns, rng)
		fixture.Histories = append(fixture.Histories, hist)
	}
	return fixture
}

func buildSymbolPath(p SynthProfile, dates []time.Time, mktR []float64, rng *rand.Rand) SymbolHistory {
	if p.StartPx <= 0 {
		p.StartPx = 100
	}
	dailyAlpha := p.Alpha / 252.0
	idioSigma := p.IdioVol / math.Sqrt(252.0)
	bars := make([]Bar, len(dates))
	px := p.StartPx
	for i, d := range dates {
		r := dailyAlpha + p.Beta*mktR[i] + idioSigma*rng.NormFloat64()
		px = px * math.Exp(r)
		// Synth OHLC: close is the simulated price; open / high /
		// low are constructed with small noise around close so
		// strategies that touch ATR have something to work with.
		jitter := 0.005 * px
		bars[i] = Bar{
			Date:  d,
			Open:  px - jitter*rng.Float64(),
			High:  px + jitter*(0.5+rng.Float64()),
			Low:   px - jitter*(0.5+rng.Float64()),
			Close: px,
		}
	}
	return SymbolHistory{Symbol: p.Symbol, Bars: bars, Market: "us_equity"}
}

func buildBenchmark(symbol string, dates []time.Time, mktR []float64, startPx float64) *SymbolHistory {
	bars := make([]Bar, len(dates))
	px := startPx
	for i, d := range dates {
		px = px * math.Exp(mktR[i])
		bars[i] = Bar{Date: d, Close: px, Open: px, High: px, Low: px}
	}
	return &SymbolHistory{Symbol: symbol, Bars: bars, Market: "us_equity"}
}

// businessDays returns N consecutive M-F days starting from
// start (rolled forward to Monday if start lands on a weekend).
// We don't bother with holidays because the backtest is purely
// synthetic — perfect calendar coverage simplifies the math.
func businessDays(start time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	d := normaliseDate(start)
	for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		d = d.AddDate(0, 0, 1)
	}
	for len(out) < n {
		out = append(out, d)
		d = d.AddDate(0, 0, 1)
		for d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			d = d.AddDate(0, 0, 1)
		}
	}
	return out
}
