package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// IdempotencyRecord maps a caller's Idempotency-Key to the run it created,
// for 24 h (ADR-014 §3). Written in the accept transaction.
type IdempotencyRecord struct{ ent.Schema }

// Mixin of the IdempotencyRecord.
func (IdempotencyRecord) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}, TenantMixin{}} }

// Fields of the IdempotencyRecord.
func (IdempotencyRecord) Fields() []ent.Field {
	return []ent.Field{
		field.String("key").NotEmpty().MaxLen(128).Immutable(),
		field.Bytes("request_hash").NotEmpty().Immutable(),
		field.UUID("run_id", uuid.UUID{}).Immutable(),
		field.Time("created_at").Immutable(),
		field.Time("expires_at").Immutable(),
	}
}

// Indexes of the IdempotencyRecord.
func (IdempotencyRecord) Indexes() []ent.Index {
	return []ent.Index{index.Fields("tenant_id", "key").Unique(), index.Fields("expires_at")}
}

// PlatformSMSUsage counts a tenant's platform SMS for one UTC day, reserved
// atomically at accept (ADR-005).
type PlatformSMSUsage struct{ ent.Schema }

// Mixin of the PlatformSMSUsage.
func (PlatformSMSUsage) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}, TenantMixin{}} }

// Fields of the PlatformSMSUsage.
func (PlatformSMSUsage) Fields() []ent.Field {
	return []ent.Field{
		field.Time("day").SchemaType(map[string]string{dialect.Postgres: "date"}).Immutable(),
		field.Int("count").Default(0),
	}
}

// Indexes of the PlatformSMSUsage.
func (PlatformSMSUsage) Indexes() []ent.Index {
	return []ent.Index{index.Fields("tenant_id", "day").Unique()}
}

// Annotations of the PlatformSMSUsage.
func (PlatformSMSUsage) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "platform_sms_usage"},
		entsql.Checks(map[string]string{"platform_sms_usage_count_nonneg": "count >= 0"}),
	}
}

// PlatformSMSTotal is the platform-wide daily SMS counter, kept below the AWS
// account's spend quota (ADR-005). Platform-level: no tenant.
type PlatformSMSTotal struct{ ent.Schema }

// Mixin of the PlatformSMSTotal.
func (PlatformSMSTotal) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}} }

// Fields of the PlatformSMSTotal.
func (PlatformSMSTotal) Fields() []ent.Field {
	return []ent.Field{
		field.Time("day").SchemaType(map[string]string{dialect.Postgres: "date"}).Unique().Immutable(),
		field.Int("count").Default(0),
	}
}

// Annotations of the PlatformSMSTotal.
func (PlatformSMSTotal) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "platform_sms_totals"},
		entsql.Checks(map[string]string{"platform_sms_totals_count_nonneg": "count >= 0"}),
	}
}

// UsageRecord is one billable notification (ADR-007), written in the same
// transaction as the billable status change. Unique per notification, so
// duplicate deliveries and late receipts can't bill twice.
type UsageRecord struct{ ent.Schema }

// Mixin of the UsageRecord.
func (UsageRecord) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}, TenantMixin{}} }

// Fields of the UsageRecord.
func (UsageRecord) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("notification_id", uuid.UUID{}).Unique().Immutable(),
		field.Enum("channel").Values("email", "sms", "push", "slack", "outlook", "whatsapp", "webhook").Immutable(),
		field.String("credential_mode").NotEmpty().MaxLen(16).Immutable(),
		field.String("billing_basis").NotEmpty().MaxLen(16).Immutable(),
		field.String("country").Default("").MaxLen(2).Immutable(),
		field.Time("billable_at").Immutable(),
		field.String("unit_price_ref").Default("").MaxLen(64).Immutable(),
	}
}

// Indexes of the UsageRecord.
func (UsageRecord) Indexes() []ent.Index {
	return []ent.Index{index.Fields("tenant_id", "billable_at")}
}

// SchedulerLease elects one scheduler leader per job group (ADR-014 §7).
// Platform-level: no tenant.
type SchedulerLease struct{ ent.Schema }

// Mixin of the SchedulerLease.
func (SchedulerLease) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}} }

// Fields of the SchedulerLease.
func (SchedulerLease) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty().MaxLen(64).Unique().Immutable(),
		field.String("holder").NotEmpty().MaxLen(128),
		field.Time("expires_at"),
	}
}
