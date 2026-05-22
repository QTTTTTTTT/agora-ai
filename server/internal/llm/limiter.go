package llm

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// OwnerLimiter 提供按 (ownerID, provider) 维度的熔断 + 限流，
// 把"某个 owner 的某个 provider 出问题/超额"的爆炸半径限制在该 owner 内，
// 不影响其他 owner 的调用。
//
// 关键点：
//   - 熔断器：连续 N 次失败 → open 一段时间 → half-open 探测 → 恢复
//   - 令牌桶：每 owner 每 provider 一个独立桶，按 RPS 上限发放
//
// 设计为零配置可用：未配置时所有 Allow 直接通过，所有 Record 是 no-op。
type OwnerLimiter struct {
	mu sync.Mutex

	cfg LimiterConfig

	// key = ownerKey(owner, provider)
	breakers map[string]*breakerState
	buckets  map[string]*bucketState
	now      func() time.Time
}

// LimiterConfig 控制熔断器与令牌桶的参数。所有 0 值代表"禁用该项"。
type LimiterConfig struct {
	// 熔断
	BreakerFailureThreshold int           // 连续失败次数 ≥ 此值 → open
	BreakerOpenDuration     time.Duration // open 持续时间
	BreakerHalfOpenMaxCalls int           // half-open 状态下允许的探测调用数
	// 限流（令牌桶）
	BucketCapacity   int     // 桶容量
	BucketRefillRate float64 // 每秒补充的 token 数
}

// DefaultLimiterConfig 给出生产环境的安全默认值。
func DefaultLimiterConfig() LimiterConfig {
	return LimiterConfig{
		BreakerFailureThreshold: 5,
		BreakerOpenDuration:     30 * time.Second,
		BreakerHalfOpenMaxCalls: 1,
		BucketCapacity:          60,
		BucketRefillRate:        2.0, // 2 RPS 稳态，60 个 burst
	}
}

// ErrCircuitOpen 当某个 (owner, provider) 处于熔断打开状态时返回。
var ErrCircuitOpen = errors.New("llm: circuit breaker open for owner/provider")

// ErrRateLimited 当 owner 当前 provider 的令牌桶耗尽时返回。
var ErrRateLimited = errors.New("llm: owner rate limit exceeded for provider")

// NewOwnerLimiter 创建 limiter。cfg 中字段为 0 时该子项被禁用。
func NewOwnerLimiter(cfg LimiterConfig) *OwnerLimiter {
	return &OwnerLimiter{
		cfg:      cfg,
		breakers: make(map[string]*breakerState),
		buckets:  make(map[string]*bucketState),
		now:      time.Now,
	}
}

// Allow 必须在每次外发调用前调用。
// 返回 nil 表示放行，并占用 1 个 token / breaker probe slot。
// owner 或 provider 为空时直接放行（如内部探活、未登录调用等）。
func (l *OwnerLimiter) Allow(owner, provider string) error {
	if l == nil {
		return nil
	}
	if owner == "" || provider == "" {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	key := ownerProviderKey(owner, provider)

	// 1) 熔断检查
	if l.cfg.BreakerFailureThreshold > 0 {
		b := l.breakerLocked(key)
		switch b.state {
		case breakerOpen:
			if now.Before(b.openUntil) {
				return fmt.Errorf("%w: owner=%s provider=%s reopen_in=%s", ErrCircuitOpen, owner, provider, b.openUntil.Sub(now))
			}
			// 自然过渡到 half-open
			b.state = breakerHalfOpen
			b.halfOpenInFlight = 0
		case breakerHalfOpen:
			if b.halfOpenInFlight >= max1(l.cfg.BreakerHalfOpenMaxCalls) {
				return fmt.Errorf("%w: owner=%s provider=%s state=half_open_full", ErrCircuitOpen, owner, provider)
			}
		}
		if b.state == breakerHalfOpen {
			b.halfOpenInFlight++
		}
	}

	// 2) 令牌桶
	if l.cfg.BucketCapacity > 0 && l.cfg.BucketRefillRate > 0 {
		bk := l.bucketLocked(key)
		bk.refill(now, l.cfg)
		if bk.tokens < 1 {
			return fmt.Errorf("%w: owner=%s provider=%s", ErrRateLimited, owner, provider)
		}
		bk.tokens--
	}

	return nil
}

// RecordSuccess 在调用成功后调用。
func (l *OwnerLimiter) RecordSuccess(owner, provider string) {
	if l == nil || owner == "" || provider == "" {
		return
	}
	if l.cfg.BreakerFailureThreshold <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := ownerProviderKey(owner, provider)
	b := l.breakerLocked(key)
	b.consecutiveFailures = 0
	b.state = breakerClosed
	b.halfOpenInFlight = 0
}

// RecordFailure 在调用失败后调用。
// 调用方应只对"被认为是熔断信号"的错误调用此方法（5xx、超时、429）。
func (l *OwnerLimiter) RecordFailure(owner, provider string) {
	if l == nil || owner == "" || provider == "" {
		return
	}
	if l.cfg.BreakerFailureThreshold <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	key := ownerProviderKey(owner, provider)
	b := l.breakerLocked(key)
	b.consecutiveFailures++
	b.halfOpenInFlight = 0
	if b.consecutiveFailures >= l.cfg.BreakerFailureThreshold {
		b.state = breakerOpen
		b.openUntil = now.Add(l.cfg.BreakerOpenDuration)
	}
}

// State 返回 (owner, provider) 当前熔断器状态字符串。仅供观测/测试。
func (l *OwnerLimiter) State(owner, provider string) string {
	if l == nil {
		return "closed"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.breakers[ownerProviderKey(owner, provider)]
	if !ok {
		return "closed"
	}
	switch b.state {
	case breakerOpen:
		if l.now().After(b.openUntil) {
			return "half_open"
		}
		return "open"
	case breakerHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// IsCircuitOpen 是 ErrCircuitOpen 的便捷判定。
func IsCircuitOpen(err error) bool {
	return errors.Is(err, ErrCircuitOpen)
}

// IsRateLimited 是 ErrRateLimited 的便捷判定。
func IsRateLimited(err error) bool {
	return errors.Is(err, ErrRateLimited)
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

type breakerStateValue int

const (
	breakerClosed breakerStateValue = iota
	breakerOpen
	breakerHalfOpen
)

type breakerState struct {
	state               breakerStateValue
	consecutiveFailures int
	openUntil           time.Time
	halfOpenInFlight    int
}

type bucketState struct {
	tokens     float64
	lastRefill time.Time
}

func (b *bucketState) refill(now time.Time, cfg LimiterConfig) {
	if b.lastRefill.IsZero() {
		b.tokens = float64(cfg.BucketCapacity)
		b.lastRefill = now
		return
	}
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed * cfg.BucketRefillRate
	if b.tokens > float64(cfg.BucketCapacity) {
		b.tokens = float64(cfg.BucketCapacity)
	}
	b.lastRefill = now
}

func (l *OwnerLimiter) breakerLocked(key string) *breakerState {
	b, ok := l.breakers[key]
	if !ok {
		b = &breakerState{state: breakerClosed}
		l.breakers[key] = b
	}
	return b
}

func (l *OwnerLimiter) bucketLocked(key string) *bucketState {
	b, ok := l.buckets[key]
	if !ok {
		b = &bucketState{tokens: float64(l.cfg.BucketCapacity), lastRefill: l.now()}
		l.buckets[key] = b
	}
	return b
}

func ownerProviderKey(owner, provider string) string {
	return owner + "|" + provider
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}
