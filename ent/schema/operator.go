package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// Operator is a Notifex staff account (ADR-011 §5). A separate realm from
// tenant users: created only by the operator CLI, password + TOTP required.
type Operator struct{ ent.Schema }

// Mixin of the Operator.
func (Operator) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}, TimeMixin{}} }

// Fields of the Operator.
func (Operator) Fields() []ent.Field {
	return []ent.Field{
		field.String("email").NotEmpty().MaxLen(320).Unique(),
		field.String("name").MaxLen(200).Default(""),
		field.String("password_hash").NotEmpty().Sensitive(),
		// TOTP seed sealed under its own KMS-wrapped key, bound to the operator id.
		field.Bytes("totp_wrapped_key").NotEmpty().Sensitive(),
		field.String("totp_key_id").NotEmpty().MaxLen(256),
		field.Bytes("totp_ciphertext").NotEmpty().Sensitive(),
		// Highest TOTP time step accepted; a code is never accepted twice.
		field.Int64("totp_last_step").Default(0),
		field.Enum("status").Values("active", "disabled").Default("active"),
	}
}
