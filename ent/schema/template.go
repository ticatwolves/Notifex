package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// Template holds the schema definition for the Template entity.
type Template struct {
	ent.Schema
}

// Fields of the Template.
func (Template) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),

		field.UUID("app_id", uuid.UUID{}).
			Immutable(),

		field.String("slug").
			MaxLen(128).
			NotEmpty(),

		field.String("name").
			MaxLen(256).
			NotEmpty().
			Comment("Human display name"),

		field.String("description").
			Optional().
			MaxLen(512),

		field.JSON("variables", []TemplateVariable{}).
			Optional().
			Comment("Declared input variables for validation"),

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

// Edges of the Template.
func (Template) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("app", App.Type).
			Ref("templates").
			Field("app_id").
			Unique().
			Required().
			Immutable(),

		edge.To("contents", TemplateContent.Type),
	}
}

// Indexes of the Template.
func (Template) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("app_id", "slug").
			Unique().
			StorageKey("uidx_template_tenant_slug"),

		index.Fields("app_id", "slug"),
		index.Fields("app_id", "active"),
	}
}

type TemplateVariable struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // "string" | "number" | "boolean" | "date"
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
	Default     string `json:"default,omitempty"`
}

// TemplateContent holds the schema definition for the TemplateContent entity.
type TemplateContent struct {
	ent.Schema
}

// Fields of the TemplateContent.
func (TemplateContent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),

		field.UUID("template_id", uuid.UUID{}).
			Immutable(),

		field.Enum("channel").
			Values(
				"email", "sms", "push",
				"slack", "teams", "discord",
				"whatsapp", "webhook",
			),

		field.String("subject").
			Optional().
			Nillable().
			MaxLen(998).
			Comment("Email subject; nil for non-email channels"),

		field.Text("body_text").
			Comment("Plain text body; Go template syntax"),

		field.Text("body_html").
			Optional().
			Nillable().
			Comment("HTML body for email channel only"),

		// Channel-specific extras stored as JSON.
		// Email:   {"from_name":"Notifex","reply_to":"support@...","headers":{}}
		// Push:    {"title":"{{.title}}","icon":"","click_action":"FLUTTER_NOTIFICATION_CLICK"}
		// Slack:   {"blocks":[...Block Kit JSON...]}
		// Teams:   {"card":{...Adaptive Card JSON...}}
		// Discord: {"embeds":[...]}
		// Webhook: {"http_method":"POST","headers":{"X-Event":"{{.event_type}}"}}
		field.JSON("extras", map[string]interface{}{}).
			Optional().
			Comment("Channel-specific payload extras"),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),

		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the TemplateContent.
func (TemplateContent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("template", Template.Type).
			Ref("contents").
			Field("template_id").
			Unique().
			Required().
			Immutable(),
	}
}

// Edges of the TemplateContent.
func (TemplateContent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("template_id", "channel").
			Unique().
			StorageKey("uidx_tmpl_content_channel"),
	}
}
