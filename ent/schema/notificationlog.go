package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// NotificationLog holds the schema definition for the NotificationLog entity.
type NotificationLog struct {
	ent.Schema
}

// Fields of the NotificationLog.
func (NotificationLog) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("notification_id", uuid.UUID{}).
			Immutable(),

		field.UUID("app_id", uuid.UUID{}).
			Immutable(),
		field.Enum("channel").
			Values(
				"email", "sms", "push",
				"slack", "teams", "discord",
				"whatsapp", "webhook",
			),
		// The provider actually used (e.g. "ses", "smtp", "twilio")
		field.String("provider").
			Optional().
			Nillable().
			MaxLen(32),
		field.Enum("status").
			Values(
				"pending",   // created, not yet dispatched
				"sending",   // in-flight to provider
				"delivered", // provider confirmed delivery
				"bounced",   // permanent bounce (email)
				"failed",    // provider rejected or unreachable
				"skipped",   // recipient opted out, quiet hours, etc.
				"expired",   // notification expired before attempt
			).
			Default("pending"),
		field.String("provider_message_id").
			Optional().
			Nillable().
			MaxLen(256).
			Comment("e.g. SES MessageId, Twilio SID, FCM message_id"),
		field.Text("rendered_subject").
			Optional().
			Nillable(),

		field.Text("rendered_body").
			Optional().
			Nillable().
			Comment("First 2048 bytes of rendered body, for audit"),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),

		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the NotificationLog.
func (NotificationLog) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("notification", Notification.Type).
			Ref("notificationlog").
			Required().
			Immutable(),
		edge.From("app", App.Type).
			Ref("notificationlog").
			Required().
			Immutable(),
	}
}

// Indexes of the NotificationLog.
func (NotificationLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("notification_id", "channel"),
		index.Fields("app_id", "channel", "status", "created_at"),
	}
}
