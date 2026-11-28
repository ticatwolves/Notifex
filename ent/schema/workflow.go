package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Workflow is one notification: a key triggers address it by, and channel
// actions. There are no version tables: runs snapshot it at accept time.
type Workflow struct{ ent.Schema }

// Mixin of the Workflow.
func (Workflow) Mixin() []ent.Mixin {
	return []ent.Mixin{IDMixin{}, TenantMixin{}, TimeMixin{}}
}

// Fields of the Workflow.
func (Workflow) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("application_id", uuid.UUID{}).Immutable(),
		field.String("key").NotEmpty().MaxLen(63).Immutable(),
		field.String("name").NotEmpty().MaxLen(200),
		field.String("description").MaxLen(2000).Default(""),
		field.Enum("status").Values("draft", "active", "archived").Default("draft"),
		field.JSON("payload_schema", map[string]any{}).Optional(),
	}
}

// Indexes of the Workflow.
func (Workflow) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "application_id", "key").Unique(),
		// Target of the actions (tenant_id, workflow_id) foreign key.
		index.Fields("tenant_id", "id").Unique(),
	}
}
