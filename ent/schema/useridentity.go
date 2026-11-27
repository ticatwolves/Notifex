package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// UserIdentity links an external OAuth identity to a user (ADR-011 §3).
// subject is the provider's stable id: Google `sub`, Microsoft `tid:oid`,
// GitHub numeric user id. Never the email, which can change or be reused.
type UserIdentity struct{ ent.Schema }

// Mixin of the UserIdentity.
func (UserIdentity) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}} }

// Fields of the UserIdentity.
func (UserIdentity) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}).Immutable(),
		field.Enum("provider").Values("google", "microsoft", "github").Immutable(),
		field.String("subject").NotEmpty().MaxLen(255).Immutable(),
		// Email asserted by the provider at link time (informational only).
		field.String("email").MaxLen(320).Default(""),
		field.Time("created_at").Immutable(),
		field.Time("last_used_at"),
	}
}

// Indexes of the UserIdentity.
func (UserIdentity) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("provider", "subject").Unique(),
		// One identity per provider per user keeps unlinking unambiguous.
		index.Fields("user_id", "provider").Unique(),
	}
}
