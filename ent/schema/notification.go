package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Notification holds the schema definition for the Notification entity.
type Notification struct {
	ent.Schema
}

// Fields of the Notification.
func (Notification) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),

		field.UUID("app_id", uuid.UUID{}).
			Immutable(),

		// Template-based send
		field.UUID("template_id", uuid.UUID{}).
			Optional().
			Nillable(),

		// Template variable values: {"user_name":"Shobhit","plan":"Pro"}
		field.JSON("data", map[string]interface{}{}).
			Optional().
			Comment("Template variable substitution data"),

		// Explicit channels to use. Empty = derive from template configuration.
		field.JSON("channels", []string{}).
			Optional().
			Comment("e.g. ['email','push']; empty = use routing rules + prefs"),

		// Status of the overall notification (aggregate across all channels)
		field.Enum("status").
			Values(
				"pending",   // created, not yet queued
				"queued",    // in the worker queue
				"sending",   // at least one channel attempt in progress
				"delivered", // all requested channels delivered
				"partial",   // some channels delivered, some failed
				"failed",    // all channels failed (after retries)
			).
			Default("pending"),

		field.JSON("metadata", map[string]interface{}{}).
			Optional(),

		field.Time("delivered_at").
			Optional().
			Nillable().
			Comment("Time of first successful delivery across any channel"),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),

		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the Notification.
func (Notification) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("app", App.Type).
			Ref("notifications").
			Field("app_id").
			Unique().
			Required().
			Immutable(),
		edge.To("notificationlog", NotificationLog.Type),
	}
}

// Indexes of the Notification.
func (Notification) Indexes() []ent.Index {
	return []ent.Index{
		// Idempotency deduplication per app
		index.Fields("app_id").
			Unique().
			StorageKey("uidx_notif_tenant_idempotency"),

		// Scheduler polls this index
		index.Fields("status"),

		// Tenant analytics queries
		index.Fields("app_id", "status", "created_at"),
		index.Fields("app_id", "created_at"),

		// Recipient history
		index.Fields("created_at"),
	}
}
