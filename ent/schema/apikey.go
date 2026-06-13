package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// APIKey holds the schema definition for the APIKey entity.
type APIKey struct {
	ent.Schema
}

// Fields of the APIKey.
func (APIKey) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),

		field.UUID("app_id", uuid.UUID{}).
			Immutable(),

		field.String("name").
			MaxLen(128).
			NotEmpty().
			Comment("Human label, e.g. production-server"),

		field.String("key_prefix").
			MaxLen(16).
			Immutable().
			Comment("First 8 chars of key shown in UI: nfx_live_xxxxxxxx"),

		field.String("key_hash").
			Sensitive().
			Comment("bcrypt hash of the full API key"),

		field.Enum("environment").
			Values("live", "test").
			Default("live"),

		field.JSON("scopes", []string{}).
			Default([]string{"notify:send"}).
			Comment("Allowed: notify:send, notify:read, templates:write, recipients:write, analytics:read"),

		field.Time("expires_at").
			Optional().
			Nillable().
			Comment("Nil = never expires"),

		field.Time("last_used_at").
			Optional().
			Nillable(),

		field.Bool("active").
			Default(true),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),

		field.Time("revoked_at").
			Optional().
			Nillable(),
	}
}

// Edges of the APIKey.
func (APIKey) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("app", App.Type).
			Ref("api_keys").
			Field("app_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Indexes of the APIKey.
func (APIKey) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("key_prefix").Unique(),
		index.Fields("app_id", "active"),
		index.Fields("expires_at"),
	}
}
