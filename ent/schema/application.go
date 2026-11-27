package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Application groups workflows, environments and API keys within a tenant.
type Application struct{ ent.Schema }

// Mixin of the Application.
func (Application) Mixin() []ent.Mixin {
	return []ent.Mixin{IDMixin{}, TenantMixin{}, TimeMixin{}}
}

// Fields of the Application.
func (Application) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty().MaxLen(200),
		field.String("key").NotEmpty().MaxLen(63).Immutable(),
		field.String("description").MaxLen(2000).Default(""),
	}
}

// Indexes of the Application.
func (Application) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "key").Unique(),
		// Target of composite (tenant_id, application_id) foreign keys.
		index.Fields("tenant_id", "id").Unique(),
	}
}
