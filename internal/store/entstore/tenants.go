package entstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/tenant"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/store"
)

type tenants struct{ c *ent.Client }

func (t tenants) Create(ctx context.Context, in store.NewTenant) (store.Tenant, error) {
	if in.ID.UUID() == uuid.Nil {
		return store.Tenant{}, invalid("tenant id is required")
	}
	if !slugRE.MatchString(in.Slug) {
		return store.Tenant{}, invalid("tenant slug %q", in.Slug)
	}
	if len(in.DEK.Wrapped) == 0 || in.DEK.KeyID == "" || in.DEK.Version == 0 {
		return store.Tenant{}, invalid("tenant requires a wrapped DEK")
	}
	plan := in.Plan
	if plan == "" {
		plan = "free"
	}
	row, err := t.c.Tenant.Create().
		SetID(in.ID.UUID()).
		SetName(in.Name).
		SetSlug(in.Slug).
		SetPlan(plan).
		SetPlatformSmsDailyLimit(in.PlatformSMSDailyLimit).
		SetDekWrapped(in.DEK.Wrapped).
		SetDekKeyID(in.DEK.KeyID).
		SetDekVersion(int(in.DEK.Version)).
		Save(ctx)
	if err != nil {
		return store.Tenant{}, fmt.Errorf("tenants.create: %w", mapErr(err, opCreate))
	}
	return toTenant(row), nil
}

func (t tenants) Get(ctx context.Context, id store.TenantID) (store.Tenant, error) {
	row, err := t.c.Tenant.Get(ctx, id.UUID())
	if err != nil {
		return store.Tenant{}, fmt.Errorf("tenants.get: %w", mapErr(err, opRead))
	}
	return toTenant(row), nil
}

func (t tenants) GetBySlug(ctx context.Context, slug string) (store.Tenant, error) {
	row, err := t.c.Tenant.Query().Where(tenant.Slug(slug)).Only(ctx)
	if err != nil {
		return store.Tenant{}, fmt.Errorf("tenants.get_by_slug: %w", mapErr(err, opRead))
	}
	return toTenant(row), nil
}

func (t tenants) Update(ctx context.Context, id store.TenantID, p store.TenantPatch) (store.Tenant, error) {
	u := t.c.Tenant.UpdateOneID(id.UUID())
	if p.Name != nil {
		u.SetName(*p.Name)
	}
	if p.Status != nil {
		u.SetStatus(tenant.Status(*p.Status))
	}
	if p.Plan != nil {
		u.SetPlan(*p.Plan)
	}
	if p.Quotas != nil {
		u.SetQuotas(p.Quotas)
	}
	if p.PlatformSMSDailyLimit != nil {
		u.SetPlatformSmsDailyLimit(*p.PlatformSMSDailyLimit)
	}
	if p.Sandbox != nil {
		u.SetSandbox(*p.Sandbox)
	}
	row, err := u.Save(ctx)
	if err != nil {
		return store.Tenant{}, fmt.Errorf("tenants.update: %w", mapErr(err, opWrite))
	}
	return toTenant(row), nil
}

func (t tenants) WrappedDEK(ctx context.Context, id uuid.UUID) (crypto.WrappedDEK, error) {
	row, err := t.c.Tenant.Query().Where(tenant.ID(id)).
		Select(tenant.FieldDekWrapped, tenant.FieldDekKeyID, tenant.FieldDekVersion).Only(ctx)
	if err != nil {
		return crypto.WrappedDEK{}, fmt.Errorf("tenants.wrapped_dek: %w", mapErr(err, opRead))
	}
	if row.DekWrapped == nil || len(*row.DekWrapped) == 0 {
		return crypto.WrappedDEK{}, crypto.ErrDataKeyDestroyed
	}
	return crypto.WrappedDEK{Wrapped: *row.DekWrapped, KeyID: row.DekKeyID, Version: uint8(row.DekVersion)}, nil //nolint:gosec // G115: CHECK constraint bounds dek_version to 1..255
}

func (t tenants) ShredDEK(ctx context.Context, id store.TenantID) error {
	n, err := t.c.Tenant.Update().Where(tenant.ID(id.UUID())).ClearDekWrapped().Save(ctx)
	if err != nil {
		return fmt.Errorf("tenants.shred_dek: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		return fmt.Errorf("tenants.shred_dek: %w", store.ErrNotFound)
	}
	return nil
}

func toTenant(r *ent.Tenant) store.Tenant {
	return store.Tenant{
		ID:                    store.TenantID(r.ID),
		Name:                  r.Name,
		Slug:                  r.Slug,
		Status:                store.TenantStatus(r.Status),
		Plan:                  r.Plan,
		Quotas:                r.Quotas,
		PlatformSMSDailyLimit: r.PlatformSmsDailyLimit,
		Sandbox:               r.Sandbox,
		DEKShredded:           r.DekWrapped == nil || len(*r.DekWrapped) == 0,
		CreatedAt:             r.CreatedAt,
		UpdatedAt:             r.UpdatedAt,
	}
}

func (t tenants) Search(ctx context.Context, q string, status *store.TenantStatus, p store.Page) ([]store.Tenant, error) {
	p = p.Normalize()
	query := t.c.Tenant.Query()
	if q = strings.TrimSpace(q); q != "" {
		// HasPrefix/ContainsFold escape LIKE wildcards in q.
		query.Where(tenant.Or(tenant.SlugHasPrefix(strings.ToLower(q)), tenant.NameContainsFold(q)))
	}
	if status != nil {
		query.Where(tenant.StatusEQ(tenant.Status(*status)))
	}
	if p.After != uuid.Nil {
		query.Where(tenant.IDGT(p.After))
	}
	rows, err := query.Order(ent.Asc(tenant.FieldID)).Limit(p.Limit).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenants.search: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toTenant), nil
}
