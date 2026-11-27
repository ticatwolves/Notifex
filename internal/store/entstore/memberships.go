package entstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/membership"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/tenantscope"
)

type memberships struct{ c *ent.Client }

func (m memberships) Create(ctx context.Context, tid store.TenantID, userID uuid.UUID, role store.Role) (store.Membership, error) {
	row, err := m.c.Membership.Create().
		SetTenantID(tid.UUID()).
		SetUserID(userID).
		SetRole(membership.Role(role)).
		Save(scoped(ctx, tid))
	if err != nil {
		return store.Membership{}, fmt.Errorf("memberships.create: %w", mapErr(err, opCreate))
	}
	return toMembership(row), nil
}

func (m memberships) Get(ctx context.Context, tid store.TenantID, userID uuid.UUID) (store.Membership, error) {
	row, err := m.c.Membership.Query().
		Where(membership.TenantID(tid.UUID()), membership.UserID(userID)).
		Only(scoped(ctx, tid))
	if err != nil {
		return store.Membership{}, fmt.Errorf("memberships.get: %w", mapErr(err, opRead))
	}
	return toMembership(row), nil
}

func (m memberships) List(ctx context.Context, tid store.TenantID, p store.Page) ([]store.Membership, error) {
	p = p.Normalize()
	q := m.c.Membership.Query().Where(membership.TenantID(tid.UUID()))
	if p.After != uuid.Nil {
		q.Where(membership.IDGT(p.After))
	}
	rows, err := q.Order(ent.Asc(membership.FieldID)).Limit(p.Limit).All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("memberships.list: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toMembership), nil
}

func (m memberships) UpdateRole(ctx context.Context, tid store.TenantID, userID uuid.UUID, role store.Role) (store.Membership, error) {
	ctx = scoped(ctx, tid)
	n, err := m.c.Membership.Update().
		Where(membership.TenantID(tid.UUID()), membership.UserID(userID)).
		SetRole(membership.Role(role)).
		Save(ctx)
	if err != nil {
		return store.Membership{}, fmt.Errorf("memberships.update_role: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		return store.Membership{}, fmt.Errorf("memberships.update_role: %w", store.ErrNotFound)
	}
	return m.Get(ctx, tid, userID)
}

func (m memberships) Delete(ctx context.Context, tid store.TenantID, userID uuid.UUID) error {
	n, err := m.c.Membership.Delete().
		Where(membership.TenantID(tid.UUID()), membership.UserID(userID)).
		Exec(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("memberships.delete: %w", mapErr(err, opDelete))
	}
	if n == 0 {
		return fmt.Errorf("memberships.delete: %w", store.ErrNotFound)
	}
	return nil
}

func (m memberships) ListForUser(ctx context.Context, userID uuid.UUID) ([]store.Membership, error) {
	rows, err := m.c.Membership.Query().
		Where(membership.UserID(userID)).
		Order(ent.Asc(membership.FieldID)).
		Limit(store.MaxPageSize).
		All(tenantscope.Bypass(ctx, tenantscope.ReasonUserMemberships))
	if err != nil {
		return nil, fmt.Errorf("memberships.list_for_user: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toMembership), nil
}

func toMembership(r *ent.Membership) store.Membership {
	return store.Membership{
		ID: r.ID, TenantID: store.TenantID(r.TenantID), UserID: r.UserID,
		Role: store.Role(r.Role), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}
