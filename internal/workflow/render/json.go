package render

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// SlackTemplate is a Slack template: text and/or Block Kit blocks.
type SlackTemplate struct {
	Text   string
	Blocks []any
}

// SlackOutput is a rendered Slack message.
type SlackOutput struct {
	Text   string
	Blocks []any
}

// slackBlockTypes are the Block Kit types allowed in v1 (ADR-012 §3).
var slackBlockTypes = map[string]bool{"section": true, "header": true, "divider": true, "context": true, "actions": true, "image": true}

// leafEscaper picks the escaper for a JSON string leaf by its key. In Slack
// only text fields are escaped; URLs, ids and values must stay verbatim.
type leafEscaper func(key string) Escaper

func slackLeaf(key string) Escaper {
	if key == "text" || key == "alt_text" {
		return EscapeSlack
	}
	return EscapeNone
}

func plainLeaf(string) Escaper { return EscapeNone }

// walk parses (render=false) or renders every string leaf of a JSON value.
// Keys are never templated. path names the leaf for errors ("blocks[2].text").
// remain is the output byte budget shared by every leaf of one render.
func walk(v any, path string, depth int, b *Budget, data any, leaf leafEscaper, key string, render bool, remain *int) (any, []*Error) {
	if depth > maxJSONDepth {
		return nil, []*Error{{Field: path, Msg: fmt.Sprintf("nested deeper than %d levels", maxJSONDepth)}}
	}
	switch c := v.(type) {
	case string:
		t, err := field(path, c)
		if err != nil {
			return nil, []*Error{asError(err)}
		}
		if !render {
			return c, nil
		}
		out, err := exec(path, t, b, data, leaf(key), *remain)
		if err != nil {
			return nil, []*Error{asError(err)}
		}
		*remain -= len(out)
		return out, nil
	case []any:
		res := make([]any, len(c))
		var errs []*Error
		for i, item := range c {
			r, e := walk(item, path+"["+strconv.Itoa(i)+"]", depth+1, b, data, leaf, key, render, remain)
			errs = append(errs, e...)
			res[i] = r
		}
		return res, errs
	case map[string]any:
		res := make(map[string]any, len(c))
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var errs []*Error
		for _, k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			r, e := walk(c[k], p, depth+1, b, data, leaf, k, render, remain)
			errs = append(errs, e...)
			res[k] = r
		}
		return res, errs
	default: // numbers, bools, null pass through
		return c, nil
	}
}

// ValidateSlack checks syntax and block structure (save time).
func ValidateSlack(t SlackTemplate) error {
	var errs []*Error
	if strings.TrimSpace(t.Text) == "" && len(t.Blocks) == 0 {
		errs = append(errs, &Error{Field: "text", Msg: "text or blocks is required"})
	}
	if len(t.Blocks) > MaxSlackBlocks {
		errs = append(errs, &Error{Field: "blocks", Msg: fmt.Sprintf("more than %d blocks", MaxSlackBlocks)})
	}
	for i, blk := range t.Blocks {
		m, ok := blk.(map[string]any)
		typ, _ := m["type"].(string)
		if !ok || !slackBlockTypes[typ] {
			errs = append(errs, &Error{Field: "blocks[" + strconv.Itoa(i) + "]", Msg: "block type must be one of section, header, divider, context, actions, image"})
		}
	}
	if _, err := field("text", t.Text); err != nil {
		errs = append(errs, asError(err))
	}
	if t.Blocks != nil {
		_, e := walk(anySlice(t.Blocks), "blocks", 0, nil, nil, slackLeaf, "", false, nil)
		errs = append(errs, e...)
	}
	return collect(errs)
}

func anySlice(s []any) any { return s }

// Slack renders a Slack message. Payload values can't add keys or
// blocks: only existing string leaves are rendered.
func Slack(ctx context.Context, t SlackTemplate, in Input) (SlackOutput, error) {
	if err := ValidateSlack(t); err != nil {
		return SlackOutput{}, err
	}
	b, data := ctxOf(ctx), in.data()
	var out SlackOutput
	if t.Text != "" {
		tt, _ := field("text", t.Text)
		s, err := exec("text", tt, b, data, EscapeSlack, MaxSlackBytes)
		if err != nil {
			return SlackOutput{}, err
		}
		out.Text = s
	}
	if t.Blocks != nil {
		remain := MaxSlackBytes - len(out.Text)
		r, errs := walk(anySlice(t.Blocks), "blocks", 0, b, data, slackLeaf, "", true, &remain)
		if len(errs) > 0 {
			return SlackOutput{}, Errors(errs)
		}
		out.Blocks, _ = r.([]any)
	}
	size, err := json.Marshal(out.Blocks)
	if err != nil {
		return SlackOutput{}, fmt.Errorf("render: slack: %w", err)
	}
	if len(size)+len(out.Text) > MaxSlackBytes {
		return SlackOutput{}, &Error{Field: "blocks", Msg: fmt.Sprintf("message is larger than %d KiB", MaxSlackBytes>>10)}
	}
	return out, nil
}

// ValidateWebhook checks syntax of every string leaf (save time).
func ValidateWebhook(body any) error {
	if body == nil {
		return Errors{{Field: "body", Msg: "body is required"}}
	}
	if _, errs := walk(body, "body", 0, nil, nil, plainLeaf, "", false, nil); len(errs) > 0 {
		return Errors(errs)
	}
	return nil
}

// Webhook renders a webhook JSON body and returns its encoding.
func Webhook(ctx context.Context, body any, in Input) (json.RawMessage, error) {
	if err := ValidateWebhook(body); err != nil {
		return nil, err
	}
	remain := MaxWebhookBytes
	r, errs := walk(body, "body", 0, ctxOf(ctx), in.data(), plainLeaf, "", true, &remain)
	if len(errs) > 0 {
		return nil, Errors(errs)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("render: webhook: %w", err)
	}
	if len(raw) > MaxWebhookBytes {
		return nil, &Error{Field: "body", Msg: fmt.Sprintf("body is larger than %d KiB", MaxWebhookBytes>>10)}
	}
	return raw, nil
}
