package llm

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrCallBudgetExceeded is returned when an owner+step has exhausted its
// short-window LLM call budget. Callers should fall back to deterministic or
// lower-cost logic when possible.
var ErrCallBudgetExceeded = errors.New("llm: call budget exceeded for owner/step")

// CallBudgetConfig controls short-window LLM call budgets. Zero values disable
// the budget limiter.
type CallBudgetConfig struct {
	Window       time.Duration
	DefaultLimit int
	StepLimits   map[string]int
}

// DefaultCallBudgetConfig provides conservative production defaults that catch
// runaway duplicate workflows while leaving normal daily runs unaffected.
func DefaultCallBudgetConfig() CallBudgetConfig {
	return CallBudgetConfig{
		Window:       time.Minute,
		DefaultLimit: 60,
		StepLimits: map[string]int{
			"pm_plan":            12,
			"daily_review":       20,
			"roundtable_summary": 24,
			"roundtable_opinion": 60,
			"research_parallel":  80,
		},
	}
}

// CallBudgetLimiter tracks how many real provider calls are allowed for a
// given owner+step in a rolling-ish fixed window. Cache hits and coalesced
// followers do not call Allow, so they do not consume budget.
type CallBudgetLimiter struct {
	mu      sync.Mutex
	cfg     CallBudgetConfig
	now     func() time.Time
	windows map[string]*callBudgetWindow
}

type callBudgetWindow struct {
	startedAt time.Time
	count     int
}

func NewCallBudgetLimiter(cfg CallBudgetConfig) *CallBudgetLimiter {
	limits := make(map[string]int, len(cfg.StepLimits))
	for k, v := range cfg.StepLimits {
		limits[strings.TrimSpace(k)] = v
	}
	cfg.StepLimits = limits
	return &CallBudgetLimiter{cfg: cfg, now: time.Now, windows: make(map[string]*callBudgetWindow)}
}

func (l *CallBudgetLimiter) Allow(owner, step string) error {
	if l == nil {
		return nil
	}
	owner = strings.TrimSpace(owner)
	step = normalizeBudgetStep(step)
	if owner == "" || step == "" || l.cfg.Window <= 0 {
		return nil
	}
	limit := l.limitFor(step)
	if limit <= 0 {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	key := owner + "|" + step
	w := l.windows[key]
	if w == nil || now.Sub(w.startedAt) >= l.cfg.Window {
		w = &callBudgetWindow{startedAt: now}
		l.windows[key] = w
	}
	if w.count >= limit {
		return fmt.Errorf("%w: owner=%s step=%s limit=%d window=%s", ErrCallBudgetExceeded, owner, step, limit, l.cfg.Window)
	}
	w.count++
	l.cleanupLocked(now)
	return nil
}

func (l *CallBudgetLimiter) limitFor(step string) int {
	if l.cfg.StepLimits != nil {
		if limit, ok := l.cfg.StepLimits[step]; ok {
			return limit
		}
	}
	return l.cfg.DefaultLimit
}

func (l *CallBudgetLimiter) cleanupLocked(now time.Time) {
	if len(l.windows) <= 1024 {
		return
	}
	for key, w := range l.windows {
		if now.Sub(w.startedAt) >= l.cfg.Window {
			delete(l.windows, key)
		}
	}
}

func normalizeBudgetStep(step string) string {
	step = strings.TrimSpace(step)
	if step == "" {
		return "unknown"
	}
	return step
}

func IsCallBudgetExceeded(err error) bool {
	return errors.Is(err, ErrCallBudgetExceeded)
}
