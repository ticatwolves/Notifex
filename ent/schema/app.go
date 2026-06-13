package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// App holds the schema definition of the App entity.
type App struct {
	ent.Schema
}

// Fields of the App.
func (App) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),

		field.String("name").
			MaxLen(128).
			NotEmpty(),

		field.String("slug").
			MaxLen(63).
			Unique().
			NotEmpty().
			Immutable().
			Comment("URL-safe identifier, e.g. acme-corp"),

		field.Enum("plan").
			Values("free", "starter", "pro").
			Default("free"),

		field.Int("monthly_quota").
			Default(1_000).
			Comment("Max notifications per billing period"),

		field.Int("quota_used").
			Default(0).
			Comment("Notifications sent in current billing period"),

		field.Time("quota_reset_at").
			Default(func() time.Time {
				now := time.Now().UTC()
				return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			}).
			Comment("When quota_used resets to 0"),

		field.Bool("active").
			Default(true),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),

		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the App.
func (App) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("api_keys", APIKey.Type),
		edge.To("templates", Template.Type),
		edge.To("notifications", Notification.Type),
		edge.To("notificationlog", NotificationLog.Type),
	}
}

// Indexes of the App.
func (App) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("slug"),
		index.Fields("active"),
	}
}
