package apikey

import (
	"context"
	"notifex/internal/ent/schema"
	"notifex/internal/ent/schema/apikey"
	"time"

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

func (s *Service) Create(ctx context.Context, req APIKeyRequest) (*APIKeyResponse, error) {
	data := s.db.APIKey.Create().SetName(req.Name).
		SetName(req.Name).
		SetAppID(req.AppID).
		SetKeyHash("io").
		SetExpiresAt(time.Now()).
		SetActive(req.Active).
		SaveX(ctx)
	return &APIKeyResponse{
		ID:     data.ID,
		Name:   data.Name,
		AppID:  data.AppID,
		Active: data.Active,
	}, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*schema.APIKey, error) {
	data := s.db.APIKey.Query().Where(apikey.ID(id)).OnlyX(ctx)
	return data, nil
}

func (s *Service) List(ctx context.Context, app_id uuid.UUID) ([]*schema.APIKey, error) {
	data := s.db.APIKey.Query().Where(apikey.AppID(app_id)).AllX(ctx)
	return data, nil
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, req APIKeyRequest) (*schema.APIKey, error) {
	data := s.db.APIKey.Query().Where(apikey.ID(id)).OnlyX(ctx)
	data, _ = s.db.APIKey.UpdateOne(data).SetActive(req.Active).SetName(req.Name).Save(ctx)
	return data, nil
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.APIKey.Delete().Where(apikey.ID(id)).Exec(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) ValidateAPIKey(ctx context.Context, apiKey string) (bool, error) {
	// Implementation for validating API key
	return true, nil
}
