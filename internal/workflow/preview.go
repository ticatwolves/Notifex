package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/rbac"
	"github.com/ticatwolves/notifex/internal/tenant"
	"github.com/ticatwolves/notifex/internal/workflow/render"
)

// previewTimeout bounds one preview render (the step budget bounds it too).
const previewTimeout = 2 * time.Second

// PreviewInput is an unsaved template with sample data.
type PreviewInput struct {
	Channel    string
	Template   Content
	Payload    map[string]any
	Recipient  map[string]any
	WorkflowID *uuid.UUID // optional: workflow name/key in the context, schema warnings
}

// Warning is advisory: the preview still rendered.
type Warning struct {
	Field   string
	Message string
}

// PreviewResult is the rendered output for one channel.
type PreviewResult struct {
	Channel  string
	Subject  *string
	HTML     *string
	Text     *string
	Blocks   []any
	Body     json.RawMessage
	SMS      *render.SMSInfo
	Warnings []Warning
}

func toMap(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("preview: %w", err)
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("preview: %w", err)
	}
	return m, nil
}

// Preview renders a template as a send would, with the application's
// effective brand. Nothing is stored or logged.
func (s *Service) Preview(ctx context.Context, a tenant.Actor, appID uuid.UUID, in PreviewInput) (PreviewResult, error) {
	if err := require(a, rbac.EditWorkflows); err != nil {
		return PreviewResult{}, err
	}
	if !Channels[in.Channel] {
		return PreviewResult{}, fieldErr("channel", "%q is not available yet", truncate(in.Channel, 20))
	}
	if _, err := s.st.Applications().Get(ctx, a.TenantID, appID); err != nil {
		return PreviewResult{}, wrap("preview", err)
	}
	if err := ValidateContent(in.Channel, in.Template); err != nil {
		return PreviewResult{}, err
	}
	res := PreviewResult{Channel: in.Channel, Warnings: []Warning{}}

	brand, err := s.brands.EffectiveBrand(ctx, a.TenantID, appID)
	if err != nil {
		return PreviewResult{}, wrap("preview", err)
	}
	brandMap, err := toMap(brand.Brand)
	if err != nil {
		return PreviewResult{}, err
	}
	input := render.Input{Payload: in.Payload, Recipient: in.Recipient, Brand: brandMap}

	if in.WorkflowID != nil {
		w, err := s.st.Workflows().Get(ctx, a.TenantID, appID, *in.WorkflowID)
		if err != nil {
			return PreviewResult{}, wrap("preview", err)
		}
		input.Workflow = map[string]any{"key": w.Key, "name": w.Name}
		if w.PayloadSchema != nil {
			sch, err := CompileSchema(w.PayloadSchema)
			if err != nil {
				return PreviewResult{}, fmt.Errorf("preview: stored payload schema: %w", err)
			}
			payload := in.Payload
			if payload == nil {
				payload = map[string]any{}
			}
			if err := sch.Validate(payload); err != nil {
				res.Warnings = append(res.Warnings, Warning{Field: "payload", Message: "sample payload doesn't match the payload schema: " + truncate(err.Error(), 300)})
			}
			res.Warnings = append(res.Warnings, schemaWarnings(in.Template, sch)...)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, previewTimeout)
	defer cancel()
	t := in.Template
	switch in.Channel {
	case "email":
		out, err := render.Email(ctx, render.EmailTemplate{Subject: str(t.Subject), HTML: str(t.HTML), Text: str(t.Text), Layout: str(t.Layout)}, input)
		if err != nil {
			return PreviewResult{}, err
		}
		res.Subject, res.Text = &out.Subject, &out.Text
		if out.HTML != "" {
			res.HTML = &out.HTML
		}
	case "sms":
		out, err := render.SMS(ctx, str(t.Text), input)
		if err != nil {
			return PreviewResult{}, err
		}
		res.Text, res.SMS = &out.Text, &out.SMSInfo
	case "slack":
		out, err := render.Slack(ctx, render.SlackTemplate{Text: str(t.Text), Blocks: t.Blocks}, input)
		if err != nil {
			return PreviewResult{}, err
		}
		if out.Text != "" {
			res.Text = &out.Text
		}
		res.Blocks = out.Blocks
	case "webhook":
		out, err := render.Webhook(ctx, t.Body, input)
		if err != nil {
			return PreviewResult{}, err
		}
		res.Body = out
	}
	return res, nil
}

// schemaWarnings lists payload paths the templates use that the schema
// doesn't define. Paths relative to an #each/#with scope are not checked.
func schemaWarnings(c Content, sch *PayloadSchema) []Warning {
	var out []Warning
	seen := map[string]bool{}
	check := func(field, src string) {
		tpl, err := render.Parse(src)
		if err != nil {
			return // reported by validation
		}
		for _, p := range tpl.Paths() {
			p = strings.TrimPrefix(p, "@root.")
			rest, ok := strings.CutPrefix(p, "payload.")
			if !ok || seen[p] {
				continue
			}
			seen[p] = true
			if !sch.Defines(strings.Split(rest, ".")) {
				out = append(out, Warning{Field: field, Message: fmt.Sprintf("%s is not defined in the workflow's payload schema", p)})
			}
		}
	}
	for _, f := range []struct {
		name string
		src  *string
	}{{"subject", c.Subject}, {"html", c.HTML}, {"text", c.Text}} {
		if f.src != nil {
			check(f.name, *f.src)
		}
	}
	walkStrings(c.Blocks, "blocks", check)
	walkStrings(c.Body, "body", check)
	return out
}

func walkStrings(v any, path string, fn func(field, src string)) {
	switch x := v.(type) {
	case string:
		fn(path, x)
	case []any:
		for i, item := range x {
			walkStrings(item, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	case map[string]any:
		for k, item := range x {
			walkStrings(item, path+"."+k, fn)
		}
	}
}
