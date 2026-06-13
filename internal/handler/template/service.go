package template

import (
	"context"
	"fmt"
	"notifex/ent"
	temp "notifex/ent/template"

	"github.com/google/uuid"
)

type TemplateService struct {
	client *ent.Client
}

func NewTemplateService(client *ent.Client) *TemplateService {
	return &TemplateService{client: client}
}

func (h *TemplateService) CreateTemplate(templateData *TemplateRequest) *ent.Template {
	fmt.Println("Creating template:", templateData)
	ctx := context.Background()
	template, err := h.client.Template.
		Create().
		SetAppID(templateData.AppID).
		SetSlug(templateData.Slug).
		SetName(templateData.Name).
		SetVariables(templateData.Variables).
		SetActive(templateData.Active).
		Save(ctx)
	if err != nil {
		panic(err)
	}
	return template
}

func (h *TemplateService) GetTemplatesByAppID(appID *uuid.UUID) []*ent.Template {
	ctx := context.Background()
	templates, err := h.client.Template.Query().Where(temp.AppID(*appID)).All(ctx)
	if err != nil {
		panic(err)
	}
	return templates
}

func (h *TemplateService) GetTemplateByID(id uuid.UUID) *ent.Template {
	ctx := context.Background()
	template, err := h.client.Template.Query().Where(temp.ID(id)).Only(ctx)
	if err != nil {
		return nil
	}
	return template
}

func (h *TemplateService) UpdateTemplateByID(id uuid.UUID) (*ent.Template, error) {
	ctx := context.Background()
	template, err := h.client.Template.Query().Where(temp.ID(id)).Only(ctx)
	if err != nil {
		return nil, err
	}
	return template, nil
}

func (h *TemplateService) DeleteTemplateByID(id uuid.UUID) {
	ctx := context.Background()
	err := h.client.APIKey.DeleteOneID(id).Exec(ctx)
	if err != nil {
		println(err.Error())
		return
	}
}
