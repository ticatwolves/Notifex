package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// User is a console user. Users are global (one login, many tenants via
// Membership), so this is the one documented table without tenant_id.
type User struct{ ent.Schema }

// Mixin of the User.
func (User) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}, TimeMixin{}} }

// Fields of the User.
func (User) Fields() []ent.Field {
	return []ent.Field{
		// Stored lower-cased by the store; unique.
		field.String("email").NotEmpty().MaxLen(320).Unique(),
		field.String("name").MaxLen(200).Default(""),
		// argon2id PHC string; NULL for OAuth-only users.
		field.String("password_hash").Optional().Nillable().Sensitive(),
		field.Enum("status").Values("active", "disabled").Default("active"),
		field.Time("email_verified_at").Optional().Nillable(),
	}
}
