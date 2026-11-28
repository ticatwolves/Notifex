package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

// NotificationEvent is one entry of a notification's timeline. Partitioned by
// day like notifications (hand-written DDL, skipped by ent migrations).
// detail is redacted: never payload, rendered content or raw recipients.
type NotificationEvent struct{ ent.Schema }

// Mixin of the NotificationEvent.
func (NotificationEvent) Mixin() []ent.Mixin { return []ent.Mixin{TenantMixin{}} }

// Fields of the NotificationEvent.
func (NotificationEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Immutable(),
		field.Time("created_at").Immutable(),
		field.UUID("notification_id", uuid.UUID{}).Immutable(),
		field.String("type").NotEmpty().MaxLen(32).Immutable(),
		field.JSON("detail", map[string]any{}).Optional().Immutable(),
	}
}

// Annotations of the NotificationEvent.
func (NotificationEvent) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Skip: true}}
}
