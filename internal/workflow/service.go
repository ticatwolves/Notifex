// Package workflow manages workflows, their channel actions and templates
// (docs/03, ADR-012), and renders template previews.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/rbac"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
	"github.com/ticatwolves/notifex/internal/workflow/render"
)

// Limits.
const (
	MaxLocalesPerAction = 50
	MaxWorkflowsPerApp  = 500
)

// Channels that can have actions in Phase 2 (push, whatsapp, outlook: Phase 4).
var Channels = map[string]bool{"email": true, "sms": true, "slack": true, "webhook": true}

// RuleError is a request that conflicts with a workflow invariant (409).
type RuleError struct{ Msg string }

func (e *RuleError) Error() string { return "workflow: " + e.Msg }

// Is matches store.ErrConflict.
func (e *RuleError) Is(target error) bool { return target == store.ErrConflict }

func fieldErr(field, format string, args ...any) error {
	return &tenant.FieldError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// BrandSource gives the effective brand for previews.
type BrandSource interface {
	EffectiveBrand(ctx context.Context, tid store.TenantID, appID uuid.UUID) (tenant.BrandView, error)
}

// Service implements workflow configuration.
type Service struct {
	st     store.Store
	brands BrandSource
	log    *slog.Logger
}

// NewService builds the service.
func NewService(st store.Store, brands BrandSource, log *slog.Logger) *Service {
	return &Service{st: st, brands: brands, log: log}
}

func require(a tenant.Actor, c rbac.Capability) error {
	if !rbac.Allow(a.Role, c) {
		return fmt.Errorf("%w: %s requires %s", tenant.ErrForbidden, a.Role, c)
	}
	return nil
}

func actorOf(a tenant.Actor) audit.Actor {
	return audit.Actor{Type: store.ActorUser, ID: a.ID.String()} // the user (Actor embeds User)
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}

// View is a workflow with its actions.
type View struct {
	store.Workflow
	Actions []store.Action
}

// ---- workflows

// List returns a page of an application's workflows with their actions.
func (s *Service) List(ctx context.Context, a tenant.Actor, appID uuid.UUID, status *store.WorkflowStatus, p store.Page) ([]View, error) {
	if err := require(a, rbac.ViewTenant); err != nil {
		return nil, err
	}
	if _, err := s.st.Applications().Get(ctx, a.TenantID, appID); err != nil {
		return nil, wrap("list workflows", err)
	}
	wfs, err := s.st.Workflows().List(ctx, a.TenantID, appID, status, p)
	if err != nil {
		return nil, wrap("list workflows", err)
	}
	ids := make([]uuid.UUID, len(wfs))
	for i, w := range wfs {
		ids[i] = w.ID
	}
	acts, err := s.st.Actions().ListForWorkflows(ctx, a.TenantID, ids)
	if err != nil {
		return nil, wrap("list workflows", err)
	}
	out := make([]View, len(wfs))
	for i, w := range wfs {
		out[i] = View{Workflow: w, Actions: nonNil(acts[w.ID])}
	}
	return out, nil
}

func nonNil(a []store.Action) []store.Action {
	if a == nil {
		return []store.Action{}
	}
	return a
}

// Get returns one workflow with its actions.
func (s *Service) Get(ctx context.Context, a tenant.Actor, appID, id uuid.UUID) (View, error) {
	if err := require(a, rbac.ViewTenant); err != nil {
		return View{}, err
	}
	return s.view(ctx, s.st, a.TenantID, appID, id)
}

func (s *Service) view(ctx context.Context, st store.Store, tid store.TenantID, appID, id uuid.UUID) (View, error) {
	w, err := st.Workflows().Get(ctx, tid, appID, id)
	if err != nil {
		return View{}, wrap("get workflow", err)
	}
	acts, err := st.Actions().ListForWorkflows(ctx, tid, []uuid.UUID{id})
	if err != nil {
		return View{}, wrap("get workflow", err)
	}
	return View{Workflow: w, Actions: nonNil(acts[id])}, nil
}

// CreateInput is a new workflow.
type CreateInput struct {
	Key           string
	Name          string
	Description   string
	PayloadSchema map[string]any
}

var keyRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?$`)

// Create creates a draft workflow.
func (s *Service) Create(ctx context.Context, a tenant.Actor, appID uuid.UUID, in CreateInput) (View, error) {
	if err := require(a, rbac.EditWorkflows); err != nil {
		return View{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	switch {
	case !keyRE.MatchString(in.Key):
		return View{}, fieldErr("key", "lowercase letters, digits, _ and -; 1-63 characters")
	case in.Name == "" || len([]rune(in.Name)) > 200:
		return View{}, fieldErr("name", "1-200 characters")
	case len([]rune(in.Description)) > 2000:
		return View{}, fieldErr("description", "at most 2000 characters")
	}
	if in.PayloadSchema != nil {
		if _, err := CompileSchema(in.PayloadSchema); err != nil {
			return View{}, err
		}
	}
	var out View
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		existing, err := tx.Workflows().List(ctx, a.TenantID, appID, nil, store.Page{Limit: store.MaxPageSize})
		if err != nil {
			return err
		}
		if len(existing) >= MaxWorkflowsPerApp { // cheap guard; exact counting isn't needed
			return fieldErr("key", "an application can have at most %d workflows", MaxWorkflowsPerApp)
		}
		w, err := tx.Workflows().Create(ctx, a.TenantID, store.NewWorkflow{
			ApplicationID: appID, Key: in.Key, Name: in.Name, Description: in.Description, PayloadSchema: in.PayloadSchema,
		})
		if err != nil {
			return err
		}
		out = View{Workflow: w, Actions: []store.Action{}}
		return audit.Record(ctx, tx, actorOf(a), audit.Event{
			TenantID: &a.TenantID, Action: "workflow.create", EntityType: "workflow", EntityID: w.ID.String(),
			After: map[string]any{"application_id": appID.String(), "key": w.Key, "name": w.Name, "has_payload_schema": w.PayloadSchema != nil},
		})
	})
	return out, wrap("create workflow", err)
}

// Patch is a workflow change. ClearSchema removes the payload schema.
type Patch struct {
	Name          *string
	Description   *string
	Status        *store.WorkflowStatus
	PayloadSchema map[string]any
	ClearSchema   bool
}

// Update changes a workflow. Activating requires at least one enabled
// action, and a template for every enabled action.
func (s *Service) Update(ctx context.Context, a tenant.Actor, appID, id uuid.UUID, p Patch) (View, error) {
	if err := require(a, rbac.EditWorkflows); err != nil {
		return View{}, err
	}
	if p.Name != nil {
		n := strings.TrimSpace(*p.Name)
		if n == "" || len([]rune(n)) > 200 {
			return View{}, fieldErr("name", "1-200 characters")
		}
		p.Name = &n
	}
	if p.Description != nil && len([]rune(*p.Description)) > 2000 {
		return View{}, fieldErr("description", "at most 2000 characters")
	}
	if st := p.Status; st != nil && *st != store.WorkflowDraft && *st != store.WorkflowActive && *st != store.WorkflowArchived {
		return View{}, fieldErr("status", "draft, active or archived")
	}
	if p.PayloadSchema != nil {
		if _, err := CompileSchema(p.PayloadSchema); err != nil {
			return View{}, err
		}
	}
	var out View
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Workflows().Lock(ctx, a.TenantID, appID, id); err != nil {
			return err
		}
		before, err := s.view(ctx, tx, a.TenantID, appID, id)
		if err != nil {
			return err
		}
		if p.Status != nil && *p.Status == store.WorkflowActive {
			if err := activatable(before.Actions); err != nil {
				return err
			}
		}
		w, err := tx.Workflows().Update(ctx, a.TenantID, appID, id, store.WorkflowPatch{
			Name: p.Name, Description: p.Description, Status: p.Status, PayloadSchema: p.PayloadSchema, ClearSchema: p.ClearSchema,
		})
		if err != nil {
			return err
		}
		out = View{Workflow: w, Actions: before.Actions}
		return audit.Record(ctx, tx, actorOf(a), audit.Event{
			TenantID: &a.TenantID, Action: "workflow.update", EntityType: "workflow", EntityID: id.String(),
			Before: wfAudit(before.Workflow), After: wfAudit(w),
		})
	})
	return out, wrap("update workflow", err)
}

func wfAudit(w store.Workflow) map[string]any {
	return map[string]any{"name": w.Name, "description_len": len(w.Description), "status": string(w.Status), "has_payload_schema": w.PayloadSchema != nil}
}

func activatable(acts []store.Action) error {
	enabled := 0
	for _, act := range acts {
		if !act.Enabled {
			continue
		}
		enabled++
		if len(act.Locales) == 0 {
			return fieldErr("status", "the enabled %s action has no template; add one before activating", act.Channel)
		}
	}
	if enabled == 0 {
		return fieldErr("status", "an active workflow needs at least one enabled action")
	}
	return nil
}

// Delete removes a workflow, its actions and templates. Accepted runs keep
// their snapshot.
func (s *Service) Delete(ctx context.Context, a tenant.Actor, appID, id uuid.UUID) error {
	if err := require(a, rbac.EditWorkflows); err != nil {
		return err
	}
	return wrap("delete workflow", s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		w, err := tx.Workflows().Get(ctx, a.TenantID, appID, id)
		if err != nil {
			return err
		}
		if err := tx.Workflows().Delete(ctx, a.TenantID, appID, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a), audit.Event{
			TenantID: &a.TenantID, Action: "workflow.delete", EntityType: "workflow", EntityID: id.String(),
			Before: map[string]any{"key": w.Key, "name": w.Name, "status": string(w.Status)},
		})
	}))
}

// ---- actions

// withWorkflow locks the workflow (verifying it belongs to the application)
// and runs fn with its current state, in one transaction.
func (s *Service) withWorkflow(ctx context.Context, a tenant.Actor, appID, wid uuid.UUID, fn func(ctx context.Context, tx store.Store, w View) error) error {
	return s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Workflows().Lock(ctx, a.TenantID, appID, wid); err != nil {
			return err
		}
		w, err := s.view(ctx, tx, a.TenantID, appID, wid)
		if err != nil {
			return err
		}
		return fn(ctx, tx, w)
	})
}

func findAction(w View, id uuid.UUID) (store.Action, error) {
	for _, act := range w.Actions {
		if act.ID == id {
			return act, nil
		}
	}
	return store.Action{}, store.ErrNotFound
}

// enabledOthers counts enabled actions other than id.
func enabledOthers(w View, id uuid.UUID) int {
	n := 0
	for _, act := range w.Actions {
		if act.Enabled && act.ID != id {
			n++
		}
	}
	return n
}

// CreateAction adds a channel to a workflow. On an active workflow a new
// action must start disabled (it has no template yet).
func (s *Service) CreateAction(ctx context.Context, a tenant.Actor, appID, wid uuid.UUID, channel string, enabled bool) (store.Action, error) {
	if err := require(a, rbac.EditWorkflows); err != nil {
		return store.Action{}, err
	}
	if !Channels[channel] {
		return store.Action{}, fieldErr("channel", "%q is not available yet; Phase 2 channels are email, sms, slack and webhook", truncate(channel, 20))
	}
	var out store.Action
	err := s.withWorkflow(ctx, a, appID, wid, func(ctx context.Context, tx store.Store, w View) error {
		if w.Status == store.WorkflowActive && enabled {
			return &RuleError{Msg: "the workflow is active: create the action disabled, add a template, then enable it"}
		}
		var err error
		if out, err = tx.Actions().Create(ctx, a.TenantID, wid, channel, enabled); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a), audit.Event{
			TenantID: &a.TenantID, Action: "action.create", EntityType: "action", EntityID: out.ID.String(),
			After: map[string]any{"workflow_id": wid.String(), "channel": channel, "enabled": enabled},
		})
	})
	return out, wrap("create action", err)
}

// SetActionEnabled enables or disables an action. On an active workflow,
// the last enabled action can't be disabled, and an action needs a template
// before it can be enabled.
func (s *Service) SetActionEnabled(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID, enabled bool) (store.Action, error) {
	if err := require(a, rbac.EditWorkflows); err != nil {
		return store.Action{}, err
	}
	var out store.Action
	err := s.withWorkflow(ctx, a, appID, wid, func(ctx context.Context, tx store.Store, w View) error {
		act, err := findAction(w, aid)
		if err != nil {
			return err
		}
		if w.Status == store.WorkflowActive {
			if !enabled && act.Enabled && enabledOthers(w, aid) == 0 {
				return &RuleError{Msg: "this is the last enabled action of an active workflow; archive the workflow instead"}
			}
			if enabled && len(act.Locales) == 0 {
				return &RuleError{Msg: "add a template before enabling an action of an active workflow"}
			}
		}
		if out, err = tx.Actions().SetEnabled(ctx, a.TenantID, wid, aid, enabled); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a), audit.Event{
			TenantID: &a.TenantID, Action: "action.update", EntityType: "action", EntityID: aid.String(),
			Before: map[string]any{"enabled": act.Enabled}, After: map[string]any{"enabled": enabled},
		})
	})
	return out, wrap("update action", err)
}

// DeleteAction removes an action and its templates (same rule as disabling).
func (s *Service) DeleteAction(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID) error {
	if err := require(a, rbac.EditWorkflows); err != nil {
		return err
	}
	return wrap("delete action", s.withWorkflow(ctx, a, appID, wid, func(ctx context.Context, tx store.Store, w View) error {
		act, err := findAction(w, aid)
		if err != nil {
			return err
		}
		if w.Status == store.WorkflowActive && act.Enabled && enabledOthers(w, aid) == 0 {
			return &RuleError{Msg: "this is the last enabled action of an active workflow; archive the workflow instead"}
		}
		if err := tx.Actions().Delete(ctx, a.TenantID, wid, aid); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a), audit.Event{
			TenantID: &a.TenantID, Action: "action.delete", EntityType: "action", EntityID: aid.String(),
			Before: map[string]any{"workflow_id": wid.String(), "channel": act.Channel, "enabled": act.Enabled, "locales": act.Locales},
		})
	}))
}

// ---- templates

// Content is a template's channel-specific content (ADR-012 §3).
type Content struct {
	Subject *string `json:"subject,omitempty"`
	HTML    *string `json:"html,omitempty"`
	Text    *string `json:"text,omitempty"`
	Layout  *string `json:"layout,omitempty"`
	Blocks  []any   `json:"blocks,omitempty"`
	Body    any     `json:"body,omitempty"`
}

// TemplateView is a stored template.
type TemplateView struct {
	store.Template
	Channel string
	Content Content
}

var localeRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2}|-[A-Za-z]{4}|-[0-9]{3})?$`)

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ValidateContent checks that only the channel's fields are set, then
// validates the template with the renderer (syntax and structure).
func ValidateContent(channel string, c Content) error {
	allowed := map[string][]string{
		"email": {"subject", "html", "text", "layout"}, "sms": {"text"},
		"slack": {"text", "blocks"}, "webhook": {"body"},
	}[channel]
	if allowed == nil {
		return fieldErr("channel", "unsupported channel %q", truncate(channel, 20))
	}
	set := map[string]bool{"subject": c.Subject != nil, "html": c.HTML != nil, "text": c.Text != nil,
		"layout": c.Layout != nil, "blocks": c.Blocks != nil, "body": c.Body != nil}
	ok := map[string]bool{}
	for _, f := range allowed {
		ok[f] = true
	}
	for _, f := range []string{"subject", "html", "text", "layout", "blocks", "body"} {
		if set[f] && !ok[f] {
			return fieldErr(f, "is not used by %s templates (use %s)", channel, strings.Join(allowed, ", "))
		}
	}
	switch channel {
	case "email":
		return render.ValidateEmail(render.EmailTemplate{Subject: str(c.Subject), HTML: str(c.HTML), Text: str(c.Text), Layout: str(c.Layout)})
	case "sms":
		return render.ValidateSMS(str(c.Text))
	case "slack":
		return render.ValidateSlack(render.SlackTemplate{Text: str(c.Text), Blocks: c.Blocks})
	default:
		return render.ValidateWebhook(c.Body)
	}
}

func contentOf(m map[string]any) (Content, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return Content{}, fmt.Errorf("decode template: %w", err)
	}
	var c Content
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil {
		return Content{}, fmt.Errorf("decode template: %w", err)
	}
	return c, nil
}

func contentMap(c Content) (map[string]any, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode template: %w", err)
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("encode template: %w", err)
	}
	return m, nil
}

func (s *Service) action(ctx context.Context, st store.Store, a tenant.Actor, appID, wid, aid uuid.UUID) (store.Action, error) {
	if _, err := st.Workflows().Get(ctx, a.TenantID, appID, wid); err != nil {
		return store.Action{}, err
	}
	return st.Actions().Get(ctx, a.TenantID, wid, aid)
}

func toView(act store.Action, t store.Template) (TemplateView, error) {
	c, err := contentOf(t.Content)
	return TemplateView{Template: t, Channel: act.Channel, Content: c}, err
}

// ListTemplates returns an action's templates.
func (s *Service) ListTemplates(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID) ([]TemplateView, error) {
	if err := require(a, rbac.ViewTenant); err != nil {
		return nil, err
	}
	act, err := s.action(ctx, s.st, a, appID, wid, aid)
	if err != nil {
		return nil, wrap("list templates", err)
	}
	ts, err := s.st.Templates().List(ctx, a.TenantID, aid)
	if err != nil {
		return nil, wrap("list templates", err)
	}
	out := make([]TemplateView, len(ts))
	for i, t := range ts {
		if out[i], err = toView(act, t); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetTemplate returns the template for a locale.
func (s *Service) GetTemplate(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID, locale string) (TemplateView, error) {
	if err := require(a, rbac.ViewTenant); err != nil {
		return TemplateView{}, err
	}
	act, err := s.action(ctx, s.st, a, appID, wid, aid)
	if err != nil {
		return TemplateView{}, wrap("get template", err)
	}
	t, err := s.st.Templates().Get(ctx, a.TenantID, aid, locale)
	if err != nil {
		return TemplateView{}, wrap("get template", err)
	}
	return toView(act, t)
}

// PutTemplate creates or replaces the template for a locale.
func (s *Service) PutTemplate(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID, locale string, c Content) (TemplateView, error) {
	if err := require(a, rbac.EditWorkflows); err != nil {
		return TemplateView{}, err
	}
	if !localeRE.MatchString(locale) {
		return TemplateView{}, fieldErr("locale", "must be a BCP 47 tag such as en, en-IN or hi")
	}
	var out TemplateView
	err := s.withWorkflow(ctx, a, appID, wid, func(ctx context.Context, tx store.Store, w View) error {
		act, err := findAction(w, aid)
		if err != nil {
			return err
		}
		if err := ValidateContent(act.Channel, c); err != nil {
			return err
		}
		isNew := !contains(act.Locales, locale)
		if isNew && len(act.Locales) >= MaxLocalesPerAction {
			return fieldErr("locale", "an action can have at most %d locales", MaxLocalesPerAction)
		}
		m, err := contentMap(c)
		if err != nil {
			return err
		}
		t, err := tx.Templates().Put(ctx, a.TenantID, aid, locale, m)
		if err != nil {
			return err
		}
		if out, err = toView(act, t); err != nil {
			return err
		}
		size, _ := json.Marshal(m)
		return audit.Record(ctx, tx, actorOf(a), audit.Event{
			TenantID: &a.TenantID, Action: "template.put", EntityType: "action", EntityID: aid.String(),
			After: map[string]any{"locale": locale, "channel": act.Channel, "created": isNew, "bytes": len(size)},
		})
	})
	return out, wrap("put template", err)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// DeleteTemplate removes the template for a locale. The last template of an
// enabled action in an active workflow can't be removed.
func (s *Service) DeleteTemplate(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID, locale string) error {
	if err := require(a, rbac.EditWorkflows); err != nil {
		return err
	}
	return wrap("delete template", s.withWorkflow(ctx, a, appID, wid, func(ctx context.Context, tx store.Store, w View) error {
		act, err := findAction(w, aid)
		if err != nil {
			return err
		}
		if !contains(act.Locales, locale) {
			return store.ErrNotFound
		}
		if w.Status == store.WorkflowActive && act.Enabled && len(act.Locales) == 1 {
			return &RuleError{Msg: "this is the last template of an enabled action in an active workflow"}
		}
		if err := tx.Templates().Delete(ctx, a.TenantID, aid, locale); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a), audit.Event{
			TenantID: &a.TenantID, Action: "template.delete", EntityType: "action", EntityID: aid.String(),
			Before: map[string]any{"locale": locale, "channel": act.Channel},
		})
	}))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
