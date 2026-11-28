package entstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/brandprofile"
	"github.com/ticatwolves/notifex/ent/predicate"
	"github.com/ticatwolves/notifex/ent/sendingpolicy"
	"github.com/ticatwolves/notifex/internal/store"
)

// upsert runs update → (create on miss) → (update again if a concurrent
// create won the unique index). Both kinds of settings share it.
func upsert(ctx context.Context, update func() (int, error), create func() error) error {
	n, err := update()
	if err != nil || n > 0 {
		return err
	}
	err = create()
	if errors.Is(err, store.ErrConflict) { // lost a race with another first write
		_, err = update()
	}
	return err
}

func toSettings(appID *uuid.UUID, doc map[string]any, ret *int, updated time.Time) store.Settings {
	if doc == nil {
		doc = map[string]any{}
	}
	return store.Settings{ApplicationID: appID, Doc: doc, PayloadRetentionDays: ret, UpdatedAt: updated}
}

// ---- brand profiles

type brands struct{ c *ent.Client }

func brandScope(tid store.TenantID, appID *uuid.UUID) predicate.BrandProfile {
	if appID == nil {
		return brandprofile.And(brandprofile.TenantID(tid.UUID()), brandprofile.ApplicationIDIsNil())
	}
	return brandprofile.And(brandprofile.TenantID(tid.UUID()), brandprofile.ApplicationID(*appID))
}

func (b brands) Get(ctx context.Context, tid store.TenantID, appID *uuid.UUID) (store.Settings, error) {
	row, err := b.c.BrandProfile.Query().Where(brandScope(tid, appID)).Only(scoped(ctx, tid))
	if err != nil {
		return store.Settings{}, fmt.Errorf("brands.get: %w", mapErr(err, opRead))
	}
	return toSettings(row.ApplicationID, row.Doc, nil, row.UpdatedAt), nil
}

func (b brands) Put(ctx context.Context, tid store.TenantID, appID *uuid.UUID, doc map[string]any, ret *int) (store.Settings, error) {
	if ret != nil {
		return store.Settings{}, invalid("brand profiles have no retention")
	}
	if doc == nil {
		doc = map[string]any{}
	}
	sctx := scoped(ctx, tid)
	err := upsert(ctx,
		func() (int, error) {
			n, err := b.c.BrandProfile.Update().Where(brandScope(tid, appID)).SetDoc(doc).Save(sctx)
			return n, mapErr(err, opWrite)
		},
		func() error {
			_, err := b.c.BrandProfile.Create().SetTenantID(tid.UUID()).SetNillableApplicationID(appID).SetDoc(doc).Save(sctx)
			return mapErr(err, opCreate)
		})
	if err != nil {
		return store.Settings{}, fmt.Errorf("brands.put: %w", err)
	}
	return b.Get(ctx, tid, appID)
}

func (b brands) Delete(ctx context.Context, tid store.TenantID, appID uuid.UUID) error {
	n, err := b.c.BrandProfile.Delete().Where(brandScope(tid, &appID)).Exec(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("brands.delete: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		return fmt.Errorf("brands.delete: %w", store.ErrNotFound)
	}
	return nil
}

// ---- policies

type policies struct{ c *ent.Client }

func policyScope(tid store.TenantID, appID *uuid.UUID) predicate.SendingPolicy {
	if appID == nil {
		return sendingpolicy.And(sendingpolicy.TenantID(tid.UUID()), sendingpolicy.ApplicationIDIsNil())
	}
	return sendingpolicy.And(sendingpolicy.TenantID(tid.UUID()), sendingpolicy.ApplicationID(*appID))
}

func (p policies) Get(ctx context.Context, tid store.TenantID, appID *uuid.UUID) (store.Settings, error) {
	row, err := p.c.SendingPolicy.Query().Where(policyScope(tid, appID)).Only(scoped(ctx, tid))
	if err != nil {
		return store.Settings{}, fmt.Errorf("policies.get: %w", mapErr(err, opRead))
	}
	return toSettings(row.ApplicationID, row.Doc, row.PayloadRetentionDays, row.UpdatedAt), nil
}

func (p policies) Put(ctx context.Context, tid store.TenantID, appID *uuid.UUID, doc map[string]any, ret *int) (store.Settings, error) {
	if appID != nil && ret != nil {
		return store.Settings{}, invalid("retention is tenant-level only")
	}
	if ret != nil && (*ret < 1 || *ret > 7) {
		return store.Settings{}, invalid("payload_retention_days must be 1-7")
	}
	if doc == nil {
		doc = map[string]any{}
	}
	sctx := scoped(ctx, tid)
	err := upsert(ctx,
		func() (int, error) {
			u := p.c.SendingPolicy.Update().Where(policyScope(tid, appID)).SetDoc(doc)
			if ret != nil {
				u.SetPayloadRetentionDays(*ret)
			} else {
				u.ClearPayloadRetentionDays()
			}
			n, err := u.Save(sctx)
			return n, mapErr(err, opWrite)
		},
		func() error {
			_, err := p.c.SendingPolicy.Create().SetTenantID(tid.UUID()).SetNillableApplicationID(appID).
				SetDoc(doc).SetNillablePayloadRetentionDays(ret).Save(sctx)
			return mapErr(err, opCreate)
		})
	if err != nil {
		return store.Settings{}, fmt.Errorf("policies.put: %w", err)
	}
	return p.Get(ctx, tid, appID)
}

func (p policies) Delete(ctx context.Context, tid store.TenantID, appID uuid.UUID) error {
	n, err := p.c.SendingPolicy.Delete().Where(policyScope(tid, &appID)).Exec(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("policies.delete: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		return fmt.Errorf("policies.delete: %w", store.ErrNotFound)
	}
	return nil
}
