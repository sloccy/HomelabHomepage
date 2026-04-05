package web

import (
	"sync"
	"time"
)

type ttlEntry[V any] struct {
	value   V
	expires time.Time
}

// ttlCache is a size-capped in-memory cache with per-entry TTL expiry.
// Expired entries are evicted lazily on Get. When the cap is reached, expired
// entries are swept first; if still at cap, the oldest 25% of entries are
// removed to make room without discarding the entire cache.
type ttlCache[V any] struct {
	mu  sync.Mutex
	m   map[string]ttlEntry[V]
	cap int
}

func newTTLCache[V any](capacity int) *ttlCache[V] {
	return &ttlCache[V]{m: make(map[string]ttlEntry[V], capacity), cap: capacity}
}

// Get returns the value for key and true if it exists and has not expired.
func (c *ttlCache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		var zero V
		return zero, false
	}
	if time.Now().After(e.expires) {
		delete(c.m, key)
		var zero V
		return zero, false
	}
	return e.value, true
}

// Set stores value under key with the given TTL.
func (c *ttlCache[V]) Set(key string, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.cap {
		c.evictLocked()
	}
	c.m[key] = ttlEntry[V]{value: value, expires: time.Now().Add(ttl)}
}

// evictLocked removes expired entries; if still at cap, removes the oldest 25%.
// Must be called with c.mu held.
func (c *ttlCache[V]) evictLocked() {
	now := time.Now()
	for k, e := range c.m {
		if now.After(e.expires) {
			delete(c.m, k)
		}
	}
	if len(c.m) < c.cap {
		return
	}
	// Still at cap: find and remove the oldest 25% by expiry time.
	evict := max(1, len(c.m)/4)
	for range evict {
		oldest := ""
		var oldestExp time.Time
		for k, e := range c.m {
			if oldest == "" || e.expires.Before(oldestExp) {
				oldest = k
				oldestExp = e.expires
			}
		}
		if oldest != "" {
			delete(c.m, oldest)
		}
	}
}
