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

type NotificationDelivery struct {
	ent.Schema
}

func (NotificationDelivery) Mixin() []ent.Mixin {
	return []ent.Mixin{
		BaseMixin{},
	}
}

func (NotificationDelivery) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("notification_id", uuid.UUID{}).Immutable(),
		field.UUID("template_content_id", uuid.UUID{}).Immutable(),
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
		field.Enum("status").
			Values(
				"PENDING",
				"QUEUED",
				"PROCESSING",
				"SENT",
				"DELIVERED",
				"FAILED",
				"RETRYING",
				"CANCELLED",
			).
			Default("PENDING"),
		// Queue retry count
		field.Int("retry_count").
			Default(0),
		// Maximum retries allowed
		field.Int("max_retry").
			Default(3),
		// Channel destination
		// email, phone, slack channel etc.
		field.String("destination").
			Optional(),
		// Provider used (SES, Twilio, Slack...)
		field.String("provider").
			Optional(),
		// Provider response id
		field.String("provider_message_id").
			Optional(),
		// Raw provider response
		field.JSON("provider_response", json.RawMessage{}).
			Optional(),
		// Error message
		field.Text("error").
			Optional(),
		field.Time("queued_at").
			Optional().
			Nillable(),
		field.Time("started_at").
			Optional().
			Nillable(),
		field.Time("sent_at").
			Optional().
			Nillable(),
		field.Time("delivered_at").
			Optional().
			Nillable(),
		field.Time("next_retry_at").
			Optional().
			Nillable(),
		field.Time("expired_at").
			Optional().
			Nillable(),
		field.Time("last_attempt_at").
			Optional().
			Nillable(),
	}
}

func (NotificationDelivery) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("notification", Notification.Type).
			Ref("deliveries").
			Field("notification_id").
			Unique().
			Required().Immutable(),

		edge.From("template_content", NotificationTemplateContent.Type).
			Ref("deliveries").
			Field("template_content_id").
			Unique().
			Required().Immutable(),
	}
}

func (NotificationDelivery) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("notification_id"),
		index.Fields("template_content_id"),
		index.Fields("status"),
		index.Fields("channel"),
		index.Fields("queued_at"),
		index.Fields("next_retry_at"),
		index.Fields("created_at"),
		index.Fields("notification_id", "channel"),
	}
}

func (NotificationDelivery) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "notification_deliveries",
		},
	}
}
