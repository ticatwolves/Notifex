// Package trigger is the accept path (docs/02, ADR-004, ADR-014 §3): one
// trigger becomes a run and its notifications, decided per notification and
// written in one transaction with the outbox, then handed to the queue.
// Transports (REST now; gRPC, MCP and others later) stay thin over Service.
package trigger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/credential"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/dispatch"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
	"github.com/ticatwolves/notifex/internal/workflow"
)

// Limits (contract and ADR-006/014).
const (
	MaxRecipients   = 1000
	ScheduleHorizon = 7 * 24 * time.Hour
	MaxSQSDelay     = 15 * time.Minute
	IdempotencyTTL  = 24 * time.Hour
)

// Errors mapped by transports.
var (
	ErrWorkflowNotFound    = errors.New("trigger: workflow not found")
	ErrWorkflowNotActive   = errors.New("trigger: workflow is not active")
	ErrIdempotencyConflict = errors.New("trigger: idempotency key reused with a different request")
	ErrScheduleHorizon     = errors.New("trigger: schedule_at is more than 7 days ahead")
)

// Per-notification error codes (ADR-014 §3).
const (
	CodeChannelNotAllowed     = "channel_not_allowed"
	CodeRecipientMissing      = "recipient_missing"
	CodeTemplateMissing       = "template_missing"
	CodeCredentialUnavailable = "credential_unavailable" //nolint:gosec // G101 false positive: an error code, not a credential
	CodePlatformSMSLimit      = "platform_sms_daily_limit"
)

func fieldErr(field, format string, args ...any) error {
	return &tenant.FieldError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// Settings gives the effective brand and policy (tenant.Service).
type Settings interface {
	EffectiveBrand(ctx context.Context, tid store.TenantID, appID uuid.UUID) (tenant.BrandView, error)
	EffectivePolicy(ctx context.Context, tid store.TenantID, appID uuid.UUID) (tenant.SendingPolicyView, error)
}

// Resolver resolves a channel's credential (credential.Service).
type Resolver interface {
	Resolve(ctx context.Context, tid store.TenantID, appID, envID uuid.UUID, channel string) (credential.Ref, error)
}

// Publisher is the after-commit fast path to the queue (slice 4). Failures
// are fine: the outbox relay publishes whatever is left.
type Publisher interface {
	Publish(ctx context.Context, msgs []store.OutboxMessage) error
}

// Request is a decoded trigger.
type Request struct {
	Workflow       string         `json:"workflow"`
	Recipients     []Recipient    `json:"recipients"`
	Payload        map[string]any `json:"payload,omitempty"`
	Context        map[string]any `json:"context,omitempty"`
	Channels       []string       `json:"channels,omitempty"`
	ScheduleAt     *time.Time     `json:"schedule_at,omitempty"`
	IdempotencyKey string         `json:"-"`
	Source         string         `json:"-"`
}

// Outcome is one notification's state in the response.
type Outcome struct {
	ID           uuid.UUID
	Channel      string
	RecipientRef string
	Status       string
	ErrorCode    string
	ErrorMessage string
	RetryAfter   *time.Time
}

// Result is an accepted (or replayed) trigger.
type Result struct {
	Run           store.Run
	Notifications []Outcome
	Replayed      bool
}

// Config is the service's configuration.
type Config struct {
	RecipientHashKey    []byte
	PlatformSMSDailyCap int
}

// Service implements trigger intake and run reads.
type Service struct {
	st       store.Store
	settings Settings
	creds    Resolver
	cipher   crypto.FieldCipher
	pub      Publisher // optional
	cfg      Config
	log      *slog.Logger
	now      func() time.Time

	schemaMu sync.Mutex
	schemas  map[string]*workflow.PayloadSchema

	mRequests  *prometheus.CounterVec
	mNotifs    *prometheus.CounterVec
	mDuration  prometheus.Histogram
	mPublished *prometheus.CounterVec
}

// NewService builds the service. pub may be nil (the relay publishes).
func NewService(st store.Store, settings Settings, creds Resolver, cipher crypto.FieldCipher, pub Publisher,
	cfg Config, log *slog.Logger, reg prometheus.Registerer,
) *Service {
	s := &Service{
		st: st, settings: settings, creds: creds, cipher: cipher, pub: pub, cfg: cfg, log: log, now: time.Now,
		schemas: map[string]*workflow.PayloadSchema{},
		mRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifex_trigger_requests_total", Help: "Trigger requests by outcome.",
		}, []string{"outcome"}),
		mNotifs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifex_trigger_notifications_total", Help: "Notifications created at accept, by channel and initial status.",
		}, []string{"channel", "status"}),
		mDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "notifex_trigger_accept_seconds", Help: "Accept-path latency (validation through commit).",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}),
		mPublished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifex_trigger_fast_publish_total", Help: "After-commit fast-path publishes by result.",
		}, []string{"result"}),
	}
	if reg != nil {
		reg.MustRegister(s.mRequests, s.mNotifs, s.mDuration, s.mPublished)
	}
	return s
}

// SetClock overrides the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

var tracer = otel.Tracer("github.com/ticatwolves/notifex/internal/trigger")

// requestHash is SHA-256 of the canonical request (maps marshal with
// sorted keys), excluding transport fields.
func requestHash(r Request) []byte {
	raw, _ := json.Marshal(r)
	sum := sha256.Sum256(raw)
	return sum[:]
}

func nextUTCMidnight(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

// Accept validates and accepts a trigger (ADR-014 §3).
func (s *Service) Accept(ctx context.Context, p auth.Principal, req Request) (res Result, err error) {
	ctx, span := tracer.Start(ctx, "trigger.accept")
	defer span.End()
	span.SetAttributes(attribute.String("notifex.tenant_id", p.TenantID.String()), attribute.String("notifex.workflow", req.Workflow))
	start := s.now()
	defer func() {
		outcome := "accepted"
		switch {
		case err != nil:
			outcome = "rejected"
			span.SetStatus(codes.Error, "rejected")
		case res.Replayed:
			outcome = "replayed"
		case res.Run.Status == store.RunFailed:
			outcome = "nothing_accepted"
		}
		s.mRequests.WithLabelValues(outcome).Inc()
		s.mDuration.Observe(time.Since(start).Seconds())
	}()

	now := s.now().UTC()
	if err := validate(&req, now); err != nil {
		return Result{}, err
	}
	hash := requestHash(req)
	if req.IdempotencyKey != "" {
		if r, done, err := s.replay(ctx, p, req.IdempotencyKey, hash, now); done || err != nil {
			return r, err
		}
	}

	// Configuration, read once and snapshotted.
	wf, err := s.st.Workflows().GetByKey(ctx, p.TenantID, p.ApplicationID, req.Workflow)
	if errors.Is(err, store.ErrNotFound) {
		return Result{}, ErrWorkflowNotFound
	}
	if err != nil {
		return Result{}, fmt.Errorf("trigger: %w", err)
	}
	if wf.Status != store.WorkflowActive {
		return Result{}, ErrWorkflowNotActive
	}
	acts, err := s.st.Actions().ListForWorkflows(ctx, p.TenantID, []uuid.UUID{wf.ID})
	if err != nil {
		return Result{}, fmt.Errorf("trigger: %w", err)
	}
	actions, err := narrow(acts[wf.ID], req.Channels)
	if err != nil {
		return Result{}, err
	}
	if wf.PayloadSchema != nil {
		sch, err := s.schema(wf)
		if err != nil {
			return Result{}, fmt.Errorf("trigger: stored payload schema: %w", err)
		}
		payload := req.Payload
		if payload == nil {
			payload = map[string]any{}
		}
		if err := sch.Validate(payload); err != nil {
			return Result{}, fieldErr("payload", "doesn't match the workflow's payload schema: %s", truncate(err.Error(), 300))
		}
	}
	brand, err := s.settings.EffectiveBrand(ctx, p.TenantID, p.ApplicationID)
	if err != nil {
		return Result{}, fmt.Errorf("trigger: %w", err)
	}
	policy, err := s.settings.EffectivePolicy(ctx, p.TenantID, p.ApplicationID)
	if err != nil {
		return Result{}, fmt.Errorf("trigger: %w", err)
	}
	ten, err := s.st.Tenants().Get(ctx, p.TenantID)
	if err != nil {
		return Result{}, fmt.Errorf("trigger: %w", err)
	}
	plan, err := s.plan(ctx, p, wf, actions, brand, policy)
	if err != nil {
		return Result{}, err
	}

	// Build the run and its notifications.
	runID := uuid.Must(uuid.NewV7())
	span.SetAttributes(attribute.String("notifex.run_id", runID.String()))
	payloadJSON, err := json.Marshal(orEmpty(req.Payload))
	if err != nil {
		return Result{}, fieldErr("payload", "not valid JSON")
	}
	payloadCT, err := s.cipher.Encrypt(ctx, crypto.AAD{TenantID: p.TenantID.UUID(), RowID: runID, Column: "payload"}, payloadJSON)
	clear(payloadJSON)
	if err != nil {
		return Result{}, fmt.Errorf("trigger: encrypt payload: %w", err)
	}
	due := now
	if req.ScheduleAt != nil {
		due = req.ScheduleAt.UTC()
	}
	notifs, outcomes, err := s.buildNotifications(ctx, p, runID, req, plan, policy, brand, due, now)
	if err != nil {
		return Result{}, err
	}
	run := store.Run{
		ID: runID, ApplicationID: p.ApplicationID, EnvironmentID: p.EnvironmentID, WorkflowID: wf.ID, WorkflowKey: wf.Key,
		SnapshotHash: plan.hash, Payload: payloadCT, Context: req.Context, Source: req.Source,
		ActorType: string(store.ActorAPIKey), ActorID: p.APIKeyID.String(), ScheduleAt: req.ScheduleAt,
	}
	if req.IdempotencyKey != "" {
		run.IdempotencyKey = &req.IdempotencyKey
	}

	var created store.Run
	var queued []store.OutboxMessage
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if req.IdempotencyKey != "" {
			if err := tx.Idempotency().Insert(ctx, p.TenantID, store.IdempotencyRecord{
				Key: req.IdempotencyKey, RequestHash: hash, RunID: runID, ExpiresAt: now.Add(IdempotencyTTL),
			}, now); err != nil {
				return err
			}
		}
		if err := s.reserveSMS(ctx, tx, p.TenantID, ten.PlatformSMSDailyLimit, notifs, outcomes, now); err != nil {
			return err
		}
		run.Status = store.RunFailed
		for _, n := range notifs {
			if n.Status == store.NotifQueued || n.Status == store.NotifDeferred {
				run.Status = store.RunAccepted
				break
			}
		}
		if run.Status == store.RunFailed {
			run.TerminalAt = &now
		}
		if err := tx.Snapshots().Put(ctx, p.TenantID, plan.hash, plan.doc, now); err != nil {
			return err
		}
		var err error
		if created, err = tx.Runs().Create(ctx, p.TenantID, run); err != nil {
			return err
		}
		if err := tx.Notifications().CreateBatch(ctx, p.TenantID, notifs); err != nil {
			return err
		}
		if err := tx.Events().Append(ctx, p.TenantID, acceptEvents(notifs)); err != nil {
			return err
		}
		queued, err = s.enqueue(ctx, tx, p.TenantID, notifs, due, now)
		return err
	})
	if errors.Is(err, store.ErrConflict) && req.IdempotencyKey != "" {
		// A concurrent request with the same key committed first.
		if r, done, rerr := s.replay(ctx, p, req.IdempotencyKey, hash, now); done || rerr != nil {
			return r, rerr
		}
	}
	if err != nil {
		return Result{}, fmt.Errorf("trigger: accept: %w", err)
	}
	for _, n := range notifs {
		s.mNotifs.WithLabelValues(n.Channel, n.Status).Inc()
	}
	s.fastPublish(ctx, queued)
	s.log.InfoContext(ctx, "trigger accepted", slog.String("run_id", runID.String()),
		slog.String("workflow", wf.Key), slog.Int("notifications", len(notifs)), slog.String("status", string(created.Status)))
	return Result{Run: created, Notifications: outcomes}, nil
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func validate(req *Request, now time.Time) error {
	if req.Source == "" {
		req.Source = "rest"
	}
	switch {
	case req.Workflow == "" || len(req.Workflow) > 63:
		return fieldErr("workflow", "1-63 characters")
	case len(req.Recipients) == 0:
		return fieldErr("recipients", "at least one recipient is required")
	case len(req.Recipients) > MaxRecipients:
		return fieldErr("recipients", "at most %d recipients", MaxRecipients)
	}
	seen := map[string]bool{}
	for _, c := range req.Channels {
		if !tenant.Channels[c] {
			return fieldErr("channels", "unknown channel %q", truncate(c, 20))
		}
		if seen[c] {
			return fieldErr("channels", "duplicate channel %q", c)
		}
		seen[c] = true
	}
	if req.ScheduleAt != nil {
		switch {
		case req.ScheduleAt.After(now.Add(ScheduleHorizon)):
			return ErrScheduleHorizon
		case !req.ScheduleAt.After(now):
			req.ScheduleAt = nil // a past schedule_at means now (ADR-014 §3)
		}
	}
	return nil
}

// narrow returns the enabled actions, narrowed to channels if given. A
// requested channel the workflow doesn't have enabled is a 400.
func narrow(acts []store.Action, channels []string) ([]store.Action, error) {
	enabled := map[string]store.Action{}
	var out []store.Action
	for _, a := range acts {
		if a.Enabled {
			enabled[a.Channel] = a
			out = append(out, a)
		}
	}
	if len(channels) == 0 {
		return out, nil
	}
	out = out[:0]
	for _, c := range channels {
		a, ok := enabled[c]
		if !ok {
			return nil, fieldErr("channels", "the workflow has no enabled %s action", c)
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Service) schema(wf store.Workflow) (*workflow.PayloadSchema, error) {
	key := wf.ID.String() + "@" + wf.UpdatedAt.UTC().Format(time.RFC3339Nano)
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if sch, ok := s.schemas[key]; ok {
		return sch, nil
	}
	sch, err := workflow.CompileSchema(wf.PayloadSchema)
	if err != nil {
		return nil, err
	}
	if len(s.schemas) >= 1024 { // bounded; workflows change rarely
		s.schemas = map[string]*workflow.PayloadSchema{}
	}
	s.schemas[key] = sch
	return sch, nil
}

// actionPlan is one action's snapshotted configuration.
type actionPlan struct {
	action    store.Action
	templates map[string]map[string]any // locale → content
	cred      *credential.Ref           // nil: unavailable
}

type runPlan struct {
	actions []actionPlan
	doc     map[string]any
	hash    []byte
}

// plan resolves templates and credentials per action and builds the
// snapshot document (ADR-014 §2).
func (s *Service) plan(ctx context.Context, p auth.Principal, wf store.Workflow, actions []store.Action,
	brand tenant.BrandView, policy tenant.SendingPolicyView,
) (runPlan, error) {
	var rp runPlan
	docActions := make([]any, 0, len(actions))
	for _, a := range actions {
		ts, err := s.st.Templates().List(ctx, p.TenantID, a.ID)
		if err != nil {
			return runPlan{}, fmt.Errorf("trigger: %w", err)
		}
		ap := actionPlan{action: a, templates: map[string]map[string]any{}}
		for _, t := range ts {
			ap.templates[t.Locale] = t.Content
		}
		ref, err := s.creds.Resolve(ctx, p.TenantID, p.ApplicationID, p.EnvironmentID, a.Channel)
		switch {
		case errors.Is(err, credential.ErrUnavailable):
		case err != nil:
			return runPlan{}, fmt.Errorf("trigger: %w", err)
		default:
			ap.cred = &ref
		}
		credDoc := map[string]any{"available": ap.cred != nil}
		if ap.cred != nil {
			credDoc["mode"], credDoc["provider"], credDoc["scope"] = ap.cred.Mode, ap.cred.Provider, ap.cred.Scope
			credDoc["billing_basis"] = billingBasis(a.Channel, ap.cred.Mode)
			if ap.cred.CredentialID != nil {
				credDoc["id"] = ap.cred.CredentialID.String()
			}
		}
		docActions = append(docActions, map[string]any{
			"id": a.ID.String(), "channel": a.Channel, "templates": ap.templates, "credential": credDoc,
		})
		rp.actions = append(rp.actions, ap)
	}
	rp.doc = map[string]any{
		"v":        1,
		"workflow": map[string]any{"id": wf.ID.String(), "key": wf.Key, "name": wf.Name, "payload_schema": wf.PayloadSchema},
		"actions":  docActions,
		"brand":    brand.Brand,
		"policy":   policy.SendingPolicy,
	}
	raw, err := json.Marshal(rp.doc)
	if err != nil {
		return runPlan{}, fmt.Errorf("trigger: snapshot: %w", err)
	}
	sum := sha256.Sum256(raw)
	rp.hash = sum[:]
	// Store the canonical form (what was hashed), not Go structs.
	var canonical map[string]any
	if err := json.Unmarshal(raw, &canonical); err != nil {
		return runPlan{}, fmt.Errorf("trigger: snapshot: %w", err)
	}
	rp.doc = canonical
	return rp, nil
}

// billingBasis is fixed at accept (ADR-007): platform SMS bills on sent.
func billingBasis(channel, mode string) string {
	if channel == "sms" && mode == "platform" {
		return "sent"
	}
	return "delivered"
}

// buildNotifications decides each (action × recipient) independently
// (ADR-014 §3) and encrypts recipients. SMS reservations happen later, in
// the transaction.
func (s *Service) buildNotifications(ctx context.Context, p auth.Principal, runID uuid.UUID, req Request, plan runPlan,
	policy tenant.SendingPolicyView, brand tenant.BrandView, due, now time.Time,
) ([]store.Notification, []Outcome, error) {
	allowed := map[string]bool{}
	for _, c := range policy.AllowedChannels {
		allowed[c] = true
	}
	defLocale := ""
	if brand.DefaultLocale != nil {
		defLocale = *brand.DefaultLocale
	}
	initial := store.NotifQueued
	if due.Sub(now) > MaxSQSDelay {
		initial = store.NotifDeferred
	}
	var notifs []store.Notification
	var outcomes []Outcome
	for _, ap := range plan.actions {
		ch := ap.action.Channel
		for _, r := range req.Recipients {
			id := uuid.Must(uuid.NewV7())
			addr, required := r.address(ch)
			n := store.Notification{
				ID: id, RunID: runID, ApplicationID: p.ApplicationID, EnvironmentID: p.EnvironmentID, ActionID: ap.action.ID,
				Channel: ch, Status: initial, RecipientMasked: mask(ch, addr), RecipientRef: r.ID,
				RecipientHash: hashAddress(s.cfg.RecipientHashKey, ch, addr),
			}
			if ap.cred != nil {
				n.CredentialID, n.CredentialMode, n.Provider = ap.cred.CredentialID, ap.cred.Mode, ap.cred.Provider
				n.BillingBasis = billingBasis(ch, ap.cred.Mode)
			}
			if due.After(now) {
				d := due
				n.NextAttemptAt = &d
			}
			for _, l := range localeChain(r.Locale, defLocale) {
				if _, ok := ap.templates[l]; ok {
					n.Locale = l
					break
				}
			}
			code := ""
			switch {
			case len(allowed) > 0 && !allowed[ch]:
				code = CodeChannelNotAllowed
			case required && addr == "":
				code = CodeRecipientMissing
			case n.Locale == "":
				code = CodeTemplateMissing
			case ap.cred == nil:
				code = CodeCredentialUnavailable
			}
			if code != "" {
				n.Status, n.LastErrorCode, n.LastError = store.NotifFailed, code, message(code, ch)
				n.TerminalAt = &now
			}
			rec, err := json.Marshal(r)
			if err != nil {
				return nil, nil, fmt.Errorf("trigger: recipient: %w", err)
			}
			if n.Recipient, err = s.cipher.Encrypt(ctx, crypto.AAD{TenantID: p.TenantID.UUID(), RowID: id, Column: "recipient"}, rec); err != nil {
				return nil, nil, fmt.Errorf("trigger: encrypt recipient: %w", err)
			}
			clear(rec)
			notifs = append(notifs, n)
			outcomes = append(outcomes, Outcome{ID: id, Channel: ch, RecipientRef: r.ID, Status: n.Status, ErrorCode: n.LastErrorCode, ErrorMessage: n.LastError})
		}
	}
	if len(notifs) == 0 {
		return nil, nil, fieldErr("channels", "the workflow has no enabled actions") // unreachable for active workflows
	}
	return notifs, outcomes, nil
}

func message(code, channel string) string {
	switch code {
	case CodeChannelNotAllowed:
		return "the policy doesn't allow the " + channel + " channel"
	case CodeRecipientMissing:
		return "the recipient has no address for the " + channel + " channel"
	case CodeTemplateMissing:
		return "no " + channel + " template for the recipient's locale or the default locale"
	case CodeCredentialUnavailable:
		return "no " + channel + " credential is available"
	case CodePlatformSMSLimit:
		return "the daily platform SMS limit is reached"
	}
	return code
}

// reserveSMS reserves platform-SMS capacity for each still-queued platform
// SMS notification, one at a time (tenant limit, then the platform cap). A
// failed reservation fails that notification only.
func (s *Service) reserveSMS(ctx context.Context, tx store.Store, tid store.TenantID, limit int, notifs []store.Notification, outcomes []Outcome, now time.Time) error {
	day := now
	retry := nextUTCMidnight(now)
	for i := range notifs {
		n := &notifs[i]
		if n.Channel != "sms" || n.CredentialMode != "platform" || n.Status == store.NotifFailed {
			continue
		}
		ok, err := tx.SMSLimits().Reserve(ctx, tid, day, 1, limit)
		if err != nil {
			return err
		}
		if ok {
			if ok, err = tx.SMSLimits().ReservePlatform(ctx, day, 1, s.cfg.PlatformSMSDailyCap); err != nil {
				return err
			}
			if !ok {
				if err := tx.SMSLimits().Release(ctx, tid, day, 1); err != nil {
					return err
				}
			}
		}
		if ok {
			n.SMSReserved = true
			continue
		}
		n.Status, n.LastErrorCode, n.LastError, n.TerminalAt = store.NotifFailed, CodePlatformSMSLimit, message(CodePlatformSMSLimit, "sms"), &now
		outcomes[i].Status, outcomes[i].ErrorCode, outcomes[i].ErrorMessage, outcomes[i].RetryAfter = n.Status, n.LastErrorCode, n.LastError, &retry
	}
	return nil
}

func acceptEvents(notifs []store.Notification) []store.NotificationEvent {
	evs := make([]store.NotificationEvent, len(notifs))
	for i, n := range notifs {
		detail := map[string]any{"status": n.Status}
		if n.LastErrorCode != "" {
			detail["code"] = n.LastErrorCode
		}
		evs[i] = store.NotificationEvent{ID: uuid.Must(uuid.NewV7()), NotificationID: n.ID, Type: "accepted", Detail: detail}
		if n.Status == store.NotifFailed {
			evs[i].Type = "rejected"
		}
	}
	return evs
}

// enqueue writes outbox rows for notifications due within 15 minutes and
// scheduled_dispatch rows for later ones (ADR-004).
func (s *Service) enqueue(ctx context.Context, tx store.Store, tid store.TenantID, notifs []store.Notification, due, now time.Time) ([]store.OutboxMessage, error) {
	var out []store.OutboxMessage
	var later []store.ScheduledMessage
	tp := dispatch.TraceParent(ctx) // continued by the dispatcher
	delay := int(due.Sub(now).Round(time.Second) / time.Second)
	if delay < 0 {
		delay = 0
	}
	for _, n := range notifs {
		switch n.Status {
		case store.NotifQueued:
			out = append(out, store.OutboxMessage{TenantID: tid, NotificationID: n.ID, RunID: n.RunID, Channel: n.Channel, DelaySeconds: delay, Attempt: 1, TraceParent: tp})
		case store.NotifDeferred:
			later = append(later, store.ScheduledMessage{NotificationID: n.ID, RunID: n.RunID, Channel: n.Channel, DueAt: due, Reason: "scheduled", Attempt: 1, TraceParent: tp})
		}
	}
	if err := tx.Scheduled().Add(ctx, tid, later, now); err != nil {
		return nil, err
	}
	ids, err := tx.Outbox().Enqueue(ctx, tid, out, now)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].ID = ids[i]
		out[i].CreatedAt = now
	}
	return out, nil
}

// fastPublish hands committed outbox rows to the queue and marks them
// published. Any failure leaves them for the relay (ADR-004).
func (s *Service) fastPublish(ctx context.Context, msgs []store.OutboxMessage) {
	if s.pub == nil || len(msgs) == 0 {
		return
	}
	if err := s.pub.Publish(ctx, msgs); err != nil {
		s.mPublished.WithLabelValues("deferred_to_relay").Inc()
		s.log.WarnContext(ctx, "fast-path publish failed; the relay will publish", slog.Any("error", err))
		return
	}
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	if err := s.st.Outbox().MarkPublished(ctx, ids, s.now().UTC()); err != nil {
		// Published but not marked: the relay republishes; duplicates are harmless.
		s.log.WarnContext(ctx, "marking fast-path publishes failed", slog.Any("error", err))
	}
	s.mPublished.WithLabelValues("published").Inc()
}

// replay returns the original run for a recorded key (done=true), or
// ErrIdempotencyConflict for a different body. done=false means no record.
func (s *Service) replay(ctx context.Context, p auth.Principal, key string, hash []byte, now time.Time) (Result, bool, error) {
	rec, err := s.st.Idempotency().Get(ctx, p.TenantID, key, now)
	if errors.Is(err, store.ErrNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, true, fmt.Errorf("trigger: idempotency: %w", err)
	}
	if string(rec.RequestHash) != string(hash) {
		return Result{}, true, ErrIdempotencyConflict
	}
	run, err := s.st.Runs().GetForEnv(ctx, p.TenantID, p.ApplicationID, p.EnvironmentID, rec.RunID)
	if err != nil {
		// The key belongs to another environment of this tenant: treat as a conflict.
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, true, ErrIdempotencyConflict
		}
		return Result{}, true, fmt.Errorf("trigger: idempotency: %w", err)
	}
	outs, err := s.outcomes(ctx, p.TenantID, run)
	if err != nil {
		return Result{}, true, err
	}
	return Result{Run: run, Notifications: outs, Replayed: true}, true, nil
}

func (s *Service) outcomes(ctx context.Context, tid store.TenantID, run store.Run) ([]Outcome, error) {
	ns, err := s.st.Notifications().ListForRun(ctx, tid, run)
	if err != nil {
		return nil, fmt.Errorf("trigger: %w", err)
	}
	out := make([]Outcome, len(ns))
	for i, n := range ns {
		out[i] = Outcome{ID: n.ID, Channel: n.Channel, RecipientRef: n.RecipientRef, Status: n.Status, ErrorCode: n.LastErrorCode, ErrorMessage: n.LastError}
		if n.LastErrorCode == CodePlatformSMSLimit {
			r := nextUTCMidnight(run.CreatedAt)
			out[i].RetryAfter = &r
		}
	}
	return out, nil
}
