package notificationdelivery

import (
	"time"

	"github.com/google/uuid"
)

type NotificationDeliveryRequest struct {
	AppID       uuid.UUID `json:"app_id"`
	Name        string    `json:"name"`
	Environment string    `json:"env"`
	KeyPrefix   string    `json:"key_prefix"`
	Scopes      []string  `json:"scopes"`
	ExpiresAt   time.Time `json:"expires_at" binding:"required"`
	Active      bool      `json:"active"`
}

type NotificationDeliveryQueryParams struct {
	AppID       *uuid.UUID `json:"app_id"`
	Environment *string    `json:"env"`
	Active      *bool      `json:"active"`
}

type NotificationDeliveryResponse struct {
	ID          uuid.UUID `json:"id"`
	AppID       uuid.UUID `json:"app_id"`
	Name        string    `json:"name"`
	Environment string    `json:"env"`
	KeyPrefix   string    `json:"key_prefix"`
	Scopes      []string  `json:"scopes"`
	ExpiresAt   time.Time `json:"expires_at"`
	Active      bool      `json:"active"`
}
