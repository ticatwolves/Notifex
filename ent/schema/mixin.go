// Package schema holds the ent schemas for Notifex core tables.
//
// Conventions:
//   - ids are UUIDv7 (time-ordered: compact indexes, natural keyset pagination);
//   - every tenant-owned table embeds TenantMixin, which enforces tenant
//     scoping on every query and mutation (see internal/store/tenantscope);
//   - relationships are foreign keys declared in constraints.go, not ent
//     edges, because ent edges cannot express composite tenant-consistent keys.
package schema

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/mixin"
	"github.com/google/uuid"

	"github.com/sbshobhit/notifex/ent/intercept"
	"github.com/sbshobhit/notifex/internal/store/tenantscope"
)

// NewID returns a new UUIDv7.
func NewID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// IDMixin adds a UUIDv7 primary key.
type IDMixin struct{ mixin.Schema }

// Fields of the IDMixin.
func (IDMixin) Fields() []ent.Field {
	return []ent.Field{field.UUID("id", uuid.UUID{}).Default(NewID).Immutable()}
}

// TimeMixin adds created_at and updated_at.
type TimeMixin struct{ mixin.Schema }

// Fields of the TimeMixin.
func (TimeMixin) Fields() []ent.Field {
	return []ent.Field{
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

// TenantMixin adds tenant_id and enforces tenant scoping: queries get
// `tenant_id = <scope>` appended, updates/deletes too, and creates must carry
// the scope's tenant_id. Without a scope (or an explicit bypass) every
// operation fails with tenantscope.ErrMissing.
type TenantMixin struct{ mixin.Schema }

// Fields of the TenantMixin.
func (TenantMixin) Fields() []ent.Field {
	return []ent.Field{field.UUID("tenant_id", uuid.UUID{}).Immutable()}
}

// No standalone tenant_id index: every tenant-owned table declares a composite
// index led by tenant_id, which serves tenant_id lookups too.

type wherePer interface {
	WhereP(...func(*sql.Selector))
}

// Interceptors of the TenantMixin.
func (TenantMixin) Interceptors() []ent.Interceptor {
	return []ent.Interceptor{
		ent.TraverseFunc(func(ctx context.Context, q ent.Query) error {
			tid, ok := tenantscope.From(ctx)
			if !ok {
				if _, bypass := tenantscope.Bypassed(ctx); bypass {
					return nil
				}
				return tenantscope.ErrMissing
			}
			w, err := intercept.NewQuery(q)
			if err != nil {
				return fmt.Errorf("tenant scope: %w", err)
			}
			w.WhereP(sql.FieldEQ("tenant_id", tid))
			return nil
		}),
	}
}

// Hooks of the TenantMixin.
func (TenantMixin) Hooks() []ent.Hook {
	return []ent.Hook{
		func(next ent.Mutator) ent.Mutator {
			return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
				tid, scoped := tenantscope.From(ctx)
				if !scoped {
					if _, bypass := tenantscope.Bypassed(ctx); !bypass {
						return nil, tenantscope.ErrMissing
					}
				}
				if m.Op().Is(ent.OpCreate) {
					v, set := m.Field("tenant_id")
					switch {
					case !set && scoped:
						if err := m.SetField("tenant_id", tid); err != nil {
							return nil, fmt.Errorf("tenant scope: %w", err)
						}
					case !set:
						return nil, fmt.Errorf("tenant scope: create on %s without tenant_id", m.Type())
					case scoped && v != tid:
						return nil, fmt.Errorf("tenant scope: create on %s for another tenant: %w", m.Type(), tenantscope.ErrMissing)
					}
					return next.Mutate(ctx, m)
				}
				if scoped {
					w, ok := m.(wherePer)
					if !ok {
						return nil, fmt.Errorf("tenant scope: mutation %T cannot be filtered", m)
					}
					w.WhereP(sql.FieldEQ("tenant_id", tid))
				}
				return next.Mutate(ctx, m)
			})
		},
	}
}
