package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// SendingPolicy is a tenant's sending policy, or an application's override of it.
// payload_retention_days is a real column (tenant rows only, 1–7, ADR-006)
// so the purge job reads it without parsing JSON, and the database enforces
// the range.
type SendingPolicy struct{ ent.Schema }

// Mixin of the SendingPolicy.
func (SendingPolicy) Mixin() []ent.Mixin {
	return []ent.Mixin{IDMixin{}, TenantMixin{}, TimeMixin{}}
}

// Fields of the SendingPolicy.
func (SendingPolicy) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("application_id", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.JSON("doc", map[string]any{}),
		field.Int("payload_retention_days").Optional().Nillable(),
	}
}

// Indexes of the SendingPolicy.
func (SendingPolicy) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id").Unique().
			Annotations(entsql.IndexWhere("application_id IS NULL")).StorageKey("policies_tenant_key"),
		index.Fields("tenant_id", "application_id").Unique().
			Annotations(entsql.IndexWhere("application_id IS NOT NULL")).StorageKey("policies_application_key"),
	}
}

// Annotations of the SendingPolicy.
func (SendingPolicy) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "policies"},
		entsql.Checks(map[string]string{
			"policies_doc_object": "jsonb_typeof(doc) = 'object'",
			// Retention is tenant-level only, 1–7 days (ADR-006).
			"policies_retention_range":       "payload_retention_days IS NULL OR payload_retention_days BETWEEN 1 AND 7",
			"policies_retention_tenant_only": "application_id IS NULL OR payload_retention_days IS NULL",
		}),
	}
}
