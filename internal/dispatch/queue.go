// Package dispatch holds the queue seam (ADR-004): a versioned envelope that
// carries ids only, and the JobQueue/Delivery interfaces. SQS lives in
// dispatch/sqsqueue (the only package that imports the SQS SDK) and an
// in-memory queue in dispatch/inmem for unit tests. Kafka would be one more
// implementation: the group/partition key is always tenant_id, delays go
// through Retry (≤ 15 min), and ordering is never assumed.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/store"
)

// EnvelopeVersion is the current envelope version. Consumers accept the
// current and the previous version during a rollout (only v1 exists).
const EnvelopeVersion = 1

// MaxDelay is the longest queue delay (SQS DelaySeconds); anything longer
// goes through scheduled_dispatch.
const MaxDelay = 15 * time.Minute

// Channels that have a dispatch queue.
var Channels = []string{"email", "sms", "push", "slack", "outlook", "whatsapp", "webhook"}

// ErrBadEnvelope means a message body isn't a valid envelope (poison).
var ErrBadEnvelope = errors.New("dispatch: invalid envelope")

// Envelope is the queue message: ids only, never payload, recipients or
// rendered content (ADR-004).
type Envelope struct {
	V              int       `json:"v"`
	TenantID       uuid.UUID `json:"tenant_id"`
	RunID          uuid.UUID `json:"run_id"`
	NotificationID uuid.UUID `json:"notification_id"`
	Channel        string    `json:"channel"`
	Attempt        int       `json:"attempt"`
	EnqueuedAt     time.Time `json:"enqueued_at"`
	TraceParent    string    `json:"traceparent,omitempty"`
}

func validChannel(c string) bool {
	for _, x := range Channels {
		if x == c {
			return true
		}
	}
	return false
}

// Validate checks an envelope's fields.
func (e Envelope) Validate() error {
	switch {
	case e.V != EnvelopeVersion:
		return fmt.Errorf("%w: version %d", ErrBadEnvelope, e.V)
	case e.TenantID == uuid.Nil || e.RunID == uuid.Nil || e.NotificationID == uuid.Nil:
		return fmt.Errorf("%w: missing id", ErrBadEnvelope)
	case !validChannel(e.Channel):
		return fmt.Errorf("%w: channel %q", ErrBadEnvelope, e.Channel)
	case e.Attempt < 1:
		return fmt.Errorf("%w: attempt %d", ErrBadEnvelope, e.Attempt)
	case len(e.TraceParent) > 64:
		return fmt.Errorf("%w: traceparent too long", ErrBadEnvelope)
	}
	return nil
}

// Encode serializes a valid envelope.
func Encode(e Envelope) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("dispatch: encode: %w", err)
	}
	return b, nil
}

// Decode parses and validates a message body. Unknown fields are rejected,
// so nobody can smuggle data (payload, PII) through the queue.
func Decode(b []byte) (Envelope, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var e Envelope
	if err := dec.Decode(&e); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrBadEnvelope, err)
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// OutboundMessage is an envelope to publish, with an optional delay.
type OutboundMessage struct {
	Envelope Envelope
	Delay    time.Duration
}

// Delivery is one received message.
type Delivery interface {
	Envelope() Envelope
	// ReceiveCount is how many times the queue has delivered this message.
	ReceiveCount() int
	// Ack removes the message (done, or handed off elsewhere).
	Ack(ctx context.Context) error
	// Extend keeps the message invisible for d more (heartbeat while sending).
	Extend(ctx context.Context, d time.Duration) error
	// Retry republishes the envelope with attempt+1 after delay (≤ MaxDelay)
	// and acks this delivery.
	Retry(ctx context.Context, delay time.Duration) error
}

// Handler processes one delivery. It must Ack, Retry or (to let the
// visibility timeout redeliver) do nothing.
type Handler func(ctx context.Context, d Delivery)

// JobQueue publishes and consumes envelopes.
type JobQueue interface {
	Publish(ctx context.Context, msgs []OutboundMessage) error
	// Consume blocks, handing deliveries for channel to h, until ctx is done.
	Consume(ctx context.Context, channel string, h Handler) error
}

// FromOutbox builds the outbound message for an outbox row.
func FromOutbox(m store.OutboxMessage, now time.Time) OutboundMessage {
	attempt := m.Attempt
	if attempt < 1 {
		attempt = 1
	}
	return OutboundMessage{
		Envelope: Envelope{
			V: EnvelopeVersion, TenantID: m.TenantID.UUID(), RunID: m.RunID, NotificationID: m.NotificationID,
			Channel: m.Channel, Attempt: attempt, EnqueuedAt: now.UTC(), TraceParent: m.TraceParent,
		},
		Delay: time.Duration(m.DelaySeconds) * time.Second,
	}
}

// OutboxPublisher adapts a JobQueue to the trigger service's after-commit
// fast path (trigger.Publisher).
type OutboxPublisher struct {
	Q   JobQueue
	Now func() time.Time
}

// Publish implements trigger.Publisher.
func (p OutboxPublisher) Publish(ctx context.Context, msgs []store.OutboxMessage) error {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	out := make([]OutboundMessage, len(msgs))
	for i, m := range msgs {
		out[i] = FromOutbox(m, now())
	}
	return p.Q.Publish(ctx, out)
}
