package model

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type NotificationTemplate struct {
	ent.Schema
}

func (NotificationTemplate) Mixin() []ent.Mixin {
	return []ent.Mixin{
		BaseMixin{},
	}
}

func (NotificationTemplate) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("app_id", uuid.UUID{}).
			Immutable(),
		field.String("name").
			NotEmpty().
			MaxLen(150),
		field.String("slug").
			NotEmpty().
			Unique().
			MaxLen(100),
		field.String("description").
			Optional().
			MaxLen(1000),
		field.Bool("enabled").
			Default(true),
	}
}

func (NotificationTemplate) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("app", App.Type).
			Ref("notification_templates").
			Field("app_id").
			Unique().
			Required().Immutable(),
		edge.To("contents", NotificationTemplateContent.Type),
		edge.To("notifications", Notification.Type),
	}
}

func (NotificationTemplate) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("app_id"),
		index.Fields("slug").Unique(),
		index.Fields("enabled"),
	}
}

func (NotificationTemplate) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "notification_templates",
		},
	}
}
