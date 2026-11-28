package entstore

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/configsnapshot"
	"github.com/ticatwolves/notifex/ent/dispatchoutbox"
	"github.com/ticatwolves/notifex/ent/idempotencyrecord"
	"github.com/ticatwolves/notifex/ent/notification"
	"github.com/ticatwolves/notifex/ent/notificationevent"
	"github.com/ticatwolves/notifex/ent/run"
	"github.com/ticatwolves/notifex/ent/scheduleddispatch"
	"github.com/ticatwolves/notifex/internal/store"
)

// uuidTime is a UUIDv7's embedded millisecond timestamp: the partition key
// of notifications and events (ADR-014 §1).
func uuidTime(id uuid.UUID) (time.Time, error) {
	if id.Version() != 7 {
		return time.Time{}, invalid("id %s is not a UUIDv7", id)
	}
	ms := int64(binary.BigEndian.Uint64(id[:8]) >> 16)
	return time.UnixMilli(ms).UTC(), nil
}

// queryOne scans the single row of a raw query into dst (sql.ErrNoRows if none).
func queryOne(ctx context.Context, c *ent.Client, dst any, query string, args ...any) error {
	rows, err := c.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("query: %w", err)
		}
		return sql.ErrNoRows
	}
	if err := rows.Scan(dst); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query: %w", err)
	}
	return nil
}

func day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ---- snapshots

type snapshots struct{ c *ent.Client }

func (s snapshots) Put(ctx context.Context, tid store.TenantID, hash []byte, doc map[string]any, now time.Time) error {
	if len(hash) == 0 {
		return invalid("snapshot hash is required")
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return invalid("snapshot doc: %v", err)
	}
	// Raw SQL for ON CONFLICT DO NOTHING; tenant_id is explicit.
	_, err = s.c.ExecContext(ctx,
		`INSERT INTO config_snapshots (id, tenant_id, hash, doc, created_at) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (tenant_id, hash) DO NOTHING`,
		uuid.Must(uuid.NewV7()), tid.UUID(), hash, raw, now)
	if err != nil {
		return fmt.Errorf("snapshots.put: %w", mapErr(err, opCreate))
	}
	return nil
}

func (s snapshots) Get(ctx context.Context, tid store.TenantID, hash []byte) (store.Snapshot, error) {
	row, err := s.c.ConfigSnapshot.Query().
		Where(configsnapshot.TenantID(tid.UUID()), configsnapshot.Hash(hash)).Only(scoped(ctx, tid))
	if err != nil {
		return store.Snapshot{}, fmt.Errorf("snapshots.get: %w", mapErr(err, opRead))
	}
	return store.Snapshot{Hash: row.Hash, Doc: row.Doc, CreatedAt: row.CreatedAt}, nil
}

// ---- runs

type runs struct{ c *ent.Client }

func toRun(r *ent.Run) store.Run {
	var payload []byte
	if r.Payload != nil {
		payload = *r.Payload
	}
	return store.Run{
		ID: r.ID, TenantID: store.TenantID(r.TenantID), ApplicationID: r.ApplicationID, EnvironmentID: r.EnvironmentID,
		WorkflowID: r.WorkflowID, WorkflowKey: r.WorkflowKey, SnapshotHash: r.SnapshotHash, Payload: payload,
		Context: r.Context, IdempotencyKey: r.IdempotencyKey, Source: r.Source, ActorType: r.ActorType, ActorID: r.ActorID,
		Status: store.RunStatus(r.Status), ScheduleAt: r.ScheduleAt, TerminalAt: r.TerminalAt, PurgedAt: r.PurgedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (s runs) Create(ctx context.Context, tid store.TenantID, r store.Run) (store.Run, error) {
	created, err := uuidTime(r.ID)
	if err != nil {
		return store.Run{}, err
	}
	st := r.Status
	if st == "" {
		st = store.RunAccepted
	}
	c := s.c.Run.Create().SetID(r.ID).SetTenantID(tid.UUID()).SetApplicationID(r.ApplicationID).
		SetEnvironmentID(r.EnvironmentID).SetWorkflowID(r.WorkflowID).SetWorkflowKey(r.WorkflowKey).
		SetSnapshotHash(r.SnapshotHash).SetNillableIdempotencyKey(r.IdempotencyKey).SetSource(r.Source).
		SetActorType(r.ActorType).SetActorID(r.ActorID).SetStatus(run.Status(st)).
		SetNillableScheduleAt(r.ScheduleAt).SetNillableTerminalAt(r.TerminalAt).SetCreatedAt(created).SetUpdatedAt(created)
	if r.Payload != nil {
		c.SetPayload(r.Payload)
	}
	if r.Context != nil {
		c.SetContext(r.Context)
	}
	row, err := c.Save(scoped(ctx, tid))
	if err != nil {
		return store.Run{}, fmt.Errorf("runs.create: %w", mapErr(err, opCreate))
	}
	return toRun(row), nil
}

func (s runs) Get(ctx context.Context, tid store.TenantID, id uuid.UUID) (store.Run, error) {
	row, err := s.c.Run.Query().Where(run.TenantID(tid.UUID()), run.ID(id)).Only(scoped(ctx, tid))
	if err != nil {
		return store.Run{}, fmt.Errorf("runs.get: %w", mapErr(err, opRead))
	}
	return toRun(row), nil
}

func (s runs) GetForEnv(ctx context.Context, tid store.TenantID, appID, envID, id uuid.UUID) (store.Run, error) {
	row, err := s.c.Run.Query().
		Where(run.TenantID(tid.UUID()), run.ApplicationID(appID), run.EnvironmentID(envID), run.ID(id)).
		Only(scoped(ctx, tid))
	if err != nil {
		return store.Run{}, fmt.Errorf("runs.get: %w", mapErr(err, opRead))
	}
	return toRun(row), nil
}

func (s runs) SetStatus(ctx context.Context, tid store.TenantID, id uuid.UUID, st store.RunStatus, terminalAt *time.Time) error {
	n, err := s.c.Run.Update().Where(run.TenantID(tid.UUID()), run.ID(id)).
		SetStatus(run.Status(st)).SetNillableTerminalAt(terminalAt).Save(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("runs.set_status: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		return fmt.Errorf("runs.set_status: %w", store.ErrNotFound)
	}
	return nil
}

// ---- notifications

type notifications struct{ c *ent.Client }

func toNotification(r *ent.Notification) store.Notification {
	deref := func(b *[]byte) []byte {
		if b == nil {
			return nil
		}
		return *b
	}
	return store.Notification{
		ID: r.ID, TenantID: store.TenantID(r.TenantID), RunID: r.RunID, ApplicationID: r.ApplicationID,
		EnvironmentID: r.EnvironmentID, ActionID: r.ActionID, Channel: string(r.Channel), Locale: r.Locale,
		Status: string(r.Status), Attempt: r.Attempt, LeaseUntil: r.LeaseUntil, NextAttemptAt: r.NextAttemptAt,
		Recipient: deref(r.Recipient), RecipientMasked: r.RecipientMasked, RecipientHash: deref(r.RecipientHash),
		RecipientRef: r.RecipientRef, CredentialID: r.CredentialID, CredentialMode: r.CredentialMode,
		Provider: r.Provider, BillingBasis: r.BillingBasis, SMSReserved: r.SmsReserved,
		LastErrorCode: r.LastErrorCode, LastError: r.LastError, VendorMessageID: r.VendorMessageID,
		RenderedContent: deref(r.RenderedContent), VendorResponse: deref(r.VendorResponse),
		TerminalAt: r.TerminalAt, PurgedAt: r.PurgedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (s notifications) CreateBatch(ctx context.Context, tid store.TenantID, ns []store.Notification) error {
	if len(ns) == 0 {
		return nil
	}
	bulk := make([]*ent.NotificationCreate, len(ns))
	for i, n := range ns {
		created, err := uuidTime(n.ID)
		if err != nil {
			return err
		}
		c := s.c.Notification.Create().SetID(n.ID).SetTenantID(tid.UUID()).SetCreatedAt(created).SetUpdatedAt(created).
			SetRunID(n.RunID).SetApplicationID(n.ApplicationID).SetEnvironmentID(n.EnvironmentID).SetActionID(n.ActionID).
			SetChannel(notification.Channel(n.Channel)).SetLocale(n.Locale).SetStatus(notification.Status(n.Status)).
			SetRecipientMasked(n.RecipientMasked).SetRecipientRef(n.RecipientRef).SetNillableCredentialID(n.CredentialID).
			SetCredentialMode(n.CredentialMode).SetProvider(n.Provider).SetBillingBasis(n.BillingBasis).
			SetSmsReserved(n.SMSReserved).SetLastErrorCode(n.LastErrorCode).SetLastError(n.LastError).
			SetNillableNextAttemptAt(n.NextAttemptAt).SetNillableTerminalAt(n.TerminalAt)
		if n.Recipient != nil {
			c.SetRecipient(n.Recipient)
		}
		if n.RecipientHash != nil {
			c.SetRecipientHash(n.RecipientHash)
		}
		bulk[i] = c
	}
	if err := s.c.Notification.CreateBulk(bulk...).Exec(scoped(ctx, tid)); err != nil {
		return fmt.Errorf("notifications.create: %w", mapErr(err, opCreate))
	}
	return nil
}

func (s notifications) Get(ctx context.Context, tid store.TenantID, id uuid.UUID) (store.Notification, error) {
	created, err := uuidTime(id)
	if err != nil {
		return store.Notification{}, fmt.Errorf("notifications.get: %w", store.ErrNotFound)
	}
	row, err := s.c.Notification.Query().
		Where(notification.TenantID(tid.UUID()), notification.ID(id), notification.CreatedAt(created)).
		Only(scoped(ctx, tid))
	if err != nil {
		return store.Notification{}, fmt.Errorf("notifications.get: %w", mapErr(err, opRead))
	}
	return toNotification(row), nil
}

func (s notifications) ListForRun(ctx context.Context, tid store.TenantID, r store.Run) ([]store.Notification, error) {
	from, err := uuidTime(r.ID)
	if err != nil {
		return nil, err
	}
	rows, err := s.c.Notification.Query().
		Where(notification.TenantID(tid.UUID()), notification.RunID(r.ID),
			notification.CreatedAtGTE(from), notification.CreatedAtLT(from.Add(time.Hour))).
		Order(ent.Asc(notification.FieldID)).All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("notifications.list_for_run: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toNotification), nil
}

func (s notifications) Claim(ctx context.Context, tid store.TenantID, id uuid.UUID, now time.Time, lease time.Duration) (store.Notification, bool, error) {
	created, err := uuidTime(id)
	if err != nil {
		return store.Notification{}, false, nil // not ours to claim
	}
	n, err := s.c.Notification.Update().
		Where(notification.TenantID(tid.UUID()), notification.ID(id), notification.CreatedAt(created),
			notification.Or(
				notification.StatusIn(notification.StatusQueued, notification.StatusRetrying, notification.StatusDeferred),
				notification.And(notification.StatusEQ(notification.StatusSending), notification.LeaseUntilLT(now)),
			)).
		SetStatus(notification.StatusSending).AddAttempt(1).SetLeaseUntil(now.Add(lease)).SetUpdatedAt(now).
		Save(scoped(ctx, tid))
	if err != nil {
		return store.Notification{}, false, fmt.Errorf("notifications.claim: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		return store.Notification{}, false, nil
	}
	got, err := s.Get(ctx, tid, id)
	if err != nil {
		return store.Notification{}, false, err
	}
	return got, true, nil
}

func (s notifications) ExtendLease(ctx context.Context, tid store.TenantID, id uuid.UUID, now time.Time, lease time.Duration) (bool, error) {
	created, err := uuidTime(id)
	if err != nil {
		return false, nil
	}
	n, err := s.c.Notification.Update().
		Where(notification.TenantID(tid.UUID()), notification.ID(id), notification.CreatedAt(created),
			notification.StatusEQ(notification.StatusSending)).
		SetLeaseUntil(now.Add(lease)).SetUpdatedAt(now).Save(scoped(ctx, tid))
	if err != nil {
		return false, fmt.Errorf("notifications.extend_lease: %w", mapErr(err, opWrite))
	}
	return n == 1, nil
}

func (s notifications) Update(ctx context.Context, tid store.TenantID, id uuid.UUID, from []string, c store.NotificationChange, now time.Time) (bool, error) {
	created, err := uuidTime(id)
	if err != nil {
		return false, nil
	}
	sts := make([]notification.Status, len(from))
	for i, f := range from {
		sts[i] = notification.Status(f)
	}
	u := s.c.Notification.Update().
		Where(notification.TenantID(tid.UUID()), notification.ID(id), notification.CreatedAt(created), notification.StatusIn(sts...)).
		SetUpdatedAt(now)
	if c.Status != nil {
		u.SetStatus(notification.Status(*c.Status))
	}
	if c.LastErrorCode != nil {
		u.SetLastErrorCode(*c.LastErrorCode)
	}
	if c.LastError != nil {
		u.SetLastError(*c.LastError)
	}
	if c.VendorMessageID != nil {
		u.SetVendorMessageID(*c.VendorMessageID)
	}
	if c.NextAttemptAt != nil {
		u.SetNextAttemptAt(*c.NextAttemptAt)
	}
	if c.TerminalAt != nil {
		u.SetTerminalAt(*c.TerminalAt)
	}
	if c.RenderedContent != nil {
		u.SetRenderedContent(c.RenderedContent)
	}
	if c.VendorResponse != nil {
		u.SetVendorResponse(c.VendorResponse)
	}
	if c.SMSReserved != nil {
		u.SetSmsReserved(*c.SMSReserved)
	}
	if c.Attempt != nil {
		u.SetAttempt(*c.Attempt)
	}
	if c.ClearLease {
		u.ClearLeaseUntil()
	}
	n, err := u.Save(scoped(ctx, tid))
	if err != nil {
		return false, fmt.Errorf("notifications.update: %w", mapErr(err, opWrite))
	}
	return n == 1, nil
}

// ---- events

type events struct{ c *ent.Client }

func (s events) Append(ctx context.Context, tid store.TenantID, evs []store.NotificationEvent) error {
	if len(evs) == 0 {
		return nil
	}
	bulk := make([]*ent.NotificationEventCreate, len(evs))
	for i, e := range evs {
		created, err := uuidTime(e.ID)
		if err != nil {
			return err
		}
		c := s.c.NotificationEvent.Create().SetID(e.ID).SetTenantID(tid.UUID()).SetCreatedAt(created).
			SetNotificationID(e.NotificationID).SetType(e.Type)
		if e.Detail != nil {
			c.SetDetail(e.Detail)
		}
		bulk[i] = c
	}
	if err := s.c.NotificationEvent.CreateBulk(bulk...).Exec(scoped(ctx, tid)); err != nil {
		return fmt.Errorf("events.append: %w", mapErr(err, opCreate))
	}
	return nil
}

func (s events) List(ctx context.Context, tid store.TenantID, notificationID uuid.UUID) ([]store.NotificationEvent, error) {
	from, err := uuidTime(notificationID)
	if err != nil {
		return nil, fmt.Errorf("events.list: %w", store.ErrNotFound)
	}
	rows, err := s.c.NotificationEvent.Query().
		Where(notificationevent.TenantID(tid.UUID()), notificationevent.NotificationID(notificationID),
			notificationevent.CreatedAtGTE(from)).
		Order(ent.Asc(notificationevent.FieldCreatedAt), ent.Asc(notificationevent.FieldID)).All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("events.list: %w", mapErr(err, opRead))
	}
	out := make([]store.NotificationEvent, len(rows))
	for i, r := range rows {
		out[i] = store.NotificationEvent{ID: r.ID, NotificationID: r.NotificationID, Type: r.Type, Detail: r.Detail, CreatedAt: r.CreatedAt}
	}
	return out, nil
}

// ---- outbox (cross-tenant methods use raw SQL with explicit columns)

type outbox struct{ c *ent.Client }

func (s outbox) Enqueue(ctx context.Context, tid store.TenantID, msgs []store.OutboxMessage, now time.Time) ([]int64, error) {
	if len(msgs) == 0 {
		return nil, nil
	}
	bulk := make([]*ent.DispatchOutboxCreate, len(msgs))
	for i, m := range msgs {
		if m.DelaySeconds < 0 || m.DelaySeconds > 900 {
			return nil, invalid("delay %ds is outside 0-900", m.DelaySeconds)
		}
		attempt := m.Attempt
		if attempt == 0 {
			attempt = 1
		}
		bulk[i] = s.c.DispatchOutbox.Create().SetTenantID(tid.UUID()).SetNotificationID(m.NotificationID).
			SetRunID(m.RunID).SetChannel(dispatchoutbox.Channel(m.Channel)).SetDelayS(m.DelaySeconds).
			SetAttempt(attempt).SetTraceparent(m.TraceParent).SetCreatedAt(now)
	}
	rows, err := s.c.DispatchOutbox.CreateBulk(bulk...).Save(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("outbox.enqueue: %w", mapErr(err, opCreate))
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids, nil
}

const outboxCols = `id, tenant_id, notification_id, run_id, channel, delay_s, attempt, traceparent, created_at`

func scanOutbox(rows *sql.Rows) ([]store.OutboxMessage, error) {
	defer func() { _ = rows.Close() }()
	var out []store.OutboxMessage
	for rows.Next() {
		var m store.OutboxMessage
		var tid uuid.UUID
		if err := rows.Scan(&m.ID, &tid, &m.NotificationID, &m.RunID, &m.Channel, &m.DelaySeconds, &m.Attempt, &m.TraceParent, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		m.TenantID = store.TenantID(tid)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

func (s outbox) ClaimUnpublished(ctx context.Context, limit int) ([]store.OutboxMessage, error) {
	rows, err := s.c.QueryContext(ctx,
		`SELECT `+outboxCols+` FROM dispatch_outbox WHERE published_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox.claim: %w", mapErr(err, opRead))
	}
	out, err := scanOutbox(rows)
	if err != nil {
		return nil, fmt.Errorf("outbox.claim: %w", err)
	}
	return out, nil
}

func (s outbox) MarkPublished(ctx context.Context, ids []int64, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.c.ExecContext(ctx, `UPDATE dispatch_outbox SET published_at = $1 WHERE id = ANY($2) AND published_at IS NULL`, now, ids); err != nil {
		return fmt.Errorf("outbox.mark_published: %w", mapErr(err, opWrite))
	}
	return nil
}

func (s outbox) DeletePublishedBefore(ctx context.Context, t time.Time, limit int) (int, error) {
	res, err := s.c.ExecContext(ctx,
		`DELETE FROM dispatch_outbox WHERE id IN (SELECT id FROM dispatch_outbox WHERE published_at < $1 ORDER BY published_at LIMIT $2)`, t, limit)
	if err != nil {
		return 0, fmt.Errorf("outbox.delete_published: %w", mapErr(err, opDelete))
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s outbox) OldestUnpublished(ctx context.Context) (time.Time, error) {
	var t sql.NullTime
	if err := queryOne(ctx, s.c, &t, `SELECT min(created_at) FROM dispatch_outbox WHERE published_at IS NULL`); err != nil {
		return time.Time{}, fmt.Errorf("outbox.oldest: %w", mapErr(err, opRead))
	}
	return t.Time, nil
}

// ---- scheduled dispatch

type scheduled struct{ c *ent.Client }

func (s scheduled) Add(ctx context.Context, tid store.TenantID, msgs []store.ScheduledMessage, now time.Time) error {
	if len(msgs) == 0 {
		return nil
	}
	bulk := make([]*ent.ScheduledDispatchCreate, len(msgs))
	for i, m := range msgs {
		id := m.ID
		if id == uuid.Nil {
			id = uuid.Must(uuid.NewV7())
		}
		attempt := m.Attempt
		if attempt == 0 {
			attempt = 1
		}
		bulk[i] = s.c.ScheduledDispatch.Create().SetID(id).SetTenantID(tid.UUID()).SetNotificationID(m.NotificationID).
			SetRunID(m.RunID).SetChannel(scheduleddispatch.Channel(m.Channel)).SetDueAt(m.DueAt).
			SetReason(scheduleddispatch.Reason(m.Reason)).SetAttempt(attempt).SetTraceparent(m.TraceParent).SetCreatedAt(now)
	}
	if err := s.c.ScheduledDispatch.CreateBulk(bulk...).Exec(scoped(ctx, tid)); err != nil {
		return fmt.Errorf("scheduled.add: %w", mapErr(err, opCreate))
	}
	return nil
}

func (s scheduled) TakeDue(ctx context.Context, now time.Time, limit int) ([]store.ScheduledMessage, error) {
	rows, err := s.c.QueryContext(ctx,
		`DELETE FROM scheduled_dispatch WHERE id IN (
		   SELECT id FROM scheduled_dispatch WHERE due_at <= $1 ORDER BY due_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		 RETURNING id, tenant_id, notification_id, run_id, channel, due_at, reason, attempt, traceparent`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduled.take_due: %w", mapErr(err, opWrite))
	}
	defer func() { _ = rows.Close() }()
	var out []store.ScheduledMessage
	for rows.Next() {
		var m store.ScheduledMessage
		var tid uuid.UUID
		if err := rows.Scan(&m.ID, &tid, &m.NotificationID, &m.RunID, &m.Channel, &m.DueAt, &m.Reason, &m.Attempt, &m.TraceParent); err != nil {
			return nil, fmt.Errorf("scheduled.take_due: %w", err)
		}
		m.TenantID = store.TenantID(tid)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduled.take_due: %w", err)
	}
	return out, nil
}

// ---- idempotency

type idempotency struct{ c *ent.Client }

func (s idempotency) Insert(ctx context.Context, tid store.TenantID, r store.IdempotencyRecord, now time.Time) error {
	_, err := s.c.IdempotencyRecord.Create().SetTenantID(tid.UUID()).SetKey(r.Key).SetRequestHash(r.RequestHash).
		SetRunID(r.RunID).SetCreatedAt(now).SetExpiresAt(r.ExpiresAt).Save(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("idempotency.insert: %w", mapErr(err, opCreate))
	}
	return nil
}

func (s idempotency) Get(ctx context.Context, tid store.TenantID, key string, now time.Time) (store.IdempotencyRecord, error) {
	row, err := s.c.IdempotencyRecord.Query().
		Where(idempotencyrecord.TenantID(tid.UUID()), idempotencyrecord.Key(key), idempotencyrecord.ExpiresAtGT(now)).
		Only(scoped(ctx, tid))
	if err != nil {
		return store.IdempotencyRecord{}, fmt.Errorf("idempotency.get: %w", mapErr(err, opRead))
	}
	return store.IdempotencyRecord{Key: row.Key, RequestHash: row.RequestHash, RunID: row.RunID, ExpiresAt: row.ExpiresAt}, nil
}

func (s idempotency) DeleteExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	res, err := s.c.ExecContext(ctx,
		`DELETE FROM idempotency_records WHERE id IN (SELECT id FROM idempotency_records WHERE expires_at <= $1 LIMIT $2)`, now, limit)
	if err != nil {
		return 0, fmt.Errorf("idempotency.delete_expired: %w", mapErr(err, opDelete))
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ---- SMS limits (atomic conditional increments)

type smsLimits struct{ c *ent.Client }

func reserve(ctx context.Context, c *ent.Client, table, keyCols string, keyArgs []any, n, limit int) (bool, error) {
	if n <= 0 {
		return true, nil
	}
	// Upsert the day's row, then increment only if the result stays within
	// the limit: one statement each, safe under concurrency.
	ph := make([]any, 0, len(keyArgs)+1)
	ph = append(ph, uuid.Must(uuid.NewV7()))
	ph = append(ph, keyArgs...)
	vals := "$1"
	for i := range keyArgs {
		vals += fmt.Sprintf(", $%d", i+2)
	}
	if _, err := c.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (id, %s, count) VALUES (%s, 0) ON CONFLICT (%s) DO NOTHING`, table, keyCols, vals, keyCols), ph...); err != nil {
		return false, mapErr(err, opCreate)
	}
	where := ""
	args := []any{n, limit}
	for i, col := range splitCols(keyCols) {
		where += fmt.Sprintf(" AND %s = $%d", col, i+3)
	}
	args = append(args, keyArgs...)
	res, err := c.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET count = count + $1 WHERE count + $1 <= $2%s`, table, where), args...)
	if err != nil {
		return false, mapErr(err, opWrite)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return rows == 1, nil
}

func release(ctx context.Context, c *ent.Client, table, keyCols string, keyArgs []any, n int) error {
	where := ""
	args := []any{n}
	for i, col := range splitCols(keyCols) {
		where += fmt.Sprintf(" AND %s = $%d", col, i+2)
	}
	args = append(args, keyArgs...)
	_, err := c.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET count = GREATEST(count - $1, 0) WHERE true%s`, table, where), args...)
	return mapErr(err, opWrite)
}

func splitCols(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		switch r {
		case ',':
			out = append(out, cur)
			cur = ""
		case ' ':
		default:
			cur += string(r)
		}
	}
	return append(out, cur)
}

func (s smsLimits) Reserve(ctx context.Context, tid store.TenantID, d time.Time, n, limit int) (bool, error) {
	ok, err := reserve(ctx, s.c, "platform_sms_usage", "tenant_id, day", []any{tid.UUID(), day(d)}, n, limit)
	if err != nil {
		return false, fmt.Errorf("sms.reserve: %w", err)
	}
	return ok, nil
}

func (s smsLimits) Release(ctx context.Context, tid store.TenantID, d time.Time, n int) error {
	if err := release(ctx, s.c, "platform_sms_usage", "tenant_id, day", []any{tid.UUID(), day(d)}, n); err != nil {
		return fmt.Errorf("sms.release: %w", err)
	}
	return nil
}

func (s smsLimits) ReservePlatform(ctx context.Context, d time.Time, n, limit int) (bool, error) {
	ok, err := reserve(ctx, s.c, "platform_sms_totals", "day", []any{day(d)}, n, limit)
	if err != nil {
		return false, fmt.Errorf("sms.reserve_platform: %w", err)
	}
	return ok, nil
}

func (s smsLimits) ReleasePlatform(ctx context.Context, d time.Time, n int) error {
	if err := release(ctx, s.c, "platform_sms_totals", "day", []any{day(d)}, n); err != nil {
		return fmt.Errorf("sms.release_platform: %w", err)
	}
	return nil
}

func (s smsLimits) Count(ctx context.Context, tid store.TenantID, d time.Time) (int, error) {
	var n sql.NullInt64
	if err := queryOne(ctx, s.c, &n, `SELECT count FROM platform_sms_usage WHERE tenant_id = $1 AND day = $2`, tid.UUID(), day(d)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("sms.count: %w", mapErr(err, opRead))
	}
	return int(n.Int64), nil
}

// ---- usage records

type usage struct{ c *ent.Client }

func (s usage) Record(ctx context.Context, tid store.TenantID, u store.UsageRecord) (bool, error) {
	res, err := s.c.ExecContext(ctx,
		`INSERT INTO usage_records (id, tenant_id, notification_id, channel, credential_mode, billing_basis, country, billable_at, unit_price_ref)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (notification_id) DO NOTHING`,
		uuid.Must(uuid.NewV7()), tid.UUID(), u.NotificationID, u.Channel, u.CredentialMode, u.BillingBasis, u.Country, u.BillableAt, u.UnitPriceRef)
	if err != nil {
		return false, fmt.Errorf("usage.record: %w", mapErr(err, opCreate))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

func (s usage) Count(ctx context.Context, tid store.TenantID) (int, error) {
	n, err := s.c.UsageRecord.Query().Count(scoped(ctx, tid))
	if err != nil {
		return 0, fmt.Errorf("usage.count: %w", mapErr(err, opRead))
	}
	return n, nil
}

// ---- leader leases

type leases struct{ c *ent.Client }

func (s leases) Acquire(ctx context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error) {
	res, err := s.c.ExecContext(ctx,
		`INSERT INTO scheduler_leases (id, name, holder, expires_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (name) DO UPDATE SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
		 WHERE scheduler_leases.holder = EXCLUDED.holder OR scheduler_leases.expires_at < $5`,
		uuid.Must(uuid.NewV7()), name, holder, now.Add(ttl), now)
	if err != nil {
		return false, fmt.Errorf("leases.acquire: %w", mapErr(err, opWrite))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return n == 1, nil
}

func (s leases) Release(ctx context.Context, name, holder string) error {
	if _, err := s.c.ExecContext(ctx, `DELETE FROM scheduler_leases WHERE name = $1 AND holder = $2`, name, holder); err != nil {
		return fmt.Errorf("leases.release: %w", mapErr(err, opDelete))
	}
	return nil
}
