package schema

import "entgo.io/ent"

// UserCredential holds the schema definition for the UserCredential entity.
type UserCredential struct {
	ent.Schema
}

// Fields of the UserCredential.
func (UserCredential) Fields() []ent.Field {
	return nil
}

// Edges of the UserCredential.
func (UserCredential) Edges() []ent.Edge {
	return nil
}
