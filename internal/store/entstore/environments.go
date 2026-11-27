package entstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/environment"
	"github.com/ticatwolves/notifex/internal/store"
)

type environments struct{ c *ent.Client }

func (e environments) Create(ctx context.Context, tid store.TenantID, appID uuid.UUID, name store.EnvironmentName) (store.Environment, error) {
	row, err := e.c.Environment.Create().
		SetTenantID(tid.UUID()).
		SetApplicationID(appID).
		SetName(environment.Name(name)).
		Save(scoped(ctx, tid))
	if err != nil {
		// A composite-FK violation (app not in this tenant) maps to ErrNotFound.
		return store.Environment{}, fmt.Errorf("environments.create: %w", mapErr(err, opCreate))
	}
	return toEnvironment(row), nil
}

func (e environments) Get(ctx context.Context, tid store.TenantID, id uuid.UUID) (store.Environment, error) {
	row, err := e.c.Environment.Query().
		Where(environment.TenantID(tid.UUID()), environment.ID(id)).
		Only(scoped(ctx, tid))
	if err != nil {
		return store.Environment{}, fmt.Errorf("environments.get: %w", mapErr(err, opRead))
	}
	return toEnvironment(row), nil
}

func (e environments) List(ctx context.Context, tid store.TenantID, appID uuid.UUID) ([]store.Environment, error) {
	rows, err := e.c.Environment.Query().
		Where(environment.TenantID(tid.UUID()), environment.ApplicationID(appID)).
		Order(ent.Asc(environment.FieldID)).
		Limit(store.MaxPageSize).
		All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("environments.list: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toEnvironment), nil
}

func (e environments) Delete(ctx context.Context, tid store.TenantID, id uuid.UUID) error {
	n, err := e.c.Environment.Delete().
		Where(environment.TenantID(tid.UUID()), environment.ID(id)).
		Exec(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("environments.delete: %w", mapErr(err, opDelete))
	}
	if n == 0 {
		return fmt.Errorf("environments.delete: %w", store.ErrNotFound)
	}
	return nil
}

func toEnvironment(r *ent.Environment) store.Environment {
	return store.Environment{
		ID: r.ID, TenantID: store.TenantID(r.TenantID), ApplicationID: r.ApplicationID,
		Name: store.EnvironmentName(r.Name), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
