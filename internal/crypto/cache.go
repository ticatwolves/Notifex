package crypto

import (
	"bytes"
	"container/list"
	"sync"
	"time"

	"github.com/google/uuid"
)

// dekCache is a bounded LRU of unwrapped DEKs with a TTL. Bounding size caps
// how much key material sits in memory; the TTL caps how long a shredded or
// rotated key stays usable on other instances. Evicted keys are zeroed (best
// effort: the Go runtime may have copied them).
type dekCache struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	now   func() time.Time
	ll    *list.List // front = most recently used
	items map[uuid.UUID]*list.Element
}

type cacheEntry struct {
	tenant  uuid.UUID
	key     []byte
	version uint8
	expires time.Time
}

func newDEKCache(maxEntries int, ttl time.Duration, now func() time.Time) *dekCache {
	return &dekCache{max: maxEntries, ttl: ttl, now: now, ll: list.New(), items: map[uuid.UUID]*list.Element{}}
}

// get returns a copy of the cached key, taken under the lock, so a concurrent
// eviction (which zeroes the cached slice) can never change a key mid-use.
// Callers zero the copy when done.
func (c *dekCache) get(tenant uuid.UUID) ([]byte, uint8, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[tenant]
	if !ok {
		return nil, 0, false
	}
	e := el.Value.(*cacheEntry)
	if !c.now().Before(e.expires) {
		c.removeLocked(el)
		return nil, 0, false
	}
	c.ll.MoveToFront(el)
	return bytes.Clone(e.key), e.version, true
}

func (c *dekCache) put(tenant uuid.UUID, key []byte, version uint8) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[tenant]; ok {
		c.removeLocked(el)
	}
	c.items[tenant] = c.ll.PushFront(&cacheEntry{tenant: tenant, key: key, version: version, expires: c.now().Add(c.ttl)})
	for c.ll.Len() > c.max {
		c.removeLocked(c.ll.Back())
	}
}

func (c *dekCache) forget(tenant uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[tenant]; ok {
		c.removeLocked(el)
	}
}

func (c *dekCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *dekCache) removeLocked(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.ll.Remove(el)
	delete(c.items, e.tenant)
	// Safe to zero: get only ever hands out copies.
	clear(e.key)
}
