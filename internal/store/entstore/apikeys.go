package entstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/apikey"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/tenantscope"
)

// lastUsedGranularity throttles last_used_at writes: a key used on every
// trigger request writes at most once per interval instead of once per call.
const lastUsedGranularity = time.Minute

type apiKeys struct{ c *ent.Client }

func (k apiKeys) Create(ctx context.Context, tid store.TenantID, in store.NewAPIKey) (store.APIKey, error) {
	if in.Prefix == "" || len(in.Hash) == 0 {
		return store.APIKey{}, invalid("api key requires prefix and hash")
	}
	scopes := in.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	row, err := k.c.APIKey.Create().
		SetTenantID(tid.UUID()).
		SetApplicationID(in.ApplicationID).
		SetEnvironmentID(in.EnvironmentID).
		SetName(in.Name).
		SetPrefix(in.Prefix).
		SetHash(in.Hash).
		SetScopes(scopes).
		SetNillableExpiresAt(in.ExpiresAt).
		Save(scoped(ctx, tid))
	if err != nil {
		return store.APIKey{}, fmt.Errorf("api_keys.create: %w", mapErr(err, opCreate))
	}
	return toAPIKey(row), nil
}

func (k apiKeys) GetByPrefix(ctx context.Context, prefix string) (store.APIKey, error) {
	row, err := k.c.APIKey.Query().
		Where(apikey.Prefix(prefix)).
		Only(tenantscope.Bypass(ctx, tenantscope.ReasonAPIKeyLookup))
	if err != nil {
		return store.APIKey{}, fmt.Errorf("api_keys.get_by_prefix: %w", mapErr(err, opRead))
	}
	return toAPIKey(row), nil
}

func (k apiKeys) List(ctx context.Context, tid store.TenantID, appID uuid.UUID, p store.Page) ([]store.APIKey, error) {
	p = p.Normalize()
	q := k.c.APIKey.Query().Where(apikey.TenantID(tid.UUID()), apikey.ApplicationID(appID))
	if p.After != uuid.Nil {
		q.Where(apikey.IDGT(p.After))
	}
	rows, err := q.Order(ent.Asc(apikey.FieldID)).Limit(p.Limit).All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("api_keys.list: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toAPIKey), nil
}

func (k apiKeys) Revoke(ctx context.Context, tid store.TenantID, id uuid.UUID, at time.Time) error {
	ctx = scoped(ctx, tid)
	n, err := k.c.APIKey.Update().
		Where(apikey.TenantID(tid.UUID()), apikey.ID(id), apikey.RevokedAtIsNil()).
		SetRevokedAt(at).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("api_keys.revoke: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		// Already revoked (idempotent) or not in this tenant.
		exists, err := k.c.APIKey.Query().Where(apikey.TenantID(tid.UUID()), apikey.ID(id)).Exist(ctx)
		if err != nil {
			return fmt.Errorf("api_keys.revoke: %w", mapErr(err, opRead))
		}
		if !exists {
			return fmt.Errorf("api_keys.revoke: %w", store.ErrNotFound)
		}
	}
	return nil
}

func (k apiKeys) TouchLastUsed(ctx context.Context, tid store.TenantID, id uuid.UUID, at time.Time) error {
	_, err := k.c.APIKey.Update().
		Where(
			apikey.TenantID(tid.UUID()), apikey.ID(id),
			apikey.Or(apikey.LastUsedAtIsNil(), apikey.LastUsedAtLT(at.Add(-lastUsedGranularity))),
		).
		SetLastUsedAt(at).
		Save(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("api_keys.touch_last_used: %w", mapErr(err, opWrite))
	}
	return nil
}

func toAPIKey(r *ent.APIKey) store.APIKey {
	return store.APIKey{
		ID: r.ID, TenantID: store.TenantID(r.TenantID), ApplicationID: r.ApplicationID,
		EnvironmentID: r.EnvironmentID, Name: r.Name, Prefix: r.Prefix, Hash: r.Hash,
		Scopes: r.Scopes, ExpiresAt: r.ExpiresAt, LastUsedAt: r.LastUsedAt, RevokedAt: r.RevokedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (k apiKeys) RevokeAllForTenant(ctx context.Context, tid store.TenantID, at time.Time) (int, error) {
	n, err := k.c.APIKey.Update().
		Where(apikey.TenantID(tid.UUID()), apikey.RevokedAtIsNil()).
		SetRevokedAt(at).
		Save(scoped(ctx, tid))
	if err != nil {
		return 0, fmt.Errorf("api_keys.revoke_all_for_tenant: %w", mapErr(err, opWrite))
	}
	return n, nil
}
