package marketdata

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ProviderRateLimit pins the maximum upstream call rate (per provider) so a
// surge of cache-miss `GetQuote` calls cannot stampede a public endpoint and
// get the platform's IP blocked. Bursts default to 1.0 second of refill so a
// thin burst of parallel requests still goes through, but a sustained surge
// is paced.
//
// The default values below are intentionally conservative:
//
//   - Yahoo: most generous of the lot, but the guest endpoint (v8/finance)
//     does throttle aggressively when traffic looks bot-like.
//   - Eastmoney + Tencent: Chinese broker mirrors, especially sensitive to
//     scraping patterns; published rate caps are not advertised so we use
//     "polite client" ceilings observed in the wild.
//   - Sina: hq.sinajs.cn is happy with steady traffic but goes 451 quickly
//     when bursted.
//   - CoinGecko: free tier officially 30 req/min; we cap at 10 with bursts
//     so we never trip the warning band.
//
// Operators can override any of these via env (see ProviderRateLimits below).
type ProviderRateLimit struct {
	// PerSecond is the steady-state rate in req/s. Zero or negative disables
	// the limiter for this provider (fallback to "no limit" behaviour).
	PerSecond float64
	// Burst is the bucket capacity. A reasonable default is max(1, rate*1s).
	Burst int
}

// ProviderRateLimits maps lowercase provider name to its limit. Unknown
// providers (no entry) bypass the limiter entirely.
type ProviderRateLimits map[string]ProviderRateLimit

// DefaultProviderRateLimits is the production-ready baseline. The map keys
// match the lowercase provider names used by namedQuoteProviders and the
// news provider chain. Tests may pass a different map via Config.
func DefaultProviderRateLimits() ProviderRateLimits {
	return ProviderRateLimits{
		"yahoo":     {PerSecond: 1.0, Burst: 3},  // ~60 req/min
		"eastmoney": {PerSecond: 0.5, Burst: 2},  // ~30 req/min
		"sina":      {PerSecond: 1.0, Burst: 3},  // ~60 req/min
		"tencent":   {PerSecond: 0.5, Burst: 2},  // ~30 req/min
		"coingecko": {PerSecond: 0.17, Burst: 2}, // ~10 req/min
		// china-stock / akshare / quantdinger are operator-controlled
		// internal endpoints; default to "no limit" so on-prem deployments
		// keep the same behaviour as before this feature shipped.
	}
}

// providerRateLimiter wraps a set of `*rate.Limiter`, one per provider name.
// Wait() blocks until a token is available (or ctx is done) for that
// provider. A nil limiter (provider absent from the map / disabled) is a
// no-op so callers don't have to branch.
type providerRateLimiter struct {
	mu       sync.RWMutex
	limiters map[string]*rate.Limiter
}

func newProviderRateLimiter(cfg ProviderRateLimits) *providerRateLimiter {
	prl := &providerRateLimiter{
		limiters: make(map[string]*rate.Limiter, len(cfg)),
	}
	for name, limit := range cfg {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		if limit.PerSecond <= 0 {
			continue
		}
		burst := limit.Burst
		if burst <= 0 {
			burst = 1
			if limit.PerSecond >= 1 {
				burst = int(limit.PerSecond + 0.5)
			}
		}
		prl.limiters[key] = rate.NewLimiter(rate.Limit(limit.PerSecond), burst)
	}
	return prl
}

// Wait blocks until the named provider has capacity for a single request, or
// returns ctx.Err() if the context is cancelled / times out first. Providers
// with no configured limit return immediately. ctx may be nil — we substitute
// context.Background to keep callers ergonomic.
func (p *providerRateLimiter) Wait(ctx context.Context, name string) error {
	if p == nil {
		return nil
	}
	limiter := p.lookup(name)
	if limiter == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return limiter.Wait(ctx)
}

// Allow is the non-blocking version. Returns true when capacity exists and
// consumes a token; returns false otherwise without blocking. Used in tight
// loops (e.g. SSE pushers) where we'd rather skip a tick than queue.
func (p *providerRateLimiter) Allow(name string) bool {
	if p == nil {
		return true
	}
	limiter := p.lookup(name)
	if limiter == nil {
		return true
	}
	return limiter.Allow()
}

func (p *providerRateLimiter) lookup(name string) *rate.Limiter {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.limiters[key]
}

// ParseProviderRateLimits decodes an env-style spec like
// "yahoo=60/m,eastmoney=30/m,sina=120/m" into a ProviderRateLimits.
// Empty input returns DefaultProviderRateLimits unchanged so operators
// only need to set the env var to override.
//
// Accepted units:
//
//	N/s    – N requests per second
//	N/m    – N requests per minute
//	N/h    – N requests per hour
//	N      – N requests per second (bare integer)
//
// Each entry may include an optional burst suffix "@K" (e.g.
// "yahoo=60/m@5"). Burst defaults to max(1, rate*1s) when omitted.
//
// Unknown / malformed entries are silently dropped and a soft warning is
// the caller's responsibility (we don't want a bad env var to crash the
// binary).
func ParseProviderRateLimits(spec string, fallback ProviderRateLimits) ProviderRateLimits {
	if fallback == nil {
		fallback = DefaultProviderRateLimits()
	}
	merged := make(ProviderRateLimits, len(fallback))
	for k, v := range fallback {
		merged[k] = v
	}
	for _, raw := range strings.Split(spec, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 || eq == len(entry)-1 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(entry[:eq]))
		valueStr := strings.TrimSpace(entry[eq+1:])
		burst := 0
		if at := strings.IndexByte(valueStr, '@'); at > 0 && at < len(valueStr)-1 {
			b, err := strconv.Atoi(strings.TrimSpace(valueStr[at+1:]))
			if err == nil && b > 0 {
				burst = b
			}
			valueStr = strings.TrimSpace(valueStr[:at])
		}
		rateValue, ok := parseRateExpression(valueStr)
		if !ok {
			continue
		}
		if burst == 0 {
			burst = 1
			if rateValue >= 1 {
				burst = int(rateValue + 0.5)
			}
		}
		merged[name] = ProviderRateLimit{PerSecond: rateValue, Burst: burst}
	}
	return merged
}

// parseRateExpression converts "60/m", "0.5/s", "10" to req/sec.
func parseRateExpression(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	unitDivisor := 1.0
	if slash := strings.IndexByte(value, '/'); slash > 0 && slash == len(value)-2 {
		switch strings.ToLower(value[slash+1:]) {
		case "s":
			unitDivisor = 1
		case "m":
			unitDivisor = 60
		case "h":
			unitDivisor = 3600
		default:
			return 0, false
		}
		value = value[:slash]
	}
	count, err := strconv.ParseFloat(value, 64)
	if err != nil || count <= 0 {
		return 0, false
	}
	return count / unitDivisor, true
}

// throttleAwareCooldown returns the cooldown to apply when a provider error
// looks like an upstream throttle (HTTP 429, 451 or a circuit-breaker
// classified throttle). Throttled providers get a longer break than a
// regular failure so we don't keep poking the upstream and escalate the
// situation into a hard IP block. The minimum is `base` so this never
// shortens a regular cooldown.
func throttleAwareCooldown(base time.Duration, throttled bool) time.Duration {
	if !throttled {
		return base
	}
	extended := 5 * time.Minute
	if extended > base {
		return extended
	}
	return base
}
