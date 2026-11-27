package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// AuditEvent records a control-plane mutation. Append-only. tenant_id is NULL
// for platform-level events, so this table does not use TenantMixin; the store
// scopes every read explicitly. before/after are redacted by the caller.
type AuditEvent struct{ ent.Schema }

// Mixin of the AuditEvent.
func (AuditEvent) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}} }

// Fields of the AuditEvent.
func (AuditEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("tenant_id", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.Enum("actor_type").Values("user", "api_key", "operator", "system").Immutable(),
		field.String("actor_id").MaxLen(128).Immutable(),
		// e.g. "application.create".
		field.String("action").NotEmpty().MaxLen(128).Immutable(),
		field.String("entity_type").NotEmpty().MaxLen(64).Immutable(),
		field.String("entity_id").MaxLen(128).Immutable(),
		field.JSON("before", map[string]any{}).Optional().Immutable(),
		field.JSON("after", map[string]any{}).Optional().Immutable(),
		field.String("request_id").MaxLen(64).Default("").Immutable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

// Indexes of the AuditEvent.
func (AuditEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "created_at"),
		index.Fields("created_at"),
	}
}
