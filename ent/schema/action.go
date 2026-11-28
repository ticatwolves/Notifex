package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Action is a workflow's channel. Actions run independently and in parallel;
// one per channel per workflow.
type Action struct{ ent.Schema }

// Mixin of the Action.
func (Action) Mixin() []ent.Mixin {
	return []ent.Mixin{IDMixin{}, TenantMixin{}, TimeMixin{}}
}

// Fields of the Action.
func (Action) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("workflow_id", uuid.UUID{}).Immutable(),
		field.Enum("channel").Values("email", "sms", "push", "slack", "outlook", "whatsapp", "webhook").Immutable(),
		field.Bool("enabled").Default(true),
	}
}

// Indexes of the Action.
func (Action) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "workflow_id", "channel").Unique(),
		// Target of the templates (tenant_id, action_id) foreign key.
		index.Fields("tenant_id", "id").Unique(),
	}
}
