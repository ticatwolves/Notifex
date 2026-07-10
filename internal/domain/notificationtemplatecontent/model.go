package notificationtemplatecontent

import (
	"encoding/json"

	"github.com/google/uuid"
)

type TemplateContentRequest struct {
	NotificationTemplateID uuid.UUID       `json:"template_id"`
	Channel                string          `json:"channel"`
	Subject                string          `json:"subject"`
	Content                string          `json:"content"`
	Variables              []string        `json:"variables"`
	DefaultContext         json.RawMessage `json:"default_context"`
	Enabled                bool            `json:"enabled"`
}
