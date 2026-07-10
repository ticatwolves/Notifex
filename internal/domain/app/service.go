package app

import (
	"context"
	"notifex/internal/ent/schema"
	"notifex/internal/ent/schema/app"

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

func (s *Service) Create(ctx context.Context, req AppCreateRequest) (*AppResponse, error) {
	s.db.App.Create().SetName(req.Name).
		SetSlug(req.Slug).
		SetDescription(req.Description).
		SaveX(ctx)
	return &AppResponse{
		ID:   uuid.New(),
		Name: req.Name,
		Slug: req.Slug,
	}, nil
}

func (s *Service) Get(ctx context.Context, slug string) (*schema.App, error) {
	data, err := s.db.App.Query().Where(app.Slug(slug)).Only(ctx)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Service) List(ctx context.Context) ([]*schema.App, error) {
	data, err := s.db.App.Query().All(ctx)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Service) Update(ctx context.Context, slug string, req AppUpdateRequest) (*schema.App, error) {
	data, err := s.db.App.Query().Where(app.Slug(slug)).Only(ctx)
	data, err = s.db.App.UpdateOne(data).SetActive(req.Active).SetName(req.Name).SetDescription(req.Description).Save(ctx)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Service) Delete(ctx context.Context, slug string) error {
	_, err := s.db.App.Delete().Where(app.Slug(slug)).Exec(ctx)
	if err != nil {
		return err
	}
	return nil
}
