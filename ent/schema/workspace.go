package schema

import "entgo.io/ent"

// Workspace holds the schema definition for the Workspace entity.
type Workspace struct {
	ent.Schema
}

// Fields of the Workspace.
func (Workspace) Fields() []ent.Field {
	return nil
}

// Edges of the Workspace.
func (Workspace) Edges() []ent.Edge {
	return nil
}
