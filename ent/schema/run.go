package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Run is one accepted trigger. The payload is encrypted under the tenant DEK
// (AAD: tenant, run id, "payload") and purged after retention (ADR-006). It
// references environments and workflows by id without foreign keys: runs are
// history and outlive the configuration they were accepted with.
type Run struct{ ent.Schema }

// Mixin of the Run.
func (Run) Mixin() []ent.Mixin {
	// No id default: the caller generates it first (it is bound into the
	// payload's AAD and its time orders the run's notifications).
	return []ent.Mixin{TenantMixin{}, TimeMixin{}}
}

// Fields of the Run.
func (Run) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Immutable(),
		field.UUID("application_id", uuid.UUID{}).Immutable(),
		field.UUID("environment_id", uuid.UUID{}).Immutable(),
		field.UUID("workflow_id", uuid.UUID{}).Immutable(),
		field.String("workflow_key").NotEmpty().MaxLen(63).Immutable(),
		field.Bytes("snapshot_hash").NotEmpty().Immutable(),
		field.Bytes("payload").Optional().Nillable().Sensitive(),
		field.JSON("context", map[string]any{}).Optional(),
		field.String("idempotency_key").Optional().Nillable().MaxLen(128).Immutable(),
		field.String("source").NotEmpty().MaxLen(32).Immutable(),
		field.String("actor_type").NotEmpty().MaxLen(16).Immutable(),
		field.String("actor_id").NotEmpty().MaxLen(64).Immutable(),
		field.Enum("status").Values("accepted", "processing", "completed", "partially_failed", "failed", "canceled").Default("accepted"),
		field.Time("schedule_at").Optional().Nillable().Immutable(),
		field.Time("terminal_at").Optional().Nillable(),
		field.Time("purged_at").Optional().Nillable(),
	}
}

// Indexes of the Run.
func (Run) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "id").Unique(),
		// Listing runs of one application environment, newest first.
		index.Fields("tenant_id", "application_id", "environment_id", "id"),
	}
}
