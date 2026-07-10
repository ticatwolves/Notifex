package model

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// App is the top-level tenant. Every other entity belongs to an App.
type App struct {
	ent.Schema
}

func (App) Mixin() []ent.Mixin {
	return []ent.Mixin{
		BaseMixin{},
	}
}

func (App) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").
			MaxLen(128).
			NotEmpty(),

		field.String("slug").
			MaxLen(63).
			Unique().
			NotEmpty().
			Immutable().
			Comment("URL-safe identifier, e.g. acme-corp"),

		field.String("description").Optional(),
		field.Bool("active").
			Default(true),
	}
}

func (App) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("api_keys", APIKey.Type),
		edge.To("notifications", Notification.Type),
		edge.To("notification_templates", NotificationTemplate.Type),
	}
}

// Indexes of the App.
func (App) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug").
			Unique(),
		index.Fields("active"),
		index.Fields("created_at"),
	}
}

func (App) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{
			Table: "apps",
		},
	}
}
