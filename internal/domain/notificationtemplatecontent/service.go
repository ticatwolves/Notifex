package notificationtemplatecontent

import (
	"context"
	"notifex/internal/ent/schema"
	"notifex/internal/ent/schema/notificationtemplatecontent"

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

func (s *Service) Create(ctx context.Context, req TemplateContentRequest) (*schema.NotificationTemplateContent, error) {
	data := s.db.NotificationTemplateContent.Create().
		SetTemplateID(req.NotificationTemplateID).
		SetChannel(notificationtemplatecontent.Channel(req.Channel)).
		SetSubject(req.Subject).
		SetContent(req.Content).
		SetVariables(req.Variables).
		SetDefaultContext(req.DefaultContext).
		SetEnabled(req.Enabled).
		SaveX(ctx)
	return data, nil
}

func (s *Service) Get(ctx context.Context, id string) (*schema.NotificationTemplateContent, error) {
	data := s.db.NotificationTemplateContent.Query().Where(notificationtemplatecontent.ID(uuid.MustParse(id))).OnlyX(ctx)
	return data, nil
}

func (s *Service) List(ctx context.Context, templateId uuid.UUID) ([]*schema.NotificationTemplateContent, error) {
	data := s.db.NotificationTemplateContent.Query().Where(notificationtemplatecontent.TemplateID(templateId)).AllX(ctx)
	return data, nil
}

func (s *Service) Update(ctx context.Context, id string, req TemplateContentRequest) (*schema.NotificationTemplateContent, error) {
	data := s.db.NotificationTemplateContent.Query().Where(notificationtemplatecontent.ID(uuid.MustParse(id))).OnlyX(ctx)
	data, _ = s.db.NotificationTemplateContent.UpdateOne(data).SetChannel(notificationtemplatecontent.Channel(req.Channel)).SetSubject(req.Subject).SetContent(req.Content).SetVariables(req.Variables).SetDefaultContext(req.DefaultContext).SetEnabled(req.Enabled).Save(ctx)
	return data, nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
	s.db.NotificationTemplateContent.Delete().Where(notificationtemplatecontent.ID(uuid.MustParse(id))).ExecX(ctx)
	return nil
}
