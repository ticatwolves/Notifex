// Package sqsqueue implements dispatch.JobQueue on Amazon SQS standard
// queues (ADR-004): one queue per channel (prefix + channel) plus a DLQ for
// poison messages, MessageGroupId = tenant_id for fair queues, delays up to
// 15 minutes. It is the only package that imports the SQS SDK.
package sqsqueue

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ticatwolves/notifex/internal/dispatch"
)

// Options configures a Queue.
type Options struct {
	// Prefix is the queue name prefix: notifex-{env}-dispatch-.
	Prefix string
	// FairQueues sets MessageGroupId = tenant_id (SQS fair queues). Off only
	// for emulators that reject group ids on standard queues.
	FairQueues bool
	// Visibility is how long a received message stays invisible before
	// redelivery (default 60 s; heartbeats extend it).
	Visibility time.Duration
	// Wait is the long-poll wait (default 20 s).
	Wait time.Duration
	// Concurrency is the number of in-flight handlers per channel (default 10).
	Concurrency int
	Log         *slog.Logger
	Metrics     prometheus.Registerer
}

// Queue is the SQS JobQueue.
type Queue struct {
	c    *sqs.Client
	opts Options
	urls sync.Map // channel → queue URL

	published, received, acked, retried, poison *prometheus.CounterVec
}

// NewClient builds an SQS client. endpoint overrides the AWS endpoint
// (ElasticMQ locally); empty in AWS.
func NewClient(ctx context.Context, region, endpoint string) (*sqs.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("sqsqueue: aws config: %w", err)
	}
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	}), nil
}

// New returns a Queue.
func New(c *sqs.Client, o Options) *Queue {
	if o.Visibility <= 0 {
		o.Visibility = 60 * time.Second
	}
	if o.Wait <= 0 {
		o.Wait = 20 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 10
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	vec := func(name, help string) *prometheus.CounterVec {
		return prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, []string{"channel"})
	}
	q := &Queue{
		c: c, opts: o,
		published: vec("notifex_queue_published_total", "Messages published to SQS."),
		received:  vec("notifex_queue_received_total", "Messages received from SQS."),
		acked:     vec("notifex_queue_acked_total", "Messages deleted after handling."),
		retried:   vec("notifex_queue_retried_total", "Messages republished with a delay."),
		poison:    vec("notifex_queue_poison_total", "Undecodable messages left for the DLQ."),
	}
	if o.Metrics != nil {
		o.Metrics.MustRegister(q.published, q.received, q.acked, q.retried, q.poison)
	}
	return q
}

// QueueName is the queue for a channel.
func (q *Queue) QueueName(channel string) string { return q.opts.Prefix + channel }

func (q *Queue) url(ctx context.Context, channel string) (string, error) {
	if u, ok := q.urls.Load(channel); ok {
		return u.(string), nil
	}
	out, err := q.c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(q.QueueName(channel))})
	if err != nil {
		return "", fmt.Errorf("sqsqueue: queue %s: %w", q.QueueName(channel), err)
	}
	q.urls.Store(channel, *out.QueueUrl)
	return *out.QueueUrl, nil
}

func delaySeconds(d time.Duration) (int32, error) {
	if d < 0 || d > dispatch.MaxDelay {
		return 0, fmt.Errorf("sqsqueue: delay %s outside 0-15m", d)
	}
	return int32(d.Round(time.Second) / time.Second), nil //nolint:gosec // G115: ≤ 900
}

func (q *Queue) entry(id int, m dispatch.OutboundMessage) (types.SendMessageBatchRequestEntry, error) {
	body, err := dispatch.Encode(m.Envelope)
	if err != nil {
		return types.SendMessageBatchRequestEntry{}, err
	}
	delay, err := delaySeconds(m.Delay)
	if err != nil {
		return types.SendMessageBatchRequestEntry{}, err
	}
	e := types.SendMessageBatchRequestEntry{Id: aws.String(strconv.Itoa(id)), MessageBody: aws.String(string(body)), DelaySeconds: delay}
	if q.opts.FairQueues {
		e.MessageGroupId = aws.String(m.Envelope.TenantID.String())
	}
	return e, nil
}

// Publish implements dispatch.JobQueue: batches of 10 per channel. Any
// failed entry fails the call (the caller leaves the outbox rows for the
// relay; a duplicate publish is harmless).
func (q *Queue) Publish(ctx context.Context, msgs []dispatch.OutboundMessage) error {
	byChannel := map[string][]dispatch.OutboundMessage{}
	for _, m := range msgs {
		byChannel[m.Envelope.Channel] = append(byChannel[m.Envelope.Channel], m)
	}
	for ch, ms := range byChannel {
		u, err := q.url(ctx, ch)
		if err != nil {
			return err
		}
		for start := 0; start < len(ms); start += 10 {
			end := min(start+10, len(ms))
			entries := make([]types.SendMessageBatchRequestEntry, 0, end-start)
			for i := start; i < end; i++ {
				e, err := q.entry(i, ms[i])
				if err != nil {
					return err
				}
				entries = append(entries, e)
			}
			out, err := q.c.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{QueueUrl: aws.String(u), Entries: entries})
			if err != nil {
				return fmt.Errorf("sqsqueue: send batch to %s: %w", ch, err)
			}
			q.published.WithLabelValues(ch).Add(float64(len(out.Successful)))
			if len(out.Failed) > 0 {
				f := out.Failed[0]
				return fmt.Errorf("sqsqueue: %d of %d messages to %s failed (first: %s %s)", len(out.Failed), len(entries), ch,
					aws.ToString(f.Code), aws.ToString(f.Message))
			}
		}
	}
	return nil
}

// Consume implements dispatch.JobQueue. Undecodable messages are left alone
// so the redrive policy moves them to the DLQ (poison only, ADR-004).
func (q *Queue) Consume(ctx context.Context, channel string, h dispatch.Handler) error {
	u, err := q.url(ctx, channel)
	if err != nil {
		return err
	}
	sem := make(chan struct{}, q.opts.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for ctx.Err() == nil {
		// Never ask for more messages than there are free handler slots.
		free := cap(sem) - len(sem)
		if free == 0 {
			select {
			case sem <- struct{}{}:
				<-sem
			case <-ctx.Done():
			}
			continue
		}
		out, err := q.c.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(u), MaxNumberOfMessages: int32(min(free, 10)), //nolint:gosec // G115: ≤ 10
			WaitTimeSeconds:             int32(q.opts.Wait / time.Second),       //nolint:gosec // G115: ≤ 20
			VisibilityTimeout:           int32(q.opts.Visibility / time.Second), //nolint:gosec // G115: small
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount},
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			q.opts.Log.WarnContext(ctx, "sqs receive failed", slog.String("channel", channel), slog.Any("error", err))
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
			}
			continue
		}
		for _, m := range out.Messages {
			q.received.WithLabelValues(channel).Inc()
			env, err := dispatch.Decode([]byte(aws.ToString(m.Body)))
			if err != nil {
				q.poison.WithLabelValues(channel).Inc()
				q.opts.Log.ErrorContext(ctx, "undecodable queue message left for the DLQ",
					slog.String("channel", channel), slog.String("message_id", aws.ToString(m.MessageId)))
				continue
			}
			count, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
			d := &delivery{q: q, channel: channel, url: u, receipt: aws.ToString(m.ReceiptHandle), env: env, receives: count}
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()
				h(ctx, d)
			}()
		}
	}
	return nil
}

type delivery struct {
	q        *Queue
	channel  string
	url      string
	receipt  string
	env      dispatch.Envelope
	receives int
}

func (d *delivery) Envelope() dispatch.Envelope { return d.env }
func (d *delivery) ReceiveCount() int           { return d.receives }

func (d *delivery) Ack(ctx context.Context) error {
	if _, err := d.q.c.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(d.url), ReceiptHandle: aws.String(d.receipt)}); err != nil {
		return fmt.Errorf("sqsqueue: delete: %w", err)
	}
	d.q.acked.WithLabelValues(d.channel).Inc()
	return nil
}

func (d *delivery) Extend(ctx context.Context, dur time.Duration) error {
	if _, err := d.q.c.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(d.url), ReceiptHandle: aws.String(d.receipt), VisibilityTimeout: int32(dur / time.Second), //nolint:gosec // G115: small
	}); err != nil {
		return fmt.Errorf("sqsqueue: extend visibility: %w", err)
	}
	return nil
}

func (d *delivery) Retry(ctx context.Context, delay time.Duration) error {
	env := d.env
	env.Attempt++
	env.EnqueuedAt = time.Now().UTC()
	if err := d.q.Publish(ctx, []dispatch.OutboundMessage{{Envelope: env, Delay: delay}}); err != nil {
		return err
	}
	d.q.retried.WithLabelValues(d.channel).Inc()
	return d.Ack(ctx)
}

// EnsureQueues creates each channel's queue and DLQ if missing (local dev and
// tests; Terraform owns them in AWS). maxReceive is the DLQ redrive count.
func EnsureQueues(ctx context.Context, c *sqs.Client, prefix string, visibility time.Duration, maxReceive int) error {
	for _, ch := range dispatch.Channels {
		dlq, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(prefix + ch + "-dlq")})
		if err != nil {
			return fmt.Errorf("sqsqueue: create dlq for %s: %w", ch, err)
		}
		attrs, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: dlq.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
		})
		if err != nil {
			return fmt.Errorf("sqsqueue: dlq arn for %s: %w", ch, err)
		}
		redrive := fmt.Sprintf(`{"deadLetterTargetArn":%q,"maxReceiveCount":"%d"}`, attrs.Attributes[string(types.QueueAttributeNameQueueArn)], maxReceive)
		if _, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{
			QueueName: aws.String(prefix + ch),
			Attributes: map[string]string{
				string(types.QueueAttributeNameVisibilityTimeout): strconv.Itoa(int(visibility / time.Second)),
				string(types.QueueAttributeNameRedrivePolicy):     redrive,
			},
		}); err != nil {
			return fmt.Errorf("sqsqueue: create queue for %s: %w", ch, err)
		}
	}
	return nil
}
