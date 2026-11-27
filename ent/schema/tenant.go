package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// Tenant is a customer organisation. It is the root of tenancy, so it is not
// itself tenant-scoped; access is controlled by the store (platform operations).
type Tenant struct{ ent.Schema }

// Mixin of the Tenant.
func (Tenant) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}, TimeMixin{}} }

// Fields of the Tenant.
func (Tenant) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty().MaxLen(200),
		field.String("slug").NotEmpty().MaxLen(63).Unique().Immutable(),
		field.Enum("status").Values("active", "suspended", "deleted").Default("active"),
		field.String("plan").NotEmpty().MaxLen(64).Default("free"),
		field.JSON("quotas", map[string]int64{}).Optional(),
		field.Int("platform_sms_daily_limit").Default(0).NonNegative(),
		field.Bool("sandbox").Default(true),
		// Tenant DEK wrapped by the KeyProvider (ADR-006). NULL after crypto-shred.
		field.Bytes("dek_wrapped").Optional().Nillable().Sensitive(),
		field.String("dek_key_id").NotEmpty().MaxLen(256),
		field.Int("dek_version").Default(1).Range(1, 255),
	}
}

// Annotations of the Tenant.
func (Tenant) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Checks(map[string]string{
			"tenants_platform_sms_daily_limit_nonneg": "platform_sms_daily_limit >= 0",
			"tenants_dek_version_range":               "dek_version BETWEEN 1 AND 255",
		}),
	}
}
