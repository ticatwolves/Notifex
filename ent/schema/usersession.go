package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// UserSession holds the schema definition for the UserSession entity.
type UserSession struct {
	ent.Schema
}

// Fields of the UserSession.
func (UserSession) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),

		field.UUID("user_id", uuid.UUID{}).
			Immutable(),

		field.String("refresh_token_hash").
			Unique().
			MaxLen(64).
			Sensitive().
			Comment("SHA-256 hex of the raw refresh token"),

		field.String("user_agent").
			Optional().
			Nillable().
			MaxLen(512),

		field.String("ip_address").
			Optional().
			Nillable().
			MaxLen(45).
			Comment("IPv4 or IPv6 of the session creator"),

		// Human-readable label shown in "active sessions" dashboard UI.
		// e.g. "Chrome on macOS · Mumbai"
		field.String("device_label").
			Optional().
			Nillable().
			MaxLen(256),

		field.Time("expires_at").
			Comment("Refresh token expiry; default 30 days from creation"),

		field.Bool("revoked").
			Default(false),

		field.Time("revoked_at").
			Optional().
			Nillable(),

		field.String("revoke_reason").
			Optional().
			Nillable().
			MaxLen(128).
			Comment("logout | password_changed | admin_revoke | expired | suspicious"),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Edges of the UserSession.
func (UserSession) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("sessions").
			Field("user_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Indexes of the UserSession.
func (UserSession) Indexes() []ent.Index {
	return []ent.Index{
		// Primary token lookup on every authenticated request
		index.Fields("refresh_token_hash").Unique(),

		// "Show all active sessions" — user sessions dashboard
		index.Fields("user_id", "revoked", "expires_at"),

		// Cleanup job: find expired sessions to purge
		index.Fields("expires_at", "revoked"),
	}
}
