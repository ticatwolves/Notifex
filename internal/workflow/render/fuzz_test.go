package render

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func fuzzData(v string) map[string]any {
	items := []any{v, json.Number("1"), map[string]any{"k": v}, []any{v, v}, nil, true}
	return map[string]any{
		"payload": map[string]any{"x": v, "items": items, "obj": map[string]any{"a": v, "b": items}},
		"brand":   map[string]any{"display_name": v},
	}
}

// FuzzParseExecute: any source either fails to parse with a located *Error,
// or executes without panicking and within its budgets.
func FuzzParseExecute(f *testing.F) {
	for _, s := range []string{
		"", "hi {{payload.x}}", "{{#each payload.items}}{{@index}}{{this}}{{#each ../payload.items}}{{@key}}{{/each}}{{/each}}",
		"{{#if payload.x}}a{{else}}b{{/if}}", "{{#with payload.obj}}{{a}}{{../payload.x}}{{/with}}", "{{!-- c --}}{{! c }}",
		"{{{x}}}", "{{#if a}}", "{{/if}}", "{{> p}}", "{{a b}}", "{{@root.payload.x}}", "{{payload.items.2.k}}",
		"{{#unless payload.none}}u{{/unless}}", "{{", "}}", "{{#each payload.obj}}{{@key}}={{this}}{{/each}}",
	} {
		f.Add(s, "v")
	}
	f.Fuzz(func(t *testing.T, src, v string) {
		tpl, err := Parse(src)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || !errors.Is(err, ErrInvalid) {
				t.Fatalf("parse error is not a template *Error: %v", err)
			}
			if len(src) <= MaxSource && (e.Line < 1 || e.Column < 1) {
				t.Fatalf("parse error without a location: %v", err)
			}
			return
		}
		const maxOut = 64 << 10
		out, err := tpl.Execute(NewBudget(context.Background()), fuzzData(v), EscapeHTML, maxOut)
		if err != nil {
			if !errors.Is(err, ErrBudget) {
				t.Fatalf("execute failed with a non-budget error: %v", err)
			}
			return
		}
		if len(out) > maxOut {
			t.Fatalf("output %d bytes exceeds limit %d", len(out), maxOut)
		}
	})
}

// FuzzEscapeHTML: an escaped value never contains a character that could
// open a tag or end an attribute, and unescapes back to itself.
func FuzzEscapeHTML(f *testing.F) {
	for _, s := range []string{"", "<script>", `"'`, "a=b", "`x`", "&amp;", "\x00<"} {
		f.Add(s)
	}
	tpl, err := Parse(`{{payload.x}}`)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, v string) {
		out, err := tpl.Execute(NewBudget(context.Background()), fuzzData(v), EscapeHTML, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(out, "<>\"'`=") {
			t.Fatalf("escaped output contains a special character: %q", out)
		}
		if got := html.UnescapeString(out); utf8.ValidString(v) && got != v {
			t.Fatalf("round trip: got %q want %q", got, v)
		}
	})
}

var (
	dangerTag   = regexp.MustCompile(`(?i)<\s*/?\s*(script|style|iframe|object|embed|form|input|svg|math|link|meta|base)\b`)
	dangerAttr  = regexp.MustCompile(`(?i)<[^>]*\son[a-z]+\s*=`)
	dangerURL   = regexp.MustCompile(`(?i)(href|src)\s*=\s*["']?\s*(javascript|data|vbscript):`)
	insecureImg = regexp.MustCompile(`(?i)<img[^>]*src\s*=\s*["']?\s*http:`)
)

// FuzzSanitizeHTML: whatever a tenant writes, sanitized email HTML has no
// script-capable tags, event handlers, script URLs or insecure images.
func FuzzSanitizeHTML(f *testing.F) {
	for _, s := range []string{
		`<script>alert(1)</script>`, `<img src=x onerror=alert(1)>`, `<a href="javascript:alert(1)">x</a>`,
		`<a href="JaVaScRiPt:alert(1)">x</a>`, `<img src="data:image/png;base64,AA">`, `<svg onload=alert(1)>`,
		`<p style="background:url(javascript:alert(1))">x</p>`, `<iframe src="https://x"></iframe>`,
		`<<script>script>alert(1)<</script>/script>`, `<img src="http://x/y.png">`, `<a href="  javascript:x">`,
		`<math><mi xlink:href="javascript:alert(1)">`, `<form action=x><input></form>`, `<style>*{}</style>`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		out := SanitizeHTML(src)
		for _, re := range []*regexp.Regexp{dangerTag, dangerAttr, dangerURL, insecureImg} {
			if re.MatchString(out) {
				t.Fatalf("sanitized output matches %s: %q (input %q)", re, out, src)
			}
		}
	})
}

// FuzzSMSSegments: the segment count is consistent with the units.
func FuzzSMSSegments(f *testing.F) {
	for _, s := range []string{"", "hello", strings.Repeat("€", 81), "नमस्ते 😀", strings.Repeat("a", 306)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		info := SMSSegments(s)
		single, multi := 160, 153
		if info.Encoding == UCS2 {
			single, multi = 70, 67
		}
		switch {
		case info.Units == 0 && info.Segments != 0, info.Units > 0 && info.Segments < 1:
			t.Fatalf("units %d segments %d", info.Units, info.Segments)
		case info.Units <= single && info.Units > 0 && info.Segments != 1:
			t.Fatalf("%d units must be one segment, got %d", info.Units, info.Segments)
		case info.Units > single && info.Segments*multi < info.Units:
			t.Fatalf("%d segments can't hold %d units", info.Segments, info.Units)
		case info.Units > single && info.Segments > (info.Units+multi-1)/multi+info.Units/multi:
			t.Fatalf("too many segments: %d for %d units", info.Segments, info.Units)
		}
	})
}

// FuzzWebhookJSON: payload values never change the structure of the body.
func FuzzWebhookJSON(f *testing.F) {
	for _, s := range []string{`","admin":true,"x":"`, `\`, "\x00", `{{payload.x}}`, "}]"} {
		f.Add(s)
	}
	body := map[string]any{"a": "{{payload.x}}", "b": []any{"pre {{payload.x}} post", json.Number("1")}}
	f.Fuzz(func(t *testing.T, v string) {
		raw, err := Webhook(context.Background(), body, Input{Payload: map[string]any{"x": v}})
		if err != nil {
			if errors.Is(err, ErrBudget) {
				return
			}
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("keys changed: %v", got)
		}
		if b, ok := got["b"].([]any); !ok || len(b) != 2 {
			t.Fatalf("array changed: %v", got["b"])
		}
	})
}
