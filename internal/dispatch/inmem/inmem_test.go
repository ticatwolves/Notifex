package inmem_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/dispatch"
	"github.com/ticatwolves/notifex/internal/dispatch/inmem"
	"github.com/ticatwolves/notifex/internal/store"
)

func TestInMemoryQueueSemantics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	q := inmem.New()
	q.SetClock(func() time.Time { return now })
	q.SetVisibility(30 * time.Second)

	e := dispatch.Envelope{V: 1, TenantID: uuid.New(), RunID: uuid.New(), NotificationID: uuid.New(), Channel: "email", Attempt: 1}
	require.NoError(t, q.Publish(ctx, []dispatch.OutboundMessage{{Envelope: e, Delay: 10 * time.Second}}))
	require.Error(t, q.Publish(ctx, []dispatch.OutboundMessage{{Envelope: e, Delay: 16 * time.Minute}}))
	require.Error(t, q.Publish(ctx, []dispatch.OutboundMessage{{Envelope: dispatch.Envelope{V: 1, Channel: "email"}}}), "invalid envelope")

	assert.Nil(t, q.ReceiveOne("email"), "delayed")
	now = now.Add(10 * time.Second)
	d := q.ReceiveOne("email")
	require.NotNil(t, d)
	assert.Equal(t, 1, d.ReceiveCount())
	assert.Nil(t, q.ReceiveOne("email"), "invisible while held")

	now = now.Add(31 * time.Second) // not acked: redelivered
	d = q.ReceiveOne("email")
	require.NotNil(t, d)
	assert.Equal(t, 2, d.ReceiveCount())
	require.NoError(t, d.Extend(ctx, time.Minute))
	now = now.Add(45 * time.Second)
	assert.Nil(t, q.ReceiveOne("email"), "extended")

	require.NoError(t, d.Retry(ctx, 5*time.Second))
	assert.Equal(t, 1, q.Len("email"), "retry replaces the message")
	now = now.Add(5 * time.Second)
	d = q.ReceiveOne("email")
	require.NotNil(t, d)
	assert.Equal(t, 2, d.Envelope().Attempt)
	require.NoError(t, d.Ack(ctx))
	require.NoError(t, d.Ack(ctx), "acking twice is harmless")
	assert.Equal(t, 0, q.Len("email"))

	// The outbox adapter publishes rows as envelopes.
	pub := dispatch.OutboxPublisher{Q: q, Now: func() time.Time { return now }}
	require.NoError(t, pub.Publish(ctx, []store.OutboxMessage{{TenantID: store.TenantID(uuid.New()), NotificationID: uuid.New(), RunID: uuid.New(), Channel: "sms", DelaySeconds: 0}}))
	assert.Equal(t, 1, q.Len("sms"))
	assert.Equal(t, 1, q.Peek("sms")[0].Attempt)
}
