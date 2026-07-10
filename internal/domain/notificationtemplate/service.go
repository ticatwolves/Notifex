package notificationtemplate

import (
	"context"
	"notifex/internal/ent/schema"
	"notifex/internal/ent/schema/notificationtemplate"

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

func (s *Service) Create(ctx context.Context, req TemplateRequest) (*schema.NotificationTemplate, error) {
	data := s.db.NotificationTemplate.Create().SetAppID(req.AppID).
		SetSlug(req.Slug).
		SetName(req.Name).
		SetDescription(req.Description).
		SetEnabled(req.Enabled).
		SaveX(ctx)
	return data, nil
}

func (s *Service) Get(ctx context.Context, id string) (*schema.NotificationTemplate, error) {
	data := s.db.NotificationTemplate.Query().Where(notificationtemplate.ID(uuid.MustParse(id))).OnlyX(ctx)
	return data, nil
}

func (s *Service) List(ctx context.Context) ([]*schema.NotificationTemplate, error) {
	data := s.db.NotificationTemplate.Query().AllX(ctx)
	return data, nil
}

func (s *Service) Update(ctx context.Context, id string, req TemplateRequest) (*schema.NotificationTemplate, error) {
	data := s.db.NotificationTemplate.Query().Where(notificationtemplate.ID(uuid.MustParse(id))).OnlyX(ctx)
	data, _ = s.db.NotificationTemplate.UpdateOne(data).SetSlug(req.Slug).SetName(req.Name).SetDescription(req.Description).SetEnabled(req.Enabled).Save(ctx)
	return data, nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
	s.db.NotificationTemplate.Delete().Where(notificationtemplate.ID(uuid.MustParse(id))).ExecX(ctx)
	return nil
}
