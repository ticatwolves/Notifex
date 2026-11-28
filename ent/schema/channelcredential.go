package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ChannelCredential is a vendor credential for one channel, at tenant,
// application or environment scope (ADR-005, ADR-013). The secret is
// envelope-encrypted under the tenant DEK; config holds non-secret settings.
type ChannelCredential struct{ ent.Schema }

// Mixin of the ChannelCredential.
func (ChannelCredential) Mixin() []ent.Mixin {
	// No IDMixin default: the caller generates the id first because it is
	// bound into the secret's AAD.
	return []ent.Mixin{TenantMixin{}, TimeMixin{}}
}

// Fields of the ChannelCredential.
func (ChannelCredential) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Immutable(),
		field.UUID("application_id", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.UUID("environment_id", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.Enum("channel").Values("email", "sms", "push", "slack", "outlook", "whatsapp", "webhook").Immutable(),
		field.String("provider").NotEmpty().MaxLen(32).Immutable(),
		field.Enum("mode").Values("platform", "byo").Immutable(),
		field.String("name").NotEmpty().MaxLen(200),
		field.JSON("config", map[string]any{}),
		field.Bytes("secret_ciphertext").Optional().Nillable().Sensitive(),
		field.String("secret_hint").MaxLen(8).Default(""),
		field.Enum("status").Values("pending", "verified", "disabled").Default("pending"),
		field.Time("verified_at").Optional().Nillable(),
		field.Time("rotated_at").Optional().Nillable(),
	}
}

// Indexes of the ChannelCredential: at most one non-disabled credential per
// scope and channel. Three partial indexes, one per scope shape, so NULL
// application/environment ids compare equal (ADR-013 §1).
func (ChannelCredential) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "channel").Unique().
			Annotations(entsql.IndexWhere("application_id IS NULL AND status <> 'disabled'")).StorageKey("channel_credentials_tenant_scope_key"),
		index.Fields("tenant_id", "application_id", "channel").Unique().
			Annotations(entsql.IndexWhere("application_id IS NOT NULL AND environment_id IS NULL AND status <> 'disabled'")).StorageKey("channel_credentials_app_scope_key"),
		index.Fields("tenant_id", "application_id", "environment_id", "channel").Unique().
			Annotations(entsql.IndexWhere("environment_id IS NOT NULL AND status <> 'disabled'")).StorageKey("channel_credentials_env_scope_key"),
		index.Fields("tenant_id", "id").Unique(),
	}
}

// Annotations of the ChannelCredential.
func (ChannelCredential) Annotations() []schema.Annotation {
	return []schema.Annotation{
		//nolint:gosec // G101 false positive: constraint names containing "secret", not credentials
		entsql.Checks(map[string]string{
			"channel_credentials_env_needs_app": "environment_id IS NULL OR application_id IS NOT NULL",
			"channel_credentials_config_object": "jsonb_typeof(config) = 'object'",
			"channel_credentials_secret_size":   "secret_ciphertext IS NULL OR octet_length(secret_ciphertext) <= 20000",
		}),
	}
}
