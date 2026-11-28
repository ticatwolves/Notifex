package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Template is an action's content for one locale (ADR-012). The content's
// fields depend on the channel, so it is stored as JSON.
type Template struct{ ent.Schema }

// Mixin of the Template.
func (Template) Mixin() []ent.Mixin {
	return []ent.Mixin{IDMixin{}, TenantMixin{}, TimeMixin{}}
}

// Fields of the Template.
func (Template) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("action_id", uuid.UUID{}).Immutable(),
		field.String("locale").NotEmpty().MaxLen(16).Immutable(),
		field.JSON("content", map[string]any{}),
	}
}

// Indexes of the Template.
func (Template) Indexes() []ent.Index {
	return []ent.Index{index.Fields("tenant_id", "action_id", "locale").Unique()}
}

// Annotations of the Template.
func (Template) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Checks(map[string]string{"templates_content_object": "jsonb_typeof(content) = 'object'"}),
	}
}
