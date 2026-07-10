package model

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// APIKey authenticates requests on behalf of an App.
type APIKey struct {
	ent.Schema
}

func (APIKey) Mixin() []ent.Mixin {
	return []ent.Mixin{
		BaseMixin{},
	}
}

func (APIKey) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("app_id", uuid.UUID{}).Immutable(),
		field.String("name").
			NotEmpty().
			MaxLen(100),
		field.String("key_hash").
			NotEmpty().
			Sensitive(),
		field.Time("expires_at").
			Optional().
			Nillable(),
		field.Bool("active").Default(true),
	}
}

func (APIKey) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("app", App.Type).
			Ref("api_keys").
			Field("app_id").
			Unique().
			Required().Immutable(),
	}
}

func (APIKey) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("app_id"),
		index.Fields("active"),
		index.Fields("expires_at"),
	}
}

func (APIKey) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "api_keys",
		},
	}
}
