package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// EmailToken is a single-use token sent by email (verification, password
// reset). Only its SHA-256 is stored.
type EmailToken struct{ ent.Schema }

// Mixin of the EmailToken.
func (EmailToken) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}} }

// Fields of the EmailToken.
func (EmailToken) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("user_id", uuid.UUID{}).Immutable(),
		field.Enum("purpose").Values("verify_email", "reset_password").Immutable(),
		field.Bytes("token_hash").NotEmpty().Sensitive().Immutable(),
		field.Time("created_at").Immutable(),
		field.Time("expires_at").Immutable(),
		field.Time("used_at").Optional().Nillable(),
	}
}

// Indexes of the EmailToken.
func (EmailToken) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("token_hash").Unique(),
		index.Fields("user_id", "purpose"),
		index.Fields("expires_at"), // cleanup job
	}
}
