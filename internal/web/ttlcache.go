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
// Expired entries are evicted lazily on Get. When cap is reached, all entries
// are cleared to avoid tracking LRU order.
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
		c.m = make(map[string]ttlEntry[V], c.cap)
	}
	c.m[key] = ttlEntry[V]{value: value, expires: time.Now().Add(ttl)}
}
