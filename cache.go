package main

import (
	"sync"
	"time"
)

// ttlCache is a small in-memory cache with per-entry expiry. It exists so
// that repeatedly opening the catalog or refreshing a title's streams
// doesn't re-query every addon on every single request — within the TTL,
// a repeat request for the same catalog page or the same title's streams
// is served from memory instead of fanning back out to every addon.
//
// It intentionally has no cross-instance/shared-storage backing: VeyraHub
// runs as a single process per deployment, and losing the cache on restart
// is fine since it repopulates itself from the first real request.
type ttlCache[V any] struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]ttlEntry[V]
}

type ttlEntry[V any] struct {
	value   V
	expires time.Time
}

func newTTLCache[V any](ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, entries: make(map[string]ttlEntry[V])}
}

// get returns the cached value for key, if present and not yet expired.
func (c *ttlCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expires) {
		var zero V
		return zero, false
	}
	return entry.value, true
}

// set stores value under key, expiring it after the cache's TTL.
func (c *ttlCache[V]) set(key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// Opportunistically sweep expired entries on write so a cache backing
	// varied search/skip combinations doesn't grow without bound.
	if len(c.entries) > 512 {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = ttlEntry[V]{value: value, expires: now.Add(c.ttl)}
}

// clear drops every cached entry. Used when the underlying addon list
// changes, so a stale cache never outlives its data by more than a request.
func (c *ttlCache[V]) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]ttlEntry[V])
}
