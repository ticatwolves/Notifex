package render

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ctx = context.Background()

func brand() map[string]any {
	return map[string]any{
		"display_name": "Acme <b>", "logo_url": "https://cdn.acme.test/logo.png", "footer_text": "Acme Ltd <script>",
		"legal_text": "Unsubscribe", "support_url": "https://acme.test/help",
		"colors": map[string]any{"primary": "#ff0000", "background": "red;background:url(x)"},
	}
}

func TestEmailRender(t *testing.T) {
	t.Parallel()
	in := Input{Payload: map[string]any{"name": `Ada <img src=x onerror=alert(1)>`, "url": "https://acme.test/o/1"}, Brand: brand()}
	out, err := Email(ctx, EmailTemplate{
		Subject: "Order for {{payload.name}}\r\nBcc: victim@example.com",
		HTML:    `<h1>Hi {{payload.name}}</h1><p>See <a href="{{payload.url}}">your order</a>.</p><script>steal()</script><img src="http://insecure.test/x.png"><p style="color:#00f;position:fixed" onclick="x()">styled</p>`,
	}, in)
	require.NoError(t, err)

	assert.NotContains(t, out.Subject, "\n")
	assert.NotContains(t, out.Subject, "\r")
	assert.True(t, strings.HasPrefix(out.Subject, "Order for Ada <img"), "subjects are plain text, not HTML-escaped")

	assert.NotContains(t, out.HTML, "<script")
	assert.NotContains(t, out.HTML, "<img src=x", "no injected tag (escaped text only)")
	assert.NotContains(t, out.HTML, "onclick")
	assert.NotContains(t, out.HTML, "position")
	assert.NotContains(t, out.HTML, "http://insecure.test")
	assert.Contains(t, out.HTML, "&lt;img src=x onerror=alert(1)&gt;", "payload values are escaped before sanitizing")
	assert.Contains(t, out.HTML, `href="https://acme.test/o/1"`)
	assert.Contains(t, out.HTML, "color: #00f")

	// Brand layout: escaped values, validated colours, https logo.
	assert.Contains(t, out.HTML, `src="https://cdn.acme.test/logo.png"`)
	assert.Contains(t, out.HTML, "Acme Ltd &lt;script&gt;")
	assert.Contains(t, out.HTML, "border-top:4px solid #ff0000")
	assert.NotContains(t, out.HTML, "url(x)", "invalid brand colours fall back to defaults")

	// Plain-text part generated from the HTML.
	assert.Contains(t, out.Text, "Hi Ada <img src=x onerror=alert(1)>")
	assert.Contains(t, out.Text, "your order (https://acme.test/o/1)")
	assert.NotContains(t, out.Text, "steal")
}

func TestEmailLayoutNoneAndExplicitText(t *testing.T) {
	t.Parallel()
	out, err := Email(ctx, EmailTemplate{Subject: "S", HTML: "<p>x</p>", Text: "Hello {{payload.n}}", Layout: "none"},
		Input{Payload: map[string]any{"n": json.Number("5")}, Brand: brand()})
	require.NoError(t, err)
	assert.Equal(t, "<p>x</p>", out.HTML)
	assert.Equal(t, "Hello 5", out.Text)

	out, err = Email(ctx, EmailTemplate{Subject: "S", Text: "only text"}, Input{})
	require.NoError(t, err)
	assert.Empty(t, out.HTML)
	assert.Equal(t, "only text", out.Text)
}

func TestEmailValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		email EmailTemplate
		field string
	}{
		{"no subject", EmailTemplate{HTML: "<p>x</p>"}, "subject"},
		{"no body", EmailTemplate{Subject: "s"}, "html"},
		{"bad layout", EmailTemplate{Subject: "s", HTML: "x", Layout: "fancy"}, "layout"},
		{"syntax in html", EmailTemplate{Subject: "s", HTML: "{{> p}}"}, "html"},
		{"syntax in text", EmailTemplate{Subject: "s", Text: "{{#if a}}"}, "text"},
	}
	for _, tc := range cases {
		err := ValidateEmail(tc.email)
		var es Errors
		require.ErrorAs(t, err, &es, tc.name)
		fields := []string{}
		for _, e := range es {
			fields = append(fields, e.Field)
		}
		assert.Contains(t, fields, tc.field, tc.name)
	}
	_, err := Email(ctx, EmailTemplate{Subject: "{{payload.s}}", Text: "x"}, Input{Payload: map[string]any{"s": " \r\n "}})
	require.ErrorIs(t, err, ErrInvalid, "subject that renders empty")
	_, err = Email(ctx, EmailTemplate{Subject: "{{payload.s}}", Text: "x"}, Input{Payload: map[string]any{"s": strings.Repeat("a", MaxSubjectChars+1)}})
	require.ErrorIs(t, err, ErrInvalid, "subject too long")
}

func TestHTMLToText(t *testing.T) {
	t.Parallel()
	got := HTMLToText(`<html><head><title>T</title><style>p{}</style></head><body><h1>Title</h1><p>Line one<br>Line two</p><ul><li>a</li><li>b</li></ul><table><tr><td>x</td><td>y</td></tr></table><p><a href="mailto:a@b.test">a@b.test</a> <a href="https://x.test">https://x.test</a> <a href="https://y.test"></a> &amp; done</p></body></html>`)
	assert.Equal(t, "Title\n\nLine one\nLine two\n\n- a\n- b\n\nx y\n\na@b.test https://x.test https://y.test & done", got)
	assert.Empty(t, HTMLToText(""))
}

func TestSMSSegments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		s        string
		enc      SMSEncoding
		units    int
		segments int
	}{
		{"empty", "", GSM7, 0, 0},
		{"160 gsm", strings.Repeat("a", 160), GSM7, 160, 1},
		{"161 gsm", strings.Repeat("a", 161), GSM7, 161, 2},
		{"306 gsm", strings.Repeat("a", 306), GSM7, 306, 2},
		{"307 gsm", strings.Repeat("a", 307), GSM7, 307, 3},
		{"extension chars count 2", strings.Repeat("€", 80), GSM7, 160, 1},
		{"escape pair fits one segment", strings.Repeat("a", 158) + "€", GSM7, 160, 1},
		{"escape pair pushed to next segment", strings.Repeat("a", 152) + "€" + strings.Repeat("a", 152), GSM7, 306, 3},
		{"70 ucs2", strings.Repeat("न", 70), UCS2, 70, 1},
		{"71 ucs2", strings.Repeat("न", 71), UCS2, 71, 2},
		{"emoji is two units", "😀", UCS2, 2, 1},
		{"surrogate pair pushed to next segment", strings.Repeat("न", 66) + "😀" + strings.Repeat("न", 66), UCS2, 134, 3},
		{"newlines are gsm", "a\nb\rc", GSM7, 5, 1},
	}
	for _, tc := range cases {
		info := SMSSegments(tc.s)
		assert.Equal(t, tc.enc, info.Encoding, tc.name)
		assert.Equal(t, tc.units, info.Units, tc.name)
		assert.Equal(t, tc.segments, info.Segments, tc.name)
	}
	info := SMSSegments("Hello ₹500 नमस्ते")
	assert.Equal(t, []string{"₹", "न", "म", "स", "्", "त", "े"}, info.UCS2)
}

func TestRenderSMS(t *testing.T) {
	t.Parallel()
	out, err := SMS(ctx, "Your OTP is {{payload.otp}}", Input{Payload: map[string]any{"otp": json.Number("123456")}})
	require.NoError(t, err)
	assert.Equal(t, "Your OTP is 123456", out.Text)
	assert.Equal(t, 1, out.Segments)

	_, err = SMS(ctx, "{{payload.x}}", Input{Payload: map[string]any{"x": strings.Repeat("a", 460)}})
	require.ErrorIs(t, err, ErrInvalid, "4 segments")
	_, err = SMS(ctx, "{{payload.x}}", Input{})
	require.ErrorIs(t, err, ErrInvalid, "renders empty")
	require.ErrorIs(t, ValidateSMS(" "), ErrInvalid)
	require.ErrorIs(t, ValidateSMS("{{#each}}"), ErrInvalid)
}

func TestSlack(t *testing.T) {
	t.Parallel()
	blocks := data(t, `[
		{"type":"section","text":{"type":"mrkdwn","text":"Order *{{payload.id}}* by {{payload.name}}"},
		 "accessory":{"type":"button","text":{"type":"plain_text","text":"Open"},"url":"{{payload.url}}","value":"{{payload.id}}"}},
		{"type":"divider"}]`).([]any)
	in := Input{Payload: map[string]any{"id": "A&1", "name": `<!channel> "}],"type":"x`, "url": "https://acme.test/o?a=1&b=2"}}
	out, err := Slack(ctx, SlackTemplate{Text: "New order {{payload.id}}", Blocks: blocks}, in)
	require.NoError(t, err)
	assert.Equal(t, "New order A&amp;1", out.Text)
	sec := out.Blocks[0].(map[string]any)
	assert.Equal(t, `Order *A&amp;1* by &lt;!channel&gt; "}],"type":"x`, sec["text"].(map[string]any)["text"], "mention injection escaped; JSON can't break out")
	acc := sec["accessory"].(map[string]any)
	assert.Equal(t, "https://acme.test/o?a=1&b=2", acc["url"], "URLs are not escaped")
	assert.Equal(t, "A&1", acc["value"])
	assert.Len(t, out.Blocks, 2, "payload can't add blocks")

	require.ErrorIs(t, ValidateSlack(SlackTemplate{}), ErrInvalid)
	require.ErrorIs(t, ValidateSlack(SlackTemplate{Blocks: []any{map[string]any{"type": "input"}}}), ErrInvalid, "block type allowlist")
	require.ErrorIs(t, ValidateSlack(SlackTemplate{Blocks: []any{"nope"}}), ErrInvalid)
	many := make([]any, MaxSlackBlocks+1)
	for i := range many {
		many[i] = map[string]any{"type": "divider"}
	}
	require.ErrorIs(t, ValidateSlack(SlackTemplate{Blocks: many}), ErrInvalid)
	err = ValidateSlack(SlackTemplate{Blocks: []any{map[string]any{"type": "section", "text": map[string]any{"text": "{{> x}}"}}}})
	var es Errors
	require.ErrorAs(t, err, &es)
	assert.Equal(t, "blocks[0].text.text", es[0].Field)

	_, err = Slack(ctx, SlackTemplate{Text: "{{payload.big}}"}, Input{Payload: map[string]any{"big": strings.Repeat("x", MaxSlackBytes+1)}})
	require.ErrorIs(t, err, ErrBudget)
}

func TestWebhook(t *testing.T) {
	t.Parallel()
	body := data(t, `{"event":"order.shipped","id":"{{payload.id}}","n":5,"ok":true,"x":null,"{{payload.id}}":"key not templated","items":["{{payload.id}}"]}`)
	raw, err := Webhook(ctx, body, Input{Payload: map[string]any{"id": `A","admin":true,"z":"`}})
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, `A","admin":true,"z":"`, got["id"])
	assert.NotContains(t, got, "admin")
	assert.Contains(t, got, "{{payload.id}}")
	assert.InDelta(t, 5, got["n"], 0)
	assert.Equal(t, true, got["ok"])
	assert.Nil(t, got["x"])

	require.ErrorIs(t, ValidateWebhook(nil), ErrInvalid)
	require.ErrorIs(t, ValidateWebhook(map[string]any{"a": "{{#if}}"}), ErrInvalid)

	var deep any = "x"
	for range maxJSONDepth + 2 {
		deep = []any{deep}
	}
	require.ErrorIs(t, ValidateWebhook(deep), ErrInvalid, "depth")

	// Many leaves share one output budget.
	leaves := map[string]any{}
	for i := range 20 {
		leaves[string(rune('a'+i))] = "{{payload.big}}"
	}
	_, err = Webhook(ctx, leaves, Input{Payload: map[string]any{"big": strings.Repeat("x", MaxWebhookBytes/10)}})
	require.ErrorIs(t, err, ErrBudget)
}
