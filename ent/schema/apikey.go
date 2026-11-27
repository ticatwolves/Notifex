package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// APIKey authenticates programmatic access for one application environment.
// Only a keyed hash of the secret is stored; the full key is shown once.
type APIKey struct{ ent.Schema }

// Mixin of the APIKey.
func (APIKey) Mixin() []ent.Mixin {
	return []ent.Mixin{IDMixin{}, TenantMixin{}, TimeMixin{}}
}

// Fields of the APIKey.
func (APIKey) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("application_id", uuid.UUID{}).Immutable(),
		field.UUID("environment_id", uuid.UUID{}).Immutable(),
		field.String("name").NotEmpty().MaxLen(200),
		// Public lookup handle embedded in the key; unique across all tenants.
		field.String("prefix").NotEmpty().MaxLen(32).Unique().Immutable(),
		// HMAC-SHA256(pepper, secret).
		field.Bytes("hash").NotEmpty().Sensitive().Immutable(),
		field.Strings("scopes").Default([]string{}),
		field.Time("expires_at").Optional().Nillable(),
		field.Time("last_used_at").Optional().Nillable(),
		field.Time("revoked_at").Optional().Nillable(),
	}
}

// Indexes of the APIKey.
func (APIKey) Indexes() []ent.Index {
	return []ent.Index{index.Fields("tenant_id", "application_id")}
}
