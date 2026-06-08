package schema

import "entgo.io/ent"

// SecurityAuditLog holds the schema definition for the SecurityAuditLog entity.
type SecurityAuditLog struct {
	ent.Schema
}

// Fields of the SecurityAuditLog.
func (SecurityAuditLog) Fields() []ent.Field {
	return nil
}

// Edges of the SecurityAuditLog.
func (SecurityAuditLog) Edges() []ent.Edge {
	return nil
}
