package schema

import "entgo.io/ent"

// DeliveryAttempt holds the schema definition for the DeliveryAttempt entity.
type DeliveryAttempt struct {
	ent.Schema
}

// Fields of the DeliveryAttempt.
func (DeliveryAttempt) Fields() []ent.Field {
	return nil
}

// Edges of the DeliveryAttempt.
func (DeliveryAttempt) Edges() []ent.Edge {
	return nil
}
