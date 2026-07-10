package notification

import (
	"context"
	"fmt"
	"notifex/internal/ent/schema"
	"notifex/internal/ent/schema/notification"

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

func (s *Service) Status(ctx context.Context, id string) (*schema.Notification, error) {
	data := s.db.Notification.Query().Where(notification.ID(uuid.MustParse(id))).OnlyX(ctx)
	return data, nil
}

func (s *Service) List(ctx context.Context) ([]*schema.Notification, error) {
	data := s.db.Notification.Query().AllX(ctx)
	return data, nil
}

func (s *Service) Send(ctx context.Context, req SendRequest) (*SendResponse, error) {
	fmt.Print(req)
	return &SendResponse{
		RequestID: "example-request-id",
	}, nil
}
