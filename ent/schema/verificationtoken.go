package schema

import "entgo.io/ent"

// VerificationToken holds the schema definition for the VerificationToken entity.
type VerificationToken struct {
	ent.Schema
}

// Fields of the VerificationToken.
func (VerificationToken) Fields() []ent.Field {
	return nil
}

// Edges of the VerificationToken.
func (VerificationToken) Edges() []ent.Edge {
	return nil
}
