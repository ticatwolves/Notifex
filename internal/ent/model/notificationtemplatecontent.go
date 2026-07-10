package model

import (
	"encoding/json"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type NotificationTemplateContent struct {
	ent.Schema
}

func (NotificationTemplateContent) Mixin() []ent.Mixin {
	return []ent.Mixin{
		BaseMixin{},
	}
}

func (NotificationTemplateContent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("template_id", uuid.UUID{}).Immutable(),
		field.Enum("channel").
			Values(
				"EMAIL",
				"SMS",
				"PUSH",
				"SLACK",
				"TEAMS",
				"WEBHOOK",
				"WHATSAPP",
			),
		field.String("subject").
			Optional(),
		field.Text("content"),
		field.JSON("variables", []string{}).
			Optional(),
		field.JSON("default_context", json.RawMessage{}).
			Optional(),
		field.Bool("enabled").
			Default(true),
	}
}

func (NotificationTemplateContent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("template", NotificationTemplate.Type).
			Ref("contents").
			Field("template_id").
			Unique().
			Required().
			Immutable(),
		edge.To("deliveries", NotificationDelivery.Type),
	}
}

func (NotificationTemplateContent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("template_id"),
		index.Fields("channel"),
		index.Fields("enabled"),
		index.Fields("template_id", "channel").Unique(),
	}
}

func (NotificationTemplateContent) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "notification_template_contents",
		},
	}
}
