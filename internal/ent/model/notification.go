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

type Notification struct {
	ent.Schema
}

func (Notification) Mixin() []ent.Mixin {
	return []ent.Mixin{
		BaseMixin{},
	}
}

func (Notification) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("app_id", uuid.UUID{}).Immutable(),

		field.UUID("template_id", uuid.UUID{}).Immutable(),

		field.JSON("payload", json.RawMessage{}),

		field.JSON("context", json.RawMessage{}).
			Optional(),

		field.Enum("status").
			Values(
				"QUEUED",
				"PROCESSING",
				"COMPLETED",
				"FAILED",
				"PARTIAL",
			).
			Default("QUEUED"),

		field.Int("total_channels").
			Default(0),

		field.Int("processed_channels").
			Default(0),

		field.Int("success_channels").
			Default(0),

		field.Int("failed_channels").
			Default(0),

		field.String("error").
			Optional(),

		field.Time("completed_at").
			Optional().
			Nillable(),
	}
}

func (Notification) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("app", App.Type).
			Ref("notifications").
			Field("app_id").
			Unique().
			Required().Immutable(),

		edge.From("template", NotificationTemplate.Type).
			Ref("notifications").
			Field("template_id").
			Unique().
			Required().Immutable(),

		edge.To("deliveries", NotificationDelivery.Type),
	}
}

func (Notification) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("app_id"),
		index.Fields("template_id"),
		index.Fields("status"),
		index.Fields("created_at"),
		index.Fields("completed_at"),
	}
}

func (Notification) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "notifications",
		},
	}
}
