package schema

import "entgo.io/ent"

// UserPreference holds the schema definition for the UserPreference entity.
type UserPreference struct {
	ent.Schema
}

// Fields of the UserPreference.
func (UserPreference) Fields() []ent.Field {
	return nil
}

// Edges of the UserPreference.
func (UserPreference) Edges() []ent.Edge {
	return nil
}
