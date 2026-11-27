package ratelimit

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(60, 3, 100) // 1/s, burst 3
	l.now = func() time.Time { return now }

	for i := range 3 {
		ok, _ := l.Allow("ip-a")
		assert.True(t, ok, "burst %d", i)
	}
	ok, retry := l.Allow("ip-a")
	assert.False(t, ok)
	assert.InDelta(t, time.Second, retry, float64(10*time.Millisecond))

	ok, _ = l.Allow("ip-b")
	assert.True(t, ok, "keys are independent")

	now = now.Add(time.Second)
	ok, _ = l.Allow("ip-a")
	assert.True(t, ok, "refilled")

	// Rejected attempts don't consume tokens (no penalty spiral).
	for range 10 {
		l.Allow("ip-a")
	}
	now = now.Add(time.Second)
	ok, _ = l.Allow("ip-a")
	assert.True(t, ok)
}

func TestBlockedDoesNotConsume(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(60, 2, 100)
	l.now = func() time.Time { return now }
	blocked, _ := l.Blocked("ip")
	assert.False(t, blocked, "unknown key")
	l.Allow("ip")
	l.Allow("ip")
	blocked, retry := l.Blocked("ip")
	assert.True(t, blocked)
	assert.InDelta(t, time.Second, retry, float64(10*time.Millisecond))
	for range 5 {
		l.Blocked("ip") // peeking never consumes
	}
	now = now.Add(time.Second)
	blocked, _ = l.Blocked("ip")
	assert.False(t, blocked)
}

func TestLimiterBoundsMemory(t *testing.T) {
	l := New(60, 1, 10)
	for i := range 1000 {
		l.Allow("k" + strconv.Itoa(i))
	}
	assert.LessOrEqual(t, l.ll.Len(), 10)
	assert.Len(t, l.items, l.ll.Len())
}
