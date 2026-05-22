package marketdata

import (
	"sync"
	"time"
)

type ttlCache[T any] struct {
	mu    sync.RWMutex
	items map[string]cacheItem[T]
}

type cacheItem[T any] struct {
	value     T
	expiresAt time.Time
}

func newTTLCache[T any]() *ttlCache[T] {
	return &ttlCache[T]{items: make(map[string]cacheItem[T])}
}

func (c *ttlCache[T]) Get(key string, now time.Time) (T, bool) {
	var zero T
	if c == nil {
		return zero, false
	}
	c.mu.RLock()
	item, ok := c.items[key]
	c.mu.RUnlock()
	if !ok || now.After(item.expiresAt) {
		if ok {
			c.mu.Lock()
			delete(c.items, key)
			c.mu.Unlock()
		}
		return zero, false
	}
	return item.value, true
}

func (c *ttlCache[T]) Set(key string, value T, ttl time.Duration, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.items[key] = cacheItem[T]{value: value, expiresAt: now.Add(ttl)}
	c.mu.Unlock()
}
