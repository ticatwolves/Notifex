package sqsqueue_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/dispatch"
	"github.com/ticatwolves/notifex/internal/dispatch/sqsqueue"
	"github.com/ticatwolves/notifex/internal/dispatch/sqsqueue/sqstest"
)

func env(channel string) dispatch.Envelope {
	return dispatch.Envelope{V: dispatch.EnvelopeVersion, TenantID: uuid.Must(uuid.NewV7()), RunID: uuid.Must(uuid.NewV7()),
		NotificationID: uuid.Must(uuid.NewV7()), Channel: channel, Attempt: 1, EnqueuedAt: time.Now().UTC()}
}

func newQueue(t *testing.T, visibility time.Duration, maxReceive int) (*sqsqueue.Queue, *sqs.Client, string) {
	t.Helper()
	c := sqstest.Client(t)
	prefix := sqstest.Prefix()
	require.NoError(t, sqsqueue.EnsureQueues(context.Background(), c, prefix, visibility, maxReceive))
	return sqsqueue.New(c, sqsqueue.Options{Prefix: prefix, FairQueues: true, Visibility: visibility, Wait: time.Second, Concurrency: 4}), c, prefix
}

// collector consumes a channel in the background and records deliveries.
type collector struct {
	mu   sync.Mutex
	got  []dispatch.Delivery
	stop context.CancelFunc
	done chan struct{}
}

func consume(t *testing.T, q *sqsqueue.Queue, channel string, h func(d dispatch.Delivery)) *collector {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c := &collector{stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		_ = q.Consume(ctx, channel, func(ctx context.Context, d dispatch.Delivery) {
			c.mu.Lock()
			c.got = append(c.got, d)
			c.mu.Unlock()
			if h != nil {
				h(d)
			}
		})
	}()
	t.Cleanup(func() { cancel(); <-c.done })
	return c
}

func (c *collector) waitFor(t *testing.T, n int, within time.Duration) []dispatch.Delivery {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.got) >= n {
			out := append([]dispatch.Delivery(nil), c.got...)
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("got %d deliveries, want %d", len(c.got), n)
	return nil
}

func (c *collector) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.got) }

func TestPublishConsumeAck(t *testing.T) {
	t.Parallel()
	q, _, _ := newQueue(t, 5*time.Second, 5)
	ctx := context.Background()
	var msgs []dispatch.OutboundMessage
	for range 12 { // more than one SendMessageBatch
		msgs = append(msgs, dispatch.OutboundMessage{Envelope: env("email")})
	}
	require.NoError(t, q.Publish(ctx, msgs))
	c := consume(t, q, "email", func(d dispatch.Delivery) { assert.NoError(t, d.Ack(context.Background())) })
	got := c.waitFor(t, 12, 15*time.Second)
	seen := map[uuid.UUID]bool{}
	for _, d := range got {
		seen[d.Envelope().NotificationID] = true
		assert.Equal(t, 1, d.ReceiveCount())
	}
	assert.Len(t, seen, 12)
	time.Sleep(6 * time.Second) // past the visibility timeout
	assert.Equal(t, 12, c.count(), "acked messages are never redelivered")
}

func TestDelayAndRetry(t *testing.T) {
	t.Parallel()
	q, _, _ := newQueue(t, 5*time.Second, 5)
	ctx := context.Background()
	start := time.Now()
	require.NoError(t, q.Publish(ctx, []dispatch.OutboundMessage{{Envelope: env("sms"), Delay: 3 * time.Second}}))
	require.Error(t, q.Publish(ctx, []dispatch.OutboundMessage{{Envelope: env("sms"), Delay: 16 * time.Minute}}), "max 15 minutes")

	var once sync.Once
	c := consume(t, q, "sms", func(d dispatch.Delivery) {
		once.Do(func() { assert.NoError(t, d.Retry(context.Background(), 2*time.Second)) })
		if d.Envelope().Attempt == 2 {
			assert.NoError(t, d.Ack(context.Background()))
		}
	})
	got := c.waitFor(t, 2, 20*time.Second)
	assert.GreaterOrEqual(t, time.Since(start), 5*time.Second, "3 s delay, then a 2 s retry delay")
	assert.Equal(t, 1, got[0].Envelope().Attempt)
	assert.Equal(t, 2, got[1].Envelope().Attempt, "retry increments the attempt")
	assert.Equal(t, got[0].Envelope().NotificationID, got[1].Envelope().NotificationID)
}

// TestRedeliveryAfterVisibilityTimeout: a consumer that crashes (never acks)
// gets the message back after the visibility timeout; Extend holds it.
func TestRedeliveryAfterVisibilityTimeout(t *testing.T) {
	t.Parallel()
	q, _, _ := newQueue(t, 2*time.Second, 5)
	ctx := context.Background()
	require.NoError(t, q.Publish(ctx, []dispatch.OutboundMessage{{Envelope: env("slack")}}))
	c := consume(t, q, "slack", nil) // never acks
	got := c.waitFor(t, 2, 15*time.Second)
	assert.Equal(t, got[0].Envelope().NotificationID, got[1].Envelope().NotificationID)
	assert.Equal(t, 2, got[1].ReceiveCount())
	c.stop()
	<-c.done

	// Extend keeps it invisible past the original timeout.
	q2, _, _ := newQueue(t, 2*time.Second, 5)
	require.NoError(t, q2.Publish(ctx, []dispatch.OutboundMessage{{Envelope: env("slack")}}))
	c2 := consume(t, q2, "slack", func(d dispatch.Delivery) {
		if d.ReceiveCount() == 1 {
			assert.NoError(t, d.Extend(context.Background(), 8*time.Second))
		}
	})
	c2.waitFor(t, 1, 10*time.Second)
	time.Sleep(4 * time.Second)
	assert.Equal(t, 1, c2.count(), "extended: not redelivered after the 2 s timeout")
}

func TestDuplicatesAndPoison(t *testing.T) {
	t.Parallel()
	q, c, prefix := newQueue(t, time.Second, 2)
	ctx := context.Background()

	// The queue doesn't dedupe: the same envelope twice is two deliveries
	// (the dispatcher's claim makes the second harmless).
	e := env("webhook")
	require.NoError(t, q.Publish(ctx, []dispatch.OutboundMessage{{Envelope: e}}))
	require.NoError(t, q.Publish(ctx, []dispatch.OutboundMessage{{Envelope: e}}))

	// A message that isn't an envelope never reaches the handler and lands
	// in the DLQ after maxReceiveCount (2).
	u, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(prefix + "webhook")})
	require.NoError(t, err)
	_, err = c.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: u.QueueUrl, MessageBody: aws.String(`{"payload":"pii"}`)})
	require.NoError(t, err)

	col := consume(t, q, "webhook", func(d dispatch.Delivery) { assert.NoError(t, d.Ack(context.Background())) })
	got := col.waitFor(t, 2, 15*time.Second)
	assert.Equal(t, got[0].Envelope().NotificationID, got[1].Envelope().NotificationID)

	dlq, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(prefix + "webhook-dlq")})
	require.NoError(t, err)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: dlq.QueueUrl, WaitTimeSeconds: 1})
		require.NoError(t, err)
		if len(out.Messages) == 1 {
			assert.Equal(t, `{"payload":"pii"}`, aws.ToString(out.Messages[0].Body))
			assert.Equal(t, 2, col.count(), "the poison message never reached the handler")
			return
		}
	}
	t.Fatal("poison message never reached the DLQ")
}

func TestEnvelopeRules(t *testing.T) {
	t.Parallel()
	good := env("email")
	b, err := dispatch.Encode(good)
	require.NoError(t, err)
	back, err := dispatch.Decode(b)
	require.NoError(t, err)
	assert.Equal(t, good.NotificationID, back.NotificationID)
	for name, body := range map[string]string{
		"extra field (no smuggling)": `{"v":1,"tenant_id":"` + good.TenantID.String() + `","run_id":"` + good.RunID.String() + `","notification_id":"` + good.NotificationID.String() + `","channel":"email","attempt":1,"enqueued_at":"2026-01-01T00:00:00Z","recipient":"a@b.c"}`,
		"wrong version":              `{"v":2,"tenant_id":"` + good.TenantID.String() + `","run_id":"` + good.RunID.String() + `","notification_id":"` + good.NotificationID.String() + `","channel":"email","attempt":1,"enqueued_at":"2026-01-01T00:00:00Z"}`,
		"unknown channel":            `{"v":1,"tenant_id":"` + good.TenantID.String() + `","run_id":"` + good.RunID.String() + `","notification_id":"` + good.NotificationID.String() + `","channel":"fax","attempt":1,"enqueued_at":"2026-01-01T00:00:00Z"}`,
		"missing ids":                `{"v":1,"channel":"email","attempt":1}`,
		"not json":                   `nope`,
	} {
		_, err := dispatch.Decode([]byte(body))
		require.ErrorIs(t, err, dispatch.ErrBadEnvelope, name)
	}
}
