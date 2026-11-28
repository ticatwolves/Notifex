package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ConfigSnapshot is the immutable, deduplicated configuration a run was
// accepted with: workflow, enabled actions, their templates, effective brand
// and policy, resolved credential references (ADR-014 §2). Keyed by the
// SHA-256 of its canonical JSON. Never updated; deleted once unreferenced.
type ConfigSnapshot struct{ ent.Schema }

// Mixin of the ConfigSnapshot.
func (ConfigSnapshot) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}, TenantMixin{}} }

// Fields of the ConfigSnapshot.
func (ConfigSnapshot) Fields() []ent.Field {
	return []ent.Field{
		field.Bytes("hash").NotEmpty().Immutable(),
		field.JSON("doc", map[string]any{}).Immutable(),
		field.Time("created_at").Immutable(),
	}
}

// Indexes of the ConfigSnapshot.
func (ConfigSnapshot) Indexes() []ent.Index {
	// Target of the runs (tenant_id, snapshot_hash) foreign key.
	return []ent.Index{index.Fields("tenant_id", "hash").Unique()}
}
