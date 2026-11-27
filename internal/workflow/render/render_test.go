package render

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func data(t *testing.T, js string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(js))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v))
	return v
}

func run(t *testing.T, src string, d any) (string, error) {
	t.Helper()
	tpl, err := Parse(src)
	if err != nil {
		return "", err
	}
	return tpl.Execute(NewBudget(context.Background()), d, EscapeNone, 1<<20)
}

func TestParseRejectsOutsideSubset(t *testing.T) {
	t.Parallel()
	deep := strings.Repeat("{{#if a}}", MaxDepth+2) + strings.Repeat("{{/if}}", MaxDepth+2)
	cases := []struct {
		name, src, msg string
		line, col      int
	}{
		{"triple stash", "a {{{x}}}", "unescaped", 1, 3},
		{"ampersand", "{{& x}}", "unescaped", 1, 1},
		{"partial", "\n  {{> footer}}", "partials", 2, 3},
		{"helper", "{{upper payload.name}}", "helpers", 1, 1},
		{"else if", "{{#if a}}x{{else if b}}y{{/if}}", "helpers", 1, 11},
		{"block without path", "{{#each}}{{/each}}", "exactly one path", 1, 1},
		{"block with args", "{{#each a b}}{{/each}}", "exactly one path", 1, 1},
		{"unknown block", "{{#loop a}}{{/loop}}", "unknown block", 1, 1},
		{"unclosed block", "{{#if a}}x", "never closed", 1, 1},
		{"stray close", "x{{/if}}", "without a matching block", 1, 2},
		{"mismatched close", "{{#if a}}{{/each}}", "closes {{#if}}", 1, 10},
		{"double else", "{{#if a}}1{{else}}2{{else}}3{{/if}}", "more than one", 1, 20},
		{"tilde", "{{~a}}", "whitespace control", 1, 1},
		{"tilde end", "{{a~}}", "whitespace control", 1, 1},
		{"inverse", "{{^a}}x{{/a}}", "inverse", 1, 1},
		{"slash path", "{{a/b}}", "only letters", 1, 1},
		{"empty segment", "{{a..b}}", "malformed", 1, 1},
		{"unknown private", "{{@foo}}", "unknown variable", 1, 1},
		{"private member", "{{@index.x}}", "no members", 1, 1},
		{"too many parents", "{{" + strings.Repeat("../", maxParents+1) + "a}}", "levels", 1, 1},
		{"unclosed tag", "hello {{name", "unclosed {{", 1, 7},
		{"unclosed comment", "{{!-- x", "unclosed comment", 1, 1},
		{"empty tag", "{{ }}", "empty", 1, 1},
		{"subexpression", "{{(a)}}", "unsupported", 1, 1},
		{"string literal", `{{"x"}}`, "unsupported", 1, 1},
		{"too deep", deep, "nested deeper", 1, 1 + 9*(MaxDepth+1)},
		{"multibyte column", "héllo {{> x}}", "partials", 1, 7},
	}
	for _, tc := range cases {
		_, err := Parse(tc.src)
		var e *Error
		require.ErrorAs(t, err, &e, tc.name)
		assert.Contains(t, e.Msg, tc.msg, tc.name)
		assert.Equal(t, tc.line, e.Line, "%s line", tc.name)
		assert.Equal(t, tc.col, e.Column, "%s column", tc.name)
		assert.ErrorIs(t, err, ErrInvalid, tc.name)
	}
	_, err := Parse(strings.Repeat("x", MaxSource+1))
	require.ErrorIs(t, err, ErrInvalid, "oversized source")
}

func TestExecute(t *testing.T) {
	t.Parallel()
	d := data(t, `{"payload":{"name":"Ada","n":1000000,"zero":0,"zf":0.0,"s0":"0","empty":"","f":false,"t":true,
		"items":[{"sku":"A","qty":2},{"sku":"B","qty":1}],"none":[],"obj":{"b":2,"a":1},"emptyobj":{},
		"nested":{"deep":{"x":"y"}}},"brand":{"display_name":"Acme"}}`)
	cases := []struct{ name, src, want string }{
		{"text only", "hello", "hello"},
		{"variable", "Hi {{payload.name}}!", "Hi Ada!"},
		{"missing is empty", "[{{payload.nope.deeper}}]", "[]"},
		{"number keeps JSON text", "{{payload.n}}", "1000000"},
		{"bool", "{{payload.t}}", "true"},
		{"object renders as JSON", "{{payload.obj}}", `{"a":1,"b":2}`},
		{"comments", "a{{! c }}b{{!-- {{x}} --}}c", "abc"},
		{"if truthy", "{{#if payload.name}}y{{else}}n{{/if}}", "y"},
		{"if zero", "{{#if payload.zero}}y{{else}}n{{/if}}", "n"},
		{"if 0.0", "{{#if payload.zf}}y{{else}}n{{/if}}", "n"},
		{"if string 0 is truthy", "{{#if payload.s0}}y{{else}}n{{/if}}", "y"},
		{"if empty string", "{{#if payload.empty}}y{{else}}n{{/if}}", "n"},
		{"if false", "{{#if payload.f}}y{{else}}n{{/if}}", "n"},
		{"if empty array", "{{#if payload.none}}y{{else}}n{{/if}}", "n"},
		{"if empty object is truthy", "{{#if payload.emptyobj}}y{{else}}n{{/if}}", "y"},
		{"if missing", "{{#if payload.missing}}y{{else}}n{{/if}}", "n"},
		{"unless", "{{#unless payload.f}}y{{/unless}}", "y"},
		{"unless else", "{{#unless payload.t}}y{{else}}n{{/unless}}", "n"},
		{"each array", "{{#each payload.items}}{{@index}}:{{sku}}x{{qty}}{{#if @first}}F{{/if}}{{#if @last}}L{{/if}};{{/each}}", "0:Ax2F;1:Bx1L;"},
		{"each this", "{{#each payload.items}}{{this.sku}}{{/each}}", "AB"},
		{"each empty else", "{{#each payload.none}}x{{else}}none{{/each}}", "none"},
		{"each non-collection else", "{{#each payload.name}}x{{else}}no{{/each}}", "no"},
		{"each object sorted with @key", "{{#each payload.obj}}{{@key}}={{this}},{{/each}}", "a=1,b=2,"},
		{"parent scope", "{{#each payload.items}}{{../payload.name}}{{/each}}", "AdaAda"},
		{"if keeps scope", "{{#each payload.items}}{{#if qty}}{{sku}}{{/if}}{{/each}}", "AB"},
		{"root", "{{#each payload.items}}{{@root.brand.display_name}}{{/each}}", "AcmeAcme"},
		{"with", "{{#with payload.nested.deep}}{{x}}{{/with}}", "y"},
		{"with missing else", "{{#with payload.nope}}x{{else}}no{{/with}}", "no"},
		{"array index segment", "{{payload.items.1.sku}}", "B"},
		{"array index out of range", "[{{payload.items.9.sku}}]", "[]"},
		{"parent above root", "[{{../../payload.name}}]", "[]"},
		{"@index outside each", "[{{@index}}{{@key}}{{@first}}]", "[]"},
	}
	for _, tc := range cases {
		got, err := run(t, tc.src, d)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
}

func TestStringifyNativeNumbers(t *testing.T) {
	t.Parallel()
	got, err := run(t, "{{a}} {{b}} {{c}}", map[string]any{"a": 1e6, "b": 3, "c": int64(7)})
	require.NoError(t, err)
	assert.Equal(t, "1000000 3 7", got)
}

func TestHTMLEscaping(t *testing.T) {
	t.Parallel()
	tpl, err := Parse(`<p title="{{payload.x}}">{{payload.x}}</p>`)
	require.NoError(t, err)
	got, err := tpl.Execute(NewBudget(context.Background()), data(t, `{"payload":{"x":"<script>alert('x')</script>\"&=`+"`"+`"}}`), EscapeHTML, 1<<20)
	require.NoError(t, err)
	assert.NotContains(t, got, "<script>")
	assert.Contains(t, got, "&lt;script&gt;alert(&#x27;x&#x27;)&lt;/script&gt;&quot;&amp;&#x3D;&#x60;")
}

func TestBudgets(t *testing.T) {
	t.Parallel()
	items := make([]any, 400)
	for i := range items {
		items[i] = json.Number("1")
	}
	d := map[string]any{"a": items}

	_, err := run(t, "{{#each a}}{{#each ../a}}x{{/each}}{{/each}}", d)
	require.ErrorIs(t, err, ErrBudget, "400×400 loop exceeds the step budget")
	assert.True(t, IsBudget(err))

	tpl, err := Parse("{{#each a}}0123456789{{/each}}")
	require.NoError(t, err)
	_, err = tpl.Execute(NewBudget(context.Background()), d, EscapeNone, 1000)
	require.ErrorIs(t, err, ErrBudget, "output over the limit")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tpl, err = Parse("{{#each a}}{{#each ../a}}{{/each}}{{/each}}")
	require.NoError(t, err)
	_, err = tpl.Execute(NewBudget(ctx), d, EscapeNone, 1<<20)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, errors.Is(err, ErrInvalid), "cancellation is not a template error")
}

func TestPaths(t *testing.T) {
	t.Parallel()
	tpl, err := Parse("{{payload.a}}{{#each payload.items}}{{sku}}{{@index}}{{/each}}")
	require.NoError(t, err)
	assert.Equal(t, []string{"payload.a", "payload.items", "sku", "@index"}, tpl.Paths())
}

func TestErrorMessages(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "render: html at 2:3: bad", (&Error{Field: "html", Line: 2, Column: 3, Msg: "bad"}).Error())
	assert.Equal(t, "render at 1:1: bad", (&Error{Line: 1, Column: 1, Msg: "bad"}).Error())
	assert.Equal(t, "render: subject: bad", (&Error{Field: "subject", Msg: "bad"}).Error())
	assert.Equal(t, "render: a: x; render: b: y", Errors{{Field: "a", Msg: "x"}, {Field: "b", Msg: "y"}}.Error())

	_, err := Parse("{{#" + strings.Repeat("x", 60) + " a}}")
	var e *Error
	require.ErrorAs(t, err, &e)
	assert.Contains(t, e.Msg, "…", "long names are truncated in messages")
}
