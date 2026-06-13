package apikey

import (
	"time"

	"github.com/google/uuid"
)

type APIKeyRequest struct {
	AppID       uuid.UUID `json:"app_id"`
	Name        string    `json:"name"`
	Environment string    `json:"env"`
	KeyPrefix   string    `json:"key_prefix"`
	Scopes      []string  `json:"scopes"`
	ExpiresAt   time.Time `json:"expires_at" binding:"required"`
	Active      bool      `json:"active"`
}

type APIKeyQueryParams struct {
	AppID       *uuid.UUID `json:"app_id"`
	Environment *string    `json:"env"`
	Active      *bool      `json:"active"`
}
