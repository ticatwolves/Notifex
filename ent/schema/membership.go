package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Membership links a User to a Tenant with a console role.
type Membership struct{ ent.Schema }

// Mixin of the Membership.
func (Membership) Mixin() []ent.Mixin {
	return []ent.Mixin{IDMixin{}, TenantMixin{}, TimeMixin{}}
}

// Fields of the Membership.
func (Membership) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}).Immutable(),
		field.Enum("role").Values("owner", "admin", "developer", "viewer"),
	}
}

// Indexes of the Membership.
func (Membership) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "user_id").Unique(),
		index.Fields("user_id"),
	}
}
