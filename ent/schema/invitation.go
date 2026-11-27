package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Invitation invites an email address to join a tenant with a role
// (ADR-011 §4). Only the SHA-256 of the emailed token is stored.
type Invitation struct{ ent.Schema }

// Mixin of the Invitation.
func (Invitation) Mixin() []ent.Mixin {
	return []ent.Mixin{IDMixin{}, TenantMixin{}}
}

// Fields of the Invitation.
func (Invitation) Fields() []ent.Field {
	return []ent.Field{
		// Stored lower-cased; accepting requires a session with this email.
		field.String("email").NotEmpty().MaxLen(320).Immutable(),
		field.Enum("role").Values("owner", "admin", "developer", "viewer").Immutable(),
		field.Bytes("token_hash").NotEmpty().Sensitive().Immutable(),
		field.UUID("invited_by", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.Time("created_at").Immutable(),
		field.Time("expires_at").Immutable(),
		field.Time("accepted_at").Optional().Nillable(),
		field.UUID("accepted_by", uuid.UUID{}).Optional().Nillable(),
		field.Time("revoked_at").Optional().Nillable(),
	}
}

// Indexes of the Invitation.
func (Invitation) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("token_hash").Unique(),
		// At most one live invitation per email per tenant; re-inviting
		// replaces it (the service revokes the old one first).
		index.Fields("tenant_id", "email").Unique().
			Annotations(entsql.IndexWhere("accepted_at IS NULL AND revoked_at IS NULL")),
		index.Fields("tenant_id", "created_at"),
	}
}
