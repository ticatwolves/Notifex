package notificationdelivery

import (
	"context"
	"notifex/internal/ent/schema"

	"github.com/google/uuid"
)

type Service struct {
	db *schema.Client
}

func New(db *schema.Client) *Service {
	return &Service{
		db: db,
	}
}

func (s *Service) Create(ctx context.Context, req NotificationDeliveryRequest) (*NotificationDeliveryResponse, error) {
	return &NotificationDeliveryResponse{
		ID:     uuid.New(),
		AppID:  req.AppID,
		Active: true,
	}, nil
}

func (s *Service) Get(ctx context.Context, id string) (*NotificationDeliveryResponse, error) {
	return &NotificationDeliveryResponse{
		ID:     uuid.New(),
		AppID:  uuid.New(),
		Active: true,
	}, nil
}

func (s *Service) List(ctx context.Context) ([]NotificationDeliveryResponse, error) {
	return []NotificationDeliveryResponse{
		{
			ID:     uuid.New(),
			AppID:  uuid.New(),
			Active: true,
		},
	}, nil
}

func (s *Service) Update(ctx context.Context, id string, req NotificationDeliveryRequest) (*NotificationDeliveryResponse, error) {
	return &NotificationDeliveryResponse{
		ID:     uuid.New(),
		AppID:  req.AppID,
		Active: req.Active,
	}, nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
	return nil
}

func (s *Service) ValidateAPIKey(ctx context.Context, apiKey string) (bool, error) {
	// Implementation for validating API key
	return true, nil
}
