package schema

import "entgo.io/ent"

// APIKey holds the schema definition for the APIKey entity.
type APIKey struct {
	ent.Schema
}

// Fields of the APIKey.
func (APIKey) Fields() []ent.Field {
	return nil
}

// Edges of the APIKey.
func (APIKey) Edges() []ent.Edge {
	return nil
}
