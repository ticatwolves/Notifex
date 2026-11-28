// Package inmem is an in-memory dispatch.JobQueue for unit tests. It
// mimics SQS semantics that matter to the dispatcher: delays, a visibility
// timeout with redelivery when not acked, and receive counts.
package inmem

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ticatwolves/notifex/internal/dispatch"
)

type msg struct {
	id        int
	env       dispatch.Envelope
	visibleAt time.Time
	receives  int
}

// Queue is an in-memory JobQueue. The zero value is not usable; use New.
type Queue struct {
	mu         sync.Mutex
	next       int
	queues     map[string][]*msg
	now        func() time.Time
	visibility time.Duration
	poll       time.Duration
	published  int
}

// New returns an empty queue with a 30 s visibility timeout.
func New() *Queue {
	return &Queue{queues: map[string][]*msg{}, now: time.Now, visibility: 30 * time.Second, poll: 5 * time.Millisecond}
}

// SetClock overrides the clock (tests).
func (q *Queue) SetClock(now func() time.Time) { q.mu.Lock(); q.now = now; q.mu.Unlock() }

// SetVisibility sets the visibility timeout.
func (q *Queue) SetVisibility(d time.Duration) { q.mu.Lock(); q.visibility = d; q.mu.Unlock() }

// Publish implements dispatch.JobQueue.
func (q *Queue) Publish(_ context.Context, msgs []dispatch.OutboundMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, m := range msgs {
		if err := m.Envelope.Validate(); err != nil {
			return err
		}
		if m.Delay < 0 || m.Delay > dispatch.MaxDelay {
			return fmt.Errorf("inmem: delay %s outside 0-15m", m.Delay)
		}
		q.next++
		q.published++
		q.queues[m.Envelope.Channel] = append(q.queues[m.Envelope.Channel], &msg{id: q.next, env: m.Envelope, visibleAt: q.now().Add(m.Delay)})
	}
	return nil
}

// Len returns how many messages (visible or not) a channel holds.
func (q *Queue) Len(channel string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queues[channel])
}

// Published returns how many messages were ever published.
func (q *Queue) Published() int { q.mu.Lock(); defer q.mu.Unlock(); return q.published }

// Peek returns the envelopes in a channel (tests).
func (q *Queue) Peek(channel string) []dispatch.Envelope {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]dispatch.Envelope, len(q.queues[channel]))
	for i, m := range q.queues[channel] {
		out[i] = m.env
	}
	return out
}

// receive returns one visible message, marking it invisible.
func (q *Queue) receive(channel string) *delivery {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	for _, m := range q.queues[channel] {
		if !m.visibleAt.After(now) {
			m.receives++
			m.visibleAt = now.Add(q.visibility)
			return &delivery{q: q, channel: channel, id: m.id, env: m.env, receives: m.receives}
		}
	}
	return nil
}

// Consume implements dispatch.JobQueue (one message at a time).
func (q *Queue) Consume(ctx context.Context, channel string, h dispatch.Handler) error {
	t := time.NewTicker(q.poll)
	defer t.Stop()
	for {
		for d := q.receive(channel); d != nil; d = q.receive(channel) {
			h(ctx, d)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// ReceiveOne returns one visible delivery or nil (tests that drive
// handlers step by step).
func (q *Queue) ReceiveOne(channel string) dispatch.Delivery {
	if d := q.receive(channel); d != nil {
		return d
	}
	return nil
}

type delivery struct {
	q        *Queue
	channel  string
	id       int
	env      dispatch.Envelope
	receives int
}

func (d *delivery) Envelope() dispatch.Envelope { return d.env }
func (d *delivery) ReceiveCount() int           { return d.receives }

func (d *delivery) Ack(context.Context) error {
	d.q.mu.Lock()
	defer d.q.mu.Unlock()
	ms := d.q.queues[d.channel]
	for i, m := range ms {
		if m.id == d.id {
			d.q.queues[d.channel] = append(ms[:i], ms[i+1:]...)
			return nil
		}
	}
	return nil // already gone: acking twice is harmless
}

func (d *delivery) Extend(_ context.Context, dur time.Duration) error {
	d.q.mu.Lock()
	defer d.q.mu.Unlock()
	for _, m := range d.q.queues[d.channel] {
		if m.id == d.id {
			m.visibleAt = d.q.now().Add(dur)
		}
	}
	return nil
}

func (d *delivery) Retry(ctx context.Context, delay time.Duration) error {
	env := d.env
	env.Attempt++
	env.EnqueuedAt = d.q.now().UTC()
	if err := d.q.Publish(ctx, []dispatch.OutboundMessage{{Envelope: env, Delay: delay}}); err != nil {
		return err
	}
	return d.Ack(ctx)
}
