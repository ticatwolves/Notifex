package template

import (
	"notifex/ent/schema"

	"github.com/google/uuid"
)

type TemplateRequest struct {
	AppID       uuid.UUID                 `json:"app_id"`
	Slug        string                    `json:"slug"`
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Variables   []schema.TemplateVariable `json:"variables"`
	Active      bool                      `json:"active"`
}
