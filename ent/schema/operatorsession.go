package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// OperatorSession is an operator login (nfo_ tokens; only the SHA-256 is
// stored). Separate from console sessions so the realms never mix.
type OperatorSession struct{ ent.Schema }

// Mixin of the OperatorSession.
func (OperatorSession) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}} }

// Fields of the OperatorSession.
func (OperatorSession) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("operator_id", uuid.UUID{}).Immutable(),
		field.Bytes("token_hash").NotEmpty().Sensitive().Immutable(),
		field.Time("created_at").Immutable(),
		field.Time("last_seen_at"),
		field.Time("absolute_expires_at").Immutable(),
		field.Time("revoked_at").Optional().Nillable(),
		field.String("user_agent").MaxLen(256).Default("").Immutable(),
	}
}

// Indexes of the OperatorSession.
func (OperatorSession) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("token_hash").Unique(),
		index.Fields("operator_id", "revoked_at"),
		index.Fields("absolute_expires_at"),
	}
}
