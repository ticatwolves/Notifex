package entstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/sbshobhit/notifex/ent"
	"github.com/sbshobhit/notifex/ent/auditevent"
	"github.com/sbshobhit/notifex/internal/store"
)

type audit struct{ c *ent.Client }

func (a audit) Append(ctx context.Context, e store.AuditEvent) (store.AuditEvent, error) {
	if e.Action == "" || e.EntityType == "" || e.ActorType == "" {
		return store.AuditEvent{}, invalid("audit event requires action, entity type and actor type")
	}
	c := a.c.AuditEvent.Create().
		SetActorType(auditevent.ActorType(e.ActorType)).
		SetActorID(e.ActorID).
		SetAction(e.Action).
		SetEntityType(e.EntityType).
		SetEntityID(e.EntityID).
		SetRequestID(e.RequestID)
	if e.TenantID != nil {
		c.SetTenantID(e.TenantID.UUID())
	}
	if e.Before != nil {
		c.SetBefore(e.Before)
	}
	if e.After != nil {
		c.SetAfter(e.After)
	}
	if !e.CreatedAt.IsZero() {
		c.SetCreatedAt(e.CreatedAt)
	}
	row, err := c.Save(ctx)
	if err != nil {
		return store.AuditEvent{}, fmt.Errorf("audit.append: %w", mapErr(err, opCreate))
	}
	return toAudit(row), nil
}

func (a audit) List(ctx context.Context, tid store.TenantID, p store.Page) ([]store.AuditEvent, error) {
	p = p.Normalize()
	q := a.c.AuditEvent.Query().Where(auditevent.TenantID(tid.UUID()))
	if p.After != uuid.Nil {
		q.Where(auditevent.IDLT(p.After)) // newest first
	}
	rows, err := q.Order(ent.Desc(auditevent.FieldID)).Limit(p.Limit).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("audit.list: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toAudit), nil
}

func toAudit(r *ent.AuditEvent) store.AuditEvent {
	e := store.AuditEvent{
		ID: r.ID, ActorType: store.ActorType(r.ActorType), ActorID: r.ActorID,
		Action: r.Action, EntityType: r.EntityType, EntityID: r.EntityID,
		Before: r.Before, After: r.After, RequestID: r.RequestID, CreatedAt: r.CreatedAt,
	}
	if r.TenantID != nil {
		tid := store.TenantID(*r.TenantID)
		e.TenantID = &tid
	}
	return e
}
