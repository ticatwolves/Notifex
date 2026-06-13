package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// User holds the schema definition for the User entity.
type User struct {
	ent.Schema
}

// Fields of the User.
func (User) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),

		field.String("email").
			MaxLen(320).
			NotEmpty().
			Unique(),

		field.String("full_name").
			MaxLen(256).
			NotEmpty(),

		field.String("password_hash").
			Optional().
			Nillable().
			Sensitive().
			Comment("bcrypt hash, cost=12"),

		field.Bool("email_verified").
			Default(false),

		field.Bool("active").
			Default(true).
			Comment("False = deactivated (cannot login)"),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),

		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),

		field.Time("deleted_at").
			Optional().
			Nillable().
			Comment("Soft delete; nil = active"),
	}
}

// Edges of the User.
func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("sessions", UserSession.Type),
	}
}

// Indexed of the User.
func (User) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("email").Unique(),
	}
}
