package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Environment is a deployment stage of an Application (dev, staging, prod).
// tenant_id is denormalised from the application so the composite foreign key
// (tenant_id, application_id) makes cross-tenant references impossible.
type Environment struct{ ent.Schema }

// Mixin of the Environment.
func (Environment) Mixin() []ent.Mixin {
	return []ent.Mixin{IDMixin{}, TenantMixin{}, TimeMixin{}}
}

// Fields of the Environment.
func (Environment) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("application_id", uuid.UUID{}).Immutable(),
		field.Enum("name").Values("dev", "staging", "prod").Immutable(),
	}
}

// Indexes of the Environment.
func (Environment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "application_id", "name").Unique(),
		// Target of the api_keys (tenant_id, application_id, environment_id) foreign key.
		index.Fields("tenant_id", "application_id", "id").Unique(),
	}
}
