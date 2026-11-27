// Package ratelimit provides per-key token buckets with bounded memory.
//
// Limits are per process. They complement, not replace, the WAF rate rules
// in front of the ALB and the Postgres-backed per-account backoff, which are
// shared across instances.
package ratelimit

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a set of token buckets keyed by string (e.g. client IP).
type Limiter struct {
	rate  rate.Limit
	burst int
	max   int
	now   func() time.Time

	mu    sync.Mutex
	ll    *list.List
	items map[string]*list.Element
}

type entry struct {
	key string
	lim *rate.Limiter
}

// New allows `perMinute` events per key per minute with the given burst,
// tracking at most maxKeys keys (least recently used evicted first).
func New(perMinute, burst, maxKeys int) *Limiter {
	return &Limiter{
		rate: rate.Limit(float64(perMinute) / 60), burst: burst, max: maxKeys, now: time.Now,
		ll: list.New(), items: map[string]*list.Element{},
	}
}

// Allow consumes one token for key. When it returns false, retryAfter says
// how long until a token is available.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, found := l.items[key]
	if found {
		l.ll.MoveToFront(el)
	} else {
		el = l.ll.PushFront(&entry{key: key, lim: rate.NewLimiter(l.rate, l.burst)})
		l.items[key] = el
		for l.ll.Len() > l.max {
			old := l.ll.Back()
			l.ll.Remove(old)
			delete(l.items, old.Value.(*entry).key)
		}
	}
	lim := el.Value.(*entry).lim
	now := l.now()
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Minute
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// Blocked reports, without consuming a token, whether key is out of tokens.
func (l *Limiter) Blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, found := l.items[key]
	if !found {
		return false, 0
	}
	tokens := el.Value.(*entry).lim.TokensAt(l.now())
	if tokens >= 1 {
		return false, 0
	}
	return true, time.Duration((1 - tokens) / float64(l.rate) * float64(time.Second))
}
