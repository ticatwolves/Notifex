// Package worker is the dispatcher's per-message handler (docs/02 §Dispatch
// pipeline, ADR-004, ADR-014 §5): claim with a lease, policy, decrypt,
// render from the run's snapshot, send through the channel registry, and
// record the outcome (status, events, usage, retries) in one transaction.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/dispatch"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/policy"
	"github.com/ticatwolves/notifex/internal/ratelimit"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
	"github.com/ticatwolves/notifex/internal/workflow"
	"github.com/ticatwolves/notifex/internal/workflow/render"
)

// Config holds the ADR-014 §5 timings.
type Config struct {
	Lease              time.Duration // claim lease (60 s)
	Heartbeat          time.Duration // lease and visibility renewal (20 s)
	MaxAge             time.Duration // after this, expired (48 h)
	BackoffBase        time.Duration // 10 s
	BackoffCap         time.Duration // 6 h
	DefaultMaxAttempts int           // 5
	SendTimeout        time.Duration // per vendor call (30 s)
}

// DefaultConfig is ADR-014 §5.
var DefaultConfig = Config{
	Lease: 60 * time.Second, Heartbeat: 20 * time.Second, MaxAge: 48 * time.Hour,
	BackoffBase: 10 * time.Second, BackoffCap: 6 * time.Hour, DefaultMaxAttempts: 5, SendTimeout: 30 * time.Second,
}

// SecretSource decrypts BYO credential secrets (credential.Service).
type SecretSource interface {
	Secret(ctx context.Context, tid store.TenantID, id uuid.UUID) (map[string]any, error)
}

// Worker handles deliveries.
type Worker struct {
	st      store.Store
	cipher  crypto.FieldCipher
	secrets SecretSource
	reg     *channel.Registry
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	jitter  func() float64

	limMu    sync.Mutex
	limiters map[string]*ratelimit.Limiter

	mOutcomes *prometheus.CounterVec
	mSend     *prometheus.HistogramVec
}

// New builds a worker.
func New(st store.Store, cipher crypto.FieldCipher, secrets SecretSource, reg *channel.Registry, cfg Config, log *slog.Logger, regs prometheus.Registerer) *Worker {
	w := &Worker{
		st: st, cipher: cipher, secrets: secrets, reg: reg, cfg: cfg, log: log, now: time.Now, jitter: rand.Float64,
		limiters: map[string]*ratelimit.Limiter{},
		mOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifex_dispatch_outcomes_total", Help: "Dispatch outcomes by channel.",
		}, []string{"channel", "outcome"}),
		mSend: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "notifex_dispatch_send_seconds", Help: "Vendor call latency by channel.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"channel"}),
	}
	if regs != nil {
		regs.MustRegister(w.mOutcomes, w.mSend)
	}
	return w
}

// SetClock overrides the clock and jitter source (tests).
func (w *Worker) SetClock(now func() time.Time, jitter func() float64) {
	w.now = now
	if jitter != nil {
		w.jitter = jitter
	}
}

var tracer = otel.Tracer("github.com/ticatwolves/notifex/internal/dispatch/worker")

var errLostClaim = errors.New("worker: another worker owns this notification now")

// Handle processes one delivery. Infrastructure errors leave the message
// unacked, so the queue redelivers it after the visibility timeout.
func (w *Worker) Handle(ctx context.Context, d dispatch.Delivery) {
	env := d.Envelope()
	ctx = dispatch.ContextWithTraceParent(ctx, env.TraceParent)
	ctx, span := tracer.Start(ctx, "dispatch.handle")
	defer span.End()
	span.SetAttributes(attribute.String("notifex.tenant_id", env.TenantID.String()),
		attribute.String("notifex.notification_id", env.NotificationID.String()), attribute.String("notifex.channel", env.Channel))
	ctx = observability.WithLogAttrs(ctx, slog.String(observability.KeyTenantID, env.TenantID.String()),
		slog.String("run_id", env.RunID.String()), slog.String("notification_id", env.NotificationID.String()),
		slog.String("channel", env.Channel))
	if err := w.handle(ctx, d, env); err != nil {
		w.mOutcomes.WithLabelValues(env.Channel, "error").Inc()
		w.log.WarnContext(ctx, "dispatch failed; message will be redelivered", slog.Any("error", err))
	}
}

// outcome is everything one transition writes.
type outcome struct {
	label      string
	change     store.NotificationChange
	event      store.NotificationEvent
	usage      *store.UsageRecord
	outbox     *store.OutboxMessage
	scheduled  *store.ScheduledMessage
	releaseSMS bool
}

func (w *Worker) handle(ctx context.Context, d dispatch.Delivery, env dispatch.Envelope) error {
	tid := store.TenantID(env.TenantID)
	now := w.now().UTC()
	n, ok, err := w.st.Notifications().Claim(ctx, tid, env.NotificationID, now, w.cfg.Lease)
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if !ok {
		return w.notClaimed(ctx, d, tid, env.NotificationID, now)
	}

	// Lifetime: anything still pending after MaxAge expires (ADR-014 §5).
	if now.Sub(n.CreatedAt) > w.cfg.MaxAge {
		return w.finish(ctx, d, n, w.terminal(n, store.NotifExpired, "expired", "pending for more than 48 hours", false, now))
	}
	run, err := w.st.Runs().Get(ctx, tid, n.RunID)
	if err != nil {
		return fmt.Errorf("load run: %w", err)
	}
	snap, err := w.st.Snapshots().Get(ctx, tid, run.SnapshotHash)
	if err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}
	cfg, err := parseSnapshot(snap.Doc, n.ActionID)
	if err != nil {
		return w.finish(ctx, d, n, w.terminal(n, store.NotifFailed, "snapshot_invalid", "the run's configuration snapshot is unreadable", false, now))
	}
	recipient, err := w.decryptJSON(ctx, tid, n.ID, "recipient", n.Recipient)
	if err != nil {
		return fmt.Errorf("decrypt recipient: %w", err)
	}

	// Policy at dispatch: quiet hours, then the tenant's channel rate limit.
	tz, _ := recipient["timezone"].(string)
	if dec := policy.Evaluate(cfg.policy, n.Channel, tz, now); dec.Defer {
		return w.finish(ctx, d, n, w.deferral(n, dec.Until, dec.Reason, now))
	}
	if rate := cfg.policy.RateLimitsPerMinute[n.Channel]; rate > 0 {
		if ok, retry := w.limiter(tid, n.Channel, rate).Allow(tid.String() + "/" + n.Channel); !ok {
			return w.finish(ctx, d, n, w.deferral(n, now.Add(max(retry, time.Second)), "rate_limited", now))
		}
	}

	payload, err := w.decryptJSON(ctx, tid, run.ID, "payload", run.Payload)
	if err != nil {
		return fmt.Errorf("decrypt payload: %w", err)
	}
	msg, err := w.render(ctx, n, cfg, payload, recipient)
	if err != nil {
		return w.finish(ctx, d, n, w.terminal(n, store.NotifFailed, "render_failed", truncate(err.Error(), 300), false, now))
	}
	cred, err := w.credential(ctx, tid, n, cfg)
	if err != nil {
		var se *channel.SendError
		if errors.As(err, &se) {
			return w.finish(ctx, d, n, w.terminal(n, store.NotifFailed, se.Code, se.Message, false, now))
		}
		return fmt.Errorf("credential: %w", err)
	}
	sender, err := w.reg.Lookup(n.Channel, cred.Provider)
	if err != nil {
		se := channel.AsSendError(err)
		return w.finish(ctx, d, n, w.terminal(n, store.NotifFailed, se.Code, se.Message, false, now))
	}

	res, sendErr := w.send(ctx, d, tid, n, sender, msg, cred)
	clear(cred.Secret)
	now = w.now().UTC()
	if sendErr != nil {
		return w.finish(ctx, d, n, w.failure(n, cfg, channel.AsSendError(sendErr), now))
	}
	return w.finish(ctx, d, n, w.success(ctx, tid, n, msg, res, now))
}

// notClaimed decides what to do with a message whose notification can't be
// claimed: in flight under someone else's valid lease → leave it (the queue
// redelivers after the visibility timeout, by which time the holder has
// finished or its lease has expired); anything else (done, canceled) → ack.
func (w *Worker) notClaimed(ctx context.Context, d dispatch.Delivery, tid store.TenantID, id uuid.UUID, now time.Time) error {
	n, err := w.st.Notifications().Get(ctx, tid, id)
	if errors.Is(err, store.ErrNotFound) {
		w.mOutcomes.WithLabelValues(d.Envelope().Channel, "orphan").Inc()
		return ackErr(d.Ack(ctx))
	}
	if err != nil {
		return fmt.Errorf("load notification: %w", err)
	}
	if n.Status == store.NotifSending && n.LeaseUntil != nil && n.LeaseUntil.After(now) {
		w.mOutcomes.WithLabelValues(n.Channel, "in_flight_elsewhere").Inc()
		return nil
	}
	w.mOutcomes.WithLabelValues(n.Channel, "duplicate").Inc()
	return ackErr(d.Ack(ctx))
}

func ackErr(err error) error {
	if err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	return nil
}

// send calls the vendor with a heartbeat that renews the lease and the
// message visibility while the call is in flight.
func (w *Worker) send(ctx context.Context, d dispatch.Delivery, tid store.TenantID, n store.Notification,
	s channel.Sender, m channel.Message, c channel.Credential,
) (channel.Result, error) {
	hbCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		t := time.NewTicker(w.cfg.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if _, err := w.st.Notifications().ExtendLease(hbCtx, tid, n.ID, w.now().UTC(), w.cfg.Lease); err != nil {
					w.log.WarnContext(hbCtx, "lease heartbeat failed", slog.Any("error", err))
				}
				if err := d.Extend(hbCtx, w.cfg.Lease); err != nil {
					w.log.WarnContext(hbCtx, "visibility heartbeat failed", slog.Any("error", err))
				}
			}
		}
	}()
	sctx, cancel := context.WithTimeout(ctx, w.cfg.SendTimeout)
	defer cancel()
	start := time.Now()
	res, err := s.Send(sctx, m, c)
	w.mSend.WithLabelValues(n.Channel).Observe(time.Since(start).Seconds())
	return res, err
}

func event(n store.Notification, typ string, detail map[string]any) store.NotificationEvent {
	return store.NotificationEvent{ID: uuid.Must(uuid.NewV7()), NotificationID: n.ID, Type: typ, Detail: detail}
}

func ptr[T any](v T) *T { return &v }

// terminal fails (or expires) a notification. reachedVendor=false releases
// a platform-SMS reservation: the message never reached SNS (ADR-005).
func (w *Worker) terminal(n store.Notification, status, code, msg string, reachedVendor bool, now time.Time) outcome {
	return outcome{
		label: status,
		change: store.NotificationChange{Status: &status, LastErrorCode: &code, LastError: &msg,
			TerminalAt: &now, ClearLease: true},
		event:      event(n, status, map[string]any{"code": code, "attempt": n.Attempt}),
		releaseSMS: n.SMSReserved && !reachedVendor,
	}
}

// deferral re-queues without spending an attempt (quiet hours, rate limit).
func (w *Worker) deferral(n store.Notification, until time.Time, reason string, now time.Time) outcome {
	status := store.NotifDeferred
	o := outcome{
		label:  "deferred",
		change: store.NotificationChange{Status: &status, NextAttemptAt: &until, Attempt: ptr(max(n.Attempt-1, 0)), ClearLease: true},
		event:  event(n, "deferred", map[string]any{"reason": reason, "until": until.UTC().Format(time.RFC3339)}),
	}
	w.requeue(&o, n, until, n.Attempt, "deferred", now)
	return o
}

// failure retries with full-jitter backoff, or fails permanently.
func (w *Worker) failure(n store.Notification, cfg snapshotConfig, se *channel.SendError, now time.Time) outcome {
	maxAttempts := w.cfg.DefaultMaxAttempts
	if cfg.policy.MaxAttempts != nil {
		maxAttempts = *cfg.policy.MaxAttempts
	}
	if !se.Retryable || n.Attempt >= maxAttempts {
		code := se.Code
		if se.Retryable {
			code = "max_attempts_exceeded"
		}
		return w.terminal(n, store.NotifFailed, code, se.Message, true, now)
	}
	delay := max(w.backoff(n.Attempt), se.RetryAfter)
	until := now.Add(delay)
	status := store.NotifRetrying
	o := outcome{
		label: "retrying",
		change: store.NotificationChange{Status: &status, LastErrorCode: &se.Code, LastError: &se.Message,
			NextAttemptAt: &until, ClearLease: true},
		event: event(n, "retrying", map[string]any{"code": se.Code, "attempt": n.Attempt, "next_attempt_at": until.UTC().Format(time.RFC3339)}),
	}
	w.requeue(&o, n, until, n.Attempt+1, "backoff", now)
	return o
}

// requeue schedules the next attempt through the outbox (≤ 15 min, as an
// SQS delay) or scheduled_dispatch (longer). Using the outbox, not a direct
// publish, means a crash right after commit can't strand the notification.
func (w *Worker) requeue(o *outcome, n store.Notification, until time.Time, attempt int, reason string, now time.Time) {
	delay := until.Sub(now)
	if delay <= dispatch.MaxDelay {
		o.outbox = &store.OutboxMessage{TenantID: n.TenantID, NotificationID: n.ID, RunID: n.RunID, Channel: n.Channel,
			DelaySeconds: int(math.Ceil(max(delay, 0).Seconds())), Attempt: attempt}
		return
	}
	o.scheduled = &store.ScheduledMessage{NotificationID: n.ID, RunID: n.RunID, Channel: n.Channel, DueAt: until,
		Reason: reason, Attempt: attempt}
}

func (w *Worker) backoff(attempt int) time.Duration { return Backoff(w.cfg, attempt, w.jitter()) }

// Backoff is full jitter: jitter (in [0, 1)) times min(cap,
// base·2^(attempt-1)), at least one second.
func Backoff(cfg Config, attempt int, jitter float64) time.Duration {
	ceiling := cfg.BackoffCap
	if exp := float64(cfg.BackoffBase) * math.Pow(2, float64(attempt-1)); exp < float64(ceiling) {
		ceiling = time.Duration(exp)
	}
	return max(time.Duration(jitter*float64(ceiling)), time.Second)
}

// success records sent (or delivered for synchronous channels) and bills
// when the notification reaches its billing basis (ADR-007).
func (w *Worker) success(ctx context.Context, tid store.TenantID, n store.Notification, m channel.Message, r channel.Result, now time.Time) outcome {
	status := store.NotifSent
	if r.Delivered {
		status = store.NotifDelivered
	}
	o := outcome{
		label:  status,
		change: store.NotificationChange{Status: &status, VendorMessageID: &r.VendorMessageID, ClearLease: true, LastErrorCode: ptr(""), LastError: ptr("")},
		event:  event(n, status, map[string]any{"attempt": n.Attempt, "vendor_message_id": truncate(r.VendorMessageID, 128)}),
	}
	if status == store.NotifDelivered {
		o.change.TerminalAt = &now
	}
	if rendered, err := w.encryptRendered(ctx, tid, n.ID, m); err == nil {
		o.change.RenderedContent = rendered
	} else {
		w.log.WarnContext(ctx, "storing rendered content failed", slog.Any("error", err))
	}
	if billable(n.BillingBasis, status) {
		o.usage = &store.UsageRecord{NotificationID: n.ID, Channel: n.Channel, CredentialMode: n.CredentialMode,
			BillingBasis: n.BillingBasis, BillableAt: now}
	}
	return o
}

func billable(basis, status string) bool {
	switch basis {
	case "sent":
		return status == store.NotifSent || status == store.NotifDelivered
	case "delivered":
		return status == store.NotifDelivered
	}
	return false
}

// finish writes an outcome in one transaction, then acks. The status
// change is conditional on still holding the claim (status sending): if
// another worker reclaimed it (our lease expired), nothing is written and
// the message is acked; that worker owns the notification.
func (w *Worker) finish(ctx context.Context, d dispatch.Delivery, n store.Notification, o outcome) error {
	tid := n.TenantID
	now := w.now().UTC()
	err := w.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if o.releaseSMS {
			o.change.SMSReserved = ptr(false)
		}
		ok, err := tx.Notifications().Update(ctx, tid, n.ID, []string{store.NotifSending}, o.change, now)
		if err != nil {
			return err
		}
		if !ok {
			return errLostClaim
		}
		if err := tx.Events().Append(ctx, tid, []store.NotificationEvent{o.event}); err != nil {
			return err
		}
		if o.usage != nil {
			if _, err := tx.Usage().Record(ctx, tid, *o.usage); err != nil {
				return err
			}
		}
		if o.outbox != nil {
			o.outbox.TraceParent = dispatch.TraceParent(ctx)
			if _, err := tx.Outbox().Enqueue(ctx, tid, []store.OutboxMessage{*o.outbox}, now); err != nil {
				return err
			}
		}
		if o.scheduled != nil {
			o.scheduled.TraceParent = dispatch.TraceParent(ctx)
			if err := tx.Scheduled().Add(ctx, tid, []store.ScheduledMessage{*o.scheduled}, now); err != nil {
				return err
			}
		}
		if o.releaseSMS {
			day := n.CreatedAt
			if err := tx.SMSLimits().Release(ctx, tid, day, 1); err != nil {
				return err
			}
			if err := tx.SMSLimits().ReleasePlatform(ctx, day, 1); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errLostClaim) {
		w.mOutcomes.WithLabelValues(n.Channel, "lost_claim").Inc()
		return ackErr(d.Ack(ctx))
	}
	if err != nil {
		return fmt.Errorf("record outcome: %w", err)
	}
	w.mOutcomes.WithLabelValues(n.Channel, o.label).Inc()
	if o.label != store.NotifSent && o.label != store.NotifDelivered {
		w.log.InfoContext(ctx, "notification "+o.label, slog.String("code", deref(o.change.LastErrorCode)), slog.Int("attempt", n.Attempt))
	}
	return ackErr(d.Ack(ctx))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (w *Worker) limiter(tid store.TenantID, ch string, rate int) *ratelimit.Limiter {
	key := fmt.Sprintf("%s/%s/%d", tid, ch, rate)
	w.limMu.Lock()
	defer w.limMu.Unlock()
	l, ok := w.limiters[key]
	if !ok {
		if len(w.limiters) > 10_000 {
			w.limiters = map[string]*ratelimit.Limiter{}
		}
		l = ratelimit.New(rate, max(rate/10, 1), 1)
		w.limiters[key] = l
	}
	return l
}

func (w *Worker) decryptJSON(ctx context.Context, tid store.TenantID, row uuid.UUID, column string, ct []byte) (map[string]any, error) {
	if ct == nil {
		return nil, errors.New("purged")
	}
	pt, err := w.cipher.Decrypt(ctx, crypto.AAD{TenantID: tid.UUID(), RowID: row, Column: column}, ct)
	if err != nil {
		return nil, err
	}
	defer clear(pt)
	dec := json.NewDecoder(bytes.NewReader(pt))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", column, err)
	}
	return m, nil
}

func (w *Worker) encryptRendered(ctx context.Context, tid store.TenantID, id uuid.UUID, m channel.Message) ([]byte, error) {
	raw, err := json.Marshal(map[string]any{"email": m.Email, "sms": m.SMS, "slack": m.Slack, "webhook": m.Webhook})
	if err != nil {
		return nil, fmt.Errorf("encode rendered: %w", err)
	}
	defer clear(raw)
	ct, err := w.cipher.Encrypt(ctx, crypto.AAD{TenantID: tid.UUID(), RowID: id, Column: "rendered_content"}, raw)
	if err != nil {
		return nil, fmt.Errorf("encrypt rendered: %w", err)
	}
	return ct, nil
}

// ---- snapshot

type snapshotConfig struct {
	template map[string]map[string]any // locale → content
	credID   *uuid.UUID
	credMode string
	provider string
	credOK   bool
	brand    map[string]any
	policy   tenant.SendingPolicy
	workflow map[string]any
}

func parseSnapshot(doc map[string]any, actionID uuid.UUID) (snapshotConfig, error) {
	var c snapshotConfig
	actions, _ := doc["actions"].([]any)
	for _, a := range actions {
		am, _ := a.(map[string]any)
		if am["id"] != actionID.String() {
			continue
		}
		c.template = map[string]map[string]any{}
		if ts, ok := am["templates"].(map[string]any); ok {
			for l, t := range ts {
				if tm, ok := t.(map[string]any); ok {
					c.template[l] = tm
				}
			}
		}
		cred, _ := am["credential"].(map[string]any)
		c.credOK, _ = cred["available"].(bool)
		c.credMode, _ = cred["mode"].(string)
		c.provider, _ = cred["provider"].(string)
		if id, ok := cred["id"].(string); ok {
			if u, err := uuid.Parse(id); err == nil {
				c.credID = &u
			}
		}
	}
	if c.template == nil {
		return c, errors.New("action not in snapshot")
	}
	c.brand, _ = doc["brand"].(map[string]any)
	wf, _ := doc["workflow"].(map[string]any)
	c.workflow = map[string]any{"key": wf["key"], "name": wf["name"]}
	if p, ok := doc["policy"]; ok {
		raw, err := json.Marshal(p)
		if err == nil {
			_ = json.Unmarshal(raw, &c.policy)
		}
	}
	return c, nil
}

// ---- render and credential

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (w *Worker) render(ctx context.Context, n store.Notification, cfg snapshotConfig, payload, recipient map[string]any) (channel.Message, error) {
	tpl, ok := cfg.template[n.Locale]
	if !ok {
		return channel.Message{}, errors.New("no template for the notification's locale")
	}
	var c workflow.Content
	raw, err := json.Marshal(tpl)
	if err != nil {
		return channel.Message{}, fmt.Errorf("template: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return channel.Message{}, fmt.Errorf("template: %w", err)
	}
	in := render.Input{Payload: payload, Recipient: recipient, Brand: cfg.brand, Workflow: cfg.workflow}
	m := channel.Message{NotificationID: n.ID, TenantID: n.TenantID.UUID(), Channel: n.Channel}
	s := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	switch n.Channel {
	case "email":
		out, err := render.Email(ctx, render.EmailTemplate{Subject: s(c.Subject), HTML: s(c.HTML), Text: s(c.Text), Layout: s(c.Layout)}, in)
		if err != nil {
			return channel.Message{}, err
		}
		m.Email, m.To = &out, strings.ToLower(str(recipient, "email"))
		if em, ok := cfg.brand["email"].(map[string]any); ok {
			m.FromName, m.ReplyTo = str(em, "from_name"), str(em, "reply_to")
		}
	case "sms":
		out, err := render.SMS(ctx, s(c.Text), in)
		if err != nil {
			return channel.Message{}, err
		}
		m.SMS, m.To = &out, str(recipient, "phone")
	case "slack":
		out, err := render.Slack(ctx, render.SlackTemplate{Text: s(c.Text), Blocks: c.Blocks}, in)
		if err != nil {
			return channel.Message{}, err
		}
		m.Slack, m.To = &out, str(recipient, "slack_user")
	case "webhook":
		out, err := render.Webhook(ctx, c.Body, in)
		if err != nil {
			return channel.Message{}, err
		}
		m.Webhook = out
	default:
		return channel.Message{}, fmt.Errorf("channel %s is not supported yet", n.Channel)
	}
	return m, nil
}

// credential loads the snapshotted credential reference. The secret is
// fetched and decrypted now, so a rotation applies to in-flight sends; a
// deleted credential fails permanently (ADR-005).
func (w *Worker) credential(ctx context.Context, tid store.TenantID, n store.Notification, cfg snapshotConfig) (channel.Credential, error) {
	c := channel.Credential{Mode: n.CredentialMode, Provider: n.Provider}
	if n.CredentialMode == "" || !cfg.credOK {
		return c, channel.Permanent("credential_unavailable", "no credential was resolved for this channel")
	}
	if n.CredentialID == nil {
		return c, nil // implicit platform credential (the dispatcher's IAM role)
	}
	row, err := w.st.Credentials().Get(ctx, tid, *n.CredentialID)
	if errors.Is(err, store.ErrNotFound) {
		return c, channel.Permanent("credential_unavailable", "the credential was deleted after the trigger was accepted")
	}
	if err != nil {
		return c, err
	}
	if row.Status == store.CredentialDisabled {
		return c, channel.Permanent("credential_unavailable", "the credential is disabled")
	}
	c.ID, c.Config = n.CredentialID, row.Config
	if row.SecretCiphertext != nil {
		if c.Secret, err = w.secrets.Secret(ctx, tid, *n.CredentialID); err != nil {
			if errors.Is(err, crypto.ErrDataKeyDestroyed) {
				return c, channel.Permanent("credential_unavailable", "the tenant's data key was destroyed")
			}
			return c, err
		}
	}
	return c, nil
}

// ---- consumer component

// Dispatcher consumes every channel's queue with the worker (a
// lifecycle.Component).
type Dispatcher struct {
	q        dispatch.JobQueue
	w        *Worker
	channels []string
}

// NewDispatcher builds the consumer for channels.
func NewDispatcher(q dispatch.JobQueue, w *Worker, channels []string) *Dispatcher {
	return &Dispatcher{q: q, w: w, channels: channels}
}

// Name implements lifecycle.Component.
func (d *Dispatcher) Name() string { return "dispatcher" }

// Start implements lifecycle.Component: one consumer per channel.
func (d *Dispatcher) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(d.channels))
	for _, ch := range d.channels {
		wg.Go(func() {
			if err := d.q.Consume(ctx, ch, d.w.Handle); err != nil {
				errs <- fmt.Errorf("consume %s: %w", ch, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	return errors.Join(func() []error {
		var out []error
		for e := range errs {
			out = append(out, e)
		}
		return out
	}()...)
}

// Stop implements lifecycle.Component (consumers stop with their context).
func (d *Dispatcher) Stop(context.Context) error { return nil }
