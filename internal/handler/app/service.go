package app

import (
	"context"
	"log"
	"notifex/ent"
)

type AppService struct {
	client *ent.Client
}

func NewAppService(client *ent.Client) *AppService {
	return &AppService{client: client}
}

func (h *AppService) CreateApp(appData *AppRequest) *ent.App {
	ctx := context.Background()
	app, err := h.client.App.Create().SetName(appData.Name).SetSlug(appData.Slug).Save(ctx)
	if err != nil {
		log.Println(err)
		return nil
	}
	return app
}

func (h *AppService) GetApps() []*ent.App {
	ctx := context.Background()
	u, err := h.client.App.Query().All(ctx)
	if err != nil {
		log.Println(err)
		return nil
	}
	return u
}
