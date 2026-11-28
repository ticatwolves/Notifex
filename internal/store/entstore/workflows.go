package entstore

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/action"
	"github.com/ticatwolves/notifex/ent/template"
	"github.com/ticatwolves/notifex/ent/workflow"
	"github.com/ticatwolves/notifex/internal/store"
)

// workflowKeyRE is the trigger-facing key: lowercase, digits, _ and -.
var workflowKeyRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?$`)

// ---- workflows

type workflows struct{ c *ent.Client }

func toWorkflow(r *ent.Workflow) store.Workflow {
	return store.Workflow{
		ID: r.ID, TenantID: store.TenantID(r.TenantID), ApplicationID: r.ApplicationID, Key: r.Key,
		Name: r.Name, Description: r.Description, Status: store.WorkflowStatus(r.Status),
		PayloadSchema: r.PayloadSchema, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (w workflows) Create(ctx context.Context, tid store.TenantID, in store.NewWorkflow) (store.Workflow, error) {
	if !workflowKeyRE.MatchString(in.Key) {
		return store.Workflow{}, invalid("workflow key %q", in.Key)
	}
	c := w.c.Workflow.Create().SetTenantID(tid.UUID()).SetApplicationID(in.ApplicationID).
		SetKey(in.Key).SetName(in.Name).SetDescription(in.Description)
	if in.PayloadSchema != nil {
		c.SetPayloadSchema(in.PayloadSchema)
	}
	row, err := c.Save(scoped(ctx, tid))
	if err != nil {
		return store.Workflow{}, fmt.Errorf("workflows.create: %w", mapErr(err, opCreate))
	}
	return toWorkflow(row), nil
}

func (w workflows) Get(ctx context.Context, tid store.TenantID, appID, id uuid.UUID) (store.Workflow, error) {
	row, err := w.c.Workflow.Query().
		Where(workflow.TenantID(tid.UUID()), workflow.ApplicationID(appID), workflow.ID(id)).
		Only(scoped(ctx, tid))
	if err != nil {
		return store.Workflow{}, fmt.Errorf("workflows.get: %w", mapErr(err, opRead))
	}
	return toWorkflow(row), nil
}

func (w workflows) List(ctx context.Context, tid store.TenantID, appID uuid.UUID, status *store.WorkflowStatus, p store.Page) ([]store.Workflow, error) {
	p = p.Normalize()
	q := w.c.Workflow.Query().Where(workflow.TenantID(tid.UUID()), workflow.ApplicationID(appID))
	if status != nil {
		q.Where(workflow.StatusEQ(workflow.Status(*status)))
	}
	if p.After != uuid.Nil {
		q.Where(workflow.IDGT(p.After))
	}
	rows, err := q.Order(ent.Asc(workflow.FieldID)).Limit(p.Limit).All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("workflows.list: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toWorkflow), nil
}

func (w workflows) Update(ctx context.Context, tid store.TenantID, appID, id uuid.UUID, p store.WorkflowPatch) (store.Workflow, error) {
	u := w.c.Workflow.UpdateOneID(id).Where(workflow.TenantID(tid.UUID()), workflow.ApplicationID(appID))
	if p.Name != nil {
		u.SetName(*p.Name)
	}
	if p.Description != nil {
		u.SetDescription(*p.Description)
	}
	if p.Status != nil {
		u.SetStatus(workflow.Status(*p.Status))
	}
	switch {
	case p.ClearSchema:
		u.ClearPayloadSchema()
	case p.PayloadSchema != nil:
		u.SetPayloadSchema(p.PayloadSchema)
	}
	row, err := u.Save(scoped(ctx, tid))
	if err != nil {
		return store.Workflow{}, fmt.Errorf("workflows.update: %w", mapErr(err, opWrite))
	}
	return toWorkflow(row), nil
}

func (w workflows) Delete(ctx context.Context, tid store.TenantID, appID, id uuid.UUID) error {
	n, err := w.c.Workflow.Delete().
		Where(workflow.TenantID(tid.UUID()), workflow.ApplicationID(appID), workflow.ID(id)).
		Exec(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("workflows.delete: %w", mapErr(err, opDelete))
	}
	if n == 0 {
		return fmt.Errorf("workflows.delete: %w", store.ErrNotFound)
	}
	return nil
}

// Lock uses explicit tenant_id in raw SQL (no ent interceptor runs).
func (w workflows) Lock(ctx context.Context, tid store.TenantID, appID, id uuid.UUID) error {
	rows, err := w.c.QueryContext(ctx,
		`SELECT id FROM workflows WHERE tenant_id = $1 AND application_id = $2 AND id = $3 FOR UPDATE`,
		tid.UUID(), appID, id)
	if err != nil {
		return fmt.Errorf("workflows.lock: %w", mapErr(err, opRead))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("workflows.lock: %w", mapErr(err, opRead))
		}
		return fmt.Errorf("workflows.lock: %w", store.ErrNotFound)
	}
	return nil
}

// ---- actions

type actions struct{ c *ent.Client }

func toAction(r *ent.Action, locales []string) store.Action {
	if locales == nil {
		locales = []string{}
	}
	return store.Action{
		ID: r.ID, WorkflowID: r.WorkflowID, Channel: string(r.Channel), Enabled: r.Enabled,
		Locales: locales, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (a actions) Create(ctx context.Context, tid store.TenantID, workflowID uuid.UUID, channel string, enabled bool) (store.Action, error) {
	if err := action.ChannelValidator(action.Channel(channel)); err != nil {
		return store.Action{}, invalid("channel %q", channel)
	}
	row, err := a.c.Action.Create().SetTenantID(tid.UUID()).SetWorkflowID(workflowID).
		SetChannel(action.Channel(channel)).SetEnabled(enabled).Save(scoped(ctx, tid))
	if err != nil {
		return store.Action{}, fmt.Errorf("actions.create: %w", mapErr(err, opCreate))
	}
	return toAction(row, nil), nil
}

func (a actions) locales(ctx context.Context, tid store.TenantID, actionIDs []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := map[uuid.UUID][]string{}
	if len(actionIDs) == 0 {
		return out, nil
	}
	rows, err := a.c.Template.Query().
		Where(template.TenantID(tid.UUID()), template.ActionIDIn(actionIDs...)).
		Select(template.FieldActionID, template.FieldLocale).All(scoped(ctx, tid))
	if err != nil {
		return nil, mapErr(err, opRead)
	}
	for _, r := range rows {
		out[r.ActionID] = append(out[r.ActionID], r.Locale)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out, nil
}

func (a actions) Get(ctx context.Context, tid store.TenantID, workflowID, id uuid.UUID) (store.Action, error) {
	row, err := a.c.Action.Query().
		Where(action.TenantID(tid.UUID()), action.WorkflowID(workflowID), action.ID(id)).
		Only(scoped(ctx, tid))
	if err != nil {
		return store.Action{}, fmt.Errorf("actions.get: %w", mapErr(err, opRead))
	}
	locs, err := a.locales(ctx, tid, []uuid.UUID{row.ID})
	if err != nil {
		return store.Action{}, fmt.Errorf("actions.get: %w", err)
	}
	return toAction(row, locs[row.ID]), nil
}

func (a actions) ListForWorkflows(ctx context.Context, tid store.TenantID, workflowIDs []uuid.UUID) (map[uuid.UUID][]store.Action, error) {
	out := map[uuid.UUID][]store.Action{}
	if len(workflowIDs) == 0 {
		return out, nil
	}
	rows, err := a.c.Action.Query().
		Where(action.TenantID(tid.UUID()), action.WorkflowIDIn(workflowIDs...)).
		Order(ent.Asc(action.FieldID)).All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("actions.list: %w", mapErr(err, opRead))
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	locs, err := a.locales(ctx, tid, ids)
	if err != nil {
		return nil, fmt.Errorf("actions.list: %w", err)
	}
	for _, r := range rows {
		out[r.WorkflowID] = append(out[r.WorkflowID], toAction(r, locs[r.ID]))
	}
	return out, nil
}

func (a actions) SetEnabled(ctx context.Context, tid store.TenantID, workflowID, id uuid.UUID, enabled bool) (store.Action, error) {
	n, err := a.c.Action.Update().
		Where(action.TenantID(tid.UUID()), action.WorkflowID(workflowID), action.ID(id)).
		SetEnabled(enabled).Save(scoped(ctx, tid))
	if err != nil {
		return store.Action{}, fmt.Errorf("actions.set_enabled: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		return store.Action{}, fmt.Errorf("actions.set_enabled: %w", store.ErrNotFound)
	}
	return a.Get(ctx, tid, workflowID, id)
}

func (a actions) Delete(ctx context.Context, tid store.TenantID, workflowID, id uuid.UUID) error {
	n, err := a.c.Action.Delete().
		Where(action.TenantID(tid.UUID()), action.WorkflowID(workflowID), action.ID(id)).
		Exec(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("actions.delete: %w", mapErr(err, opDelete))
	}
	if n == 0 {
		return fmt.Errorf("actions.delete: %w", store.ErrNotFound)
	}
	return nil
}

// ---- templates

type templates struct{ c *ent.Client }

func toTemplate(r *ent.Template) store.Template {
	c := r.Content
	if c == nil {
		c = map[string]any{}
	}
	return store.Template{ActionID: r.ActionID, Locale: r.Locale, Content: c, UpdatedAt: r.UpdatedAt}
}

func (t templates) Get(ctx context.Context, tid store.TenantID, actionID uuid.UUID, locale string) (store.Template, error) {
	row, err := t.c.Template.Query().
		Where(template.TenantID(tid.UUID()), template.ActionID(actionID), template.Locale(locale)).
		Only(scoped(ctx, tid))
	if err != nil {
		return store.Template{}, fmt.Errorf("templates.get: %w", mapErr(err, opRead))
	}
	return toTemplate(row), nil
}

func (t templates) List(ctx context.Context, tid store.TenantID, actionID uuid.UUID) ([]store.Template, error) {
	rows, err := t.c.Template.Query().
		Where(template.TenantID(tid.UUID()), template.ActionID(actionID)).
		Order(ent.Asc(template.FieldLocale)).All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("templates.list: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toTemplate), nil
}

func (t templates) Put(ctx context.Context, tid store.TenantID, actionID uuid.UUID, locale string, content map[string]any) (store.Template, error) {
	if content == nil {
		content = map[string]any{}
	}
	sctx := scoped(ctx, tid)
	err := upsert(ctx,
		func() (int, error) {
			n, err := t.c.Template.Update().
				Where(template.TenantID(tid.UUID()), template.ActionID(actionID), template.Locale(locale)).
				SetContent(content).Save(sctx)
			return n, mapErr(err, opWrite)
		},
		func() error {
			_, err := t.c.Template.Create().SetTenantID(tid.UUID()).SetActionID(actionID).
				SetLocale(locale).SetContent(content).Save(sctx)
			return mapErr(err, opCreate)
		})
	if err != nil {
		return store.Template{}, fmt.Errorf("templates.put: %w", err)
	}
	return t.Get(ctx, tid, actionID, locale)
}

func (t templates) Delete(ctx context.Context, tid store.TenantID, actionID uuid.UUID, locale string) error {
	n, err := t.c.Template.Delete().
		Where(template.TenantID(tid.UUID()), template.ActionID(actionID), template.Locale(locale)).
		Exec(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("templates.delete: %w", mapErr(err, opDelete))
	}
	if n == 0 {
		return fmt.Errorf("templates.delete: %w", store.ErrNotFound)
	}
	return nil
}

func (t templates) Count(ctx context.Context, tid store.TenantID, actionID uuid.UUID) (int, error) {
	n, err := t.c.Template.Query().
		Where(template.TenantID(tid.UUID()), template.ActionID(actionID)).Count(scoped(ctx, tid))
	if err != nil {
		return 0, fmt.Errorf("templates.count: %w", mapErr(err, opRead))
	}
	return n, nil
}
