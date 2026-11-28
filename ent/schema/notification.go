package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// Notification is one (action × recipient) of a run. The table is
// range-partitioned by day on created_at, which is the UUIDv7 id's timestamp
// (ADR-014 §1); its DDL is hand-written (ManualTables), so ent skips it in
// migrations. Encrypted columns use AAD (tenant, notification id, column).
type Notification struct{ ent.Schema }

// Mixin of the Notification.
func (Notification) Mixin() []ent.Mixin { return []ent.Mixin{TenantMixin{}} }

// Fields of the Notification.
func (Notification) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Immutable(),
		field.Time("created_at").Immutable(),
		field.Time("updated_at"),
		field.UUID("run_id", uuid.UUID{}).Immutable(),
		field.UUID("application_id", uuid.UUID{}).Immutable(),
		field.UUID("environment_id", uuid.UUID{}).Immutable(),
		field.UUID("action_id", uuid.UUID{}).Immutable(),
		field.Enum("channel").Values("email", "sms", "push", "slack", "outlook", "whatsapp", "webhook").Immutable(),
		field.String("locale").Default("").MaxLen(16),
		field.Enum("status").Values("queued", "deferred", "sending", "retrying", "sent", "delivered",
			"sent_unconfirmed", "failed", "suppressed", "canceled", "expired"),
		field.Int("attempt").Default(0),
		field.Time("lease_until").Optional().Nillable(),
		field.Time("next_attempt_at").Optional().Nillable(),
		field.Bytes("recipient").Optional().Nillable().Sensitive(),
		field.String("recipient_masked").Default(""),
		field.Bytes("recipient_hash").Optional().Nillable(),
		field.String("recipient_ref").Default("").MaxLen(128),
		field.UUID("credential_id", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.String("credential_mode").Default("").MaxLen(16).Immutable(),
		field.String("provider").Default("").MaxLen(32).Immutable(),
		field.String("billing_basis").Default("").MaxLen(16).Immutable(),
		field.Bool("sms_reserved").Default(false),
		field.String("last_error_code").Default("").MaxLen(64),
		field.String("last_error").Default("").MaxLen(1024),
		field.String("vendor_message_id").Default("").MaxLen(256),
		field.Bytes("rendered_content").Optional().Nillable().Sensitive(),
		field.Bytes("vendor_response").Optional().Nillable().Sensitive(),
		field.Time("terminal_at").Optional().Nillable(),
		field.Time("purged_at").Optional().Nillable(),
	}
}

// Annotations of the Notification.
func (Notification) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Skip: true}}
}
