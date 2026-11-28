package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// DispatchOutbox holds queue messages written in the accept (or dispatch)
// transaction; the relay publishes them to SQS (ADR-004). ids only.
type DispatchOutbox struct{ ent.Schema }

// Mixin of the DispatchOutbox.
func (DispatchOutbox) Mixin() []ent.Mixin { return []ent.Mixin{TenantMixin{}} }

// Fields of the DispatchOutbox.
func (DispatchOutbox) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"), // bigserial: publish order
		field.UUID("notification_id", uuid.UUID{}).Immutable(),
		field.UUID("run_id", uuid.UUID{}).Immutable(),
		field.Enum("channel").Values("email", "sms", "push", "slack", "outlook", "whatsapp", "webhook").Immutable(),
		field.Int("delay_s").Default(0).Immutable(),
		field.Int("attempt").Default(1).Immutable(),
		field.String("traceparent").Default("").MaxLen(64).Immutable(),
		field.Time("created_at").Immutable(),
		field.Time("published_at").Optional().Nillable(),
	}
}

// Indexes of the DispatchOutbox.
func (DispatchOutbox) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("id").Annotations(entsql.IndexWhere("published_at IS NULL")).StorageKey("dispatch_outbox_unpublished"),
		index.Fields("published_at").Annotations(entsql.IndexWhere("published_at IS NOT NULL")).StorageKey("dispatch_outbox_published"),
		index.Fields("tenant_id", "notification_id"),
	}
}

// Annotations of the DispatchOutbox.
func (DispatchOutbox) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "dispatch_outbox"},
		entsql.Checks(map[string]string{"dispatch_outbox_delay_range": "delay_s BETWEEN 0 AND 900"}),
	}
}

// ScheduledDispatch holds work due more than 15 minutes out (scheduled
// triggers, quiet-hours deferral, long backoff); the scheduler moves due rows
// into the outbox (ADR-004).
type ScheduledDispatch struct{ ent.Schema }

// Mixin of the ScheduledDispatch.
func (ScheduledDispatch) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}, TenantMixin{}} }

// Fields of the ScheduledDispatch.
func (ScheduledDispatch) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("notification_id", uuid.UUID{}).Immutable(),
		field.UUID("run_id", uuid.UUID{}).Immutable(),
		field.Enum("channel").Values("email", "sms", "push", "slack", "outlook", "whatsapp", "webhook").Immutable(),
		field.Time("due_at").Immutable(),
		field.Enum("reason").Values("scheduled", "deferred", "backoff").Immutable(),
		field.Int("attempt").Default(1).Immutable(),
		field.String("traceparent").Default("").MaxLen(64).Immutable(),
		field.Time("created_at").Immutable(),
	}
}

// Indexes of the ScheduledDispatch.
func (ScheduledDispatch) Indexes() []ent.Index {
	return []ent.Index{index.Fields("due_at"), index.Fields("tenant_id", "notification_id")}
}

// Annotations of the ScheduledDispatch.
func (ScheduledDispatch) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "scheduled_dispatch"}}
}
