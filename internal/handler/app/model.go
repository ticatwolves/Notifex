package app

import (
	"time"

	"github.com/google/uuid"
)

type AppRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

type AppResponse struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	Plan         string    `json:"plan"`
	MonthlyQuota int
	QuotaResetAt time.Time
	Active       bool `json:"active"`
}
