package schema

import "entgo.io/ent"

// NotificationLog holds the schema definition for the NotificationLog entity.
type NotificationLog struct {
	ent.Schema
}

// Fields of the NotificationLog.
func (NotificationLog) Fields() []ent.Field {
	return nil
}

// Edges of the NotificationLog.
func (NotificationLog) Edges() []ent.Edge {
	return nil
}
