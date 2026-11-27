package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Session is a console login (ADR-011 §1). Only the SHA-256 of the token is
// stored. User-scoped, not tenant-owned: one session spans all of the user's
// tenants, and roles are resolved per request.
type Session struct{ ent.Schema }

// Mixin of the Session.
func (Session) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}} }

// Fields of the Session.
func (Session) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}).Immutable(),
		field.Bytes("token_hash").NotEmpty().Sensitive().Immutable(),
		field.Time("created_at").Immutable(),
		field.Time("last_seen_at"),
		field.Time("absolute_expires_at").Immutable(),
		// Last password confirmation; sensitive actions need it within 10 min.
		field.Time("reauthenticated_at"),
		field.Time("revoked_at").Optional().Nillable(),
		field.String("user_agent").MaxLen(256).Default("").Immutable(),
	}
}

// Indexes of the Session.
func (Session) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("token_hash").Unique(),
		index.Fields("user_id", "revoked_at"),
		index.Fields("absolute_expires_at"), // cleanup job
	}
}
