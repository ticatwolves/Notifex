package entstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/application"
	"github.com/ticatwolves/notifex/internal/store"
)

type applications struct{ c *ent.Client }

func (a applications) Create(ctx context.Context, tid store.TenantID, in store.NewApplication) (store.Application, error) {
	if !slugRE.MatchString(in.Key) {
		return store.Application{}, invalid("application key %q", in.Key)
	}
	row, err := a.c.Application.Create().
		SetTenantID(tid.UUID()).
		SetName(in.Name).
		SetKey(in.Key).
		SetDescription(in.Description).
		Save(scoped(ctx, tid))
	if err != nil {
		return store.Application{}, fmt.Errorf("applications.create: %w", mapErr(err, opCreate))
	}
	return toApplication(row), nil
}

func (a applications) Get(ctx context.Context, tid store.TenantID, id uuid.UUID) (store.Application, error) {
	row, err := a.c.Application.Query().
		Where(application.TenantID(tid.UUID()), application.ID(id)).
		Only(scoped(ctx, tid))
	if err != nil {
		return store.Application{}, fmt.Errorf("applications.get: %w", mapErr(err, opRead))
	}
	return toApplication(row), nil
}

func (a applications) List(ctx context.Context, tid store.TenantID, p store.Page) ([]store.Application, error) {
	p = p.Normalize()
	q := a.c.Application.Query().Where(application.TenantID(tid.UUID()))
	if p.After != uuid.Nil {
		q.Where(application.IDGT(p.After))
	}
	rows, err := q.Order(ent.Asc(application.FieldID)).Limit(p.Limit).All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("applications.list: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toApplication), nil
}

func (a applications) Update(ctx context.Context, tid store.TenantID, id uuid.UUID, p store.ApplicationPatch) (store.Application, error) {
	u := a.c.Application.UpdateOneID(id).Where(application.TenantID(tid.UUID()))
	if p.Name != nil {
		u.SetName(*p.Name)
	}
	if p.Description != nil {
		u.SetDescription(*p.Description)
	}
	row, err := u.Save(scoped(ctx, tid))
	if err != nil {
		return store.Application{}, fmt.Errorf("applications.update: %w", mapErr(err, opWrite))
	}
	return toApplication(row), nil
}

func (a applications) Delete(ctx context.Context, tid store.TenantID, id uuid.UUID) error {
	n, err := a.c.Application.Delete().
		Where(application.TenantID(tid.UUID()), application.ID(id)).
		Exec(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("applications.delete: %w", mapErr(err, opDelete))
	}
	if n == 0 {
		return fmt.Errorf("applications.delete: %w", store.ErrNotFound)
	}
	return nil
}

func toApplication(r *ent.Application) store.Application {
	return store.Application{
		ID: r.ID, TenantID: store.TenantID(r.TenantID), Name: r.Name, Key: r.Key,
		Description: r.Description, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
