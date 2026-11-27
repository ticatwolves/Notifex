package render

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"regexp"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
)

// EmailTemplate is an email template (ADR-012 §3).
type EmailTemplate struct {
	Subject string
	HTML    string
	Text    string
	Layout  string // "default" (or "") wraps the brand layout; "none" doesn't
}

// EmailOutput is a rendered email. HTML is sanitized; Text is generated from
// it when the template has no text part.
type EmailOutput struct {
	Subject string
	HTML    string
	Text    string
}

// ValidateEmail checks syntax and structure without data (save time).
func ValidateEmail(t EmailTemplate) error {
	var errs []*Error
	if strings.TrimSpace(t.Subject) == "" {
		errs = append(errs, &Error{Field: "subject", Msg: "subject is required"})
	}
	if t.HTML == "" && t.Text == "" {
		errs = append(errs, &Error{Field: "html", Msg: "html or text is required"})
	}
	if t.Layout != "" && t.Layout != "default" && t.Layout != "none" {
		errs = append(errs, &Error{Field: "layout", Msg: `layout must be "default" or "none"`})
	}
	for _, f := range []struct{ name, src string }{{"subject", t.Subject}, {"html", t.HTML}, {"text", t.Text}} {
		if _, err := field(f.name, f.src); err != nil {
			errs = append(errs, asError(err))
		}
	}
	return collect(errs)
}

// Email renders, sanitizes and lays out an email.
func Email(ctx context.Context, t EmailTemplate, in Input) (EmailOutput, error) {
	if err := ValidateEmail(t); err != nil {
		return EmailOutput{}, err
	}
	b, data := ctxOf(ctx), in.data()
	var out EmailOutput

	st, _ := field("subject", t.Subject)
	subject, err := exec("subject", st, b, data, EscapeNone, MaxEmailBytes)
	if err != nil {
		return EmailOutput{}, err
	}
	out.Subject = stripControl(subject)
	if runeLen(out.Subject) > MaxSubjectChars {
		return EmailOutput{}, &Error{Field: "subject", Msg: fmt.Sprintf("subject is longer than %d characters", MaxSubjectChars)}
	}
	if out.Subject == "" {
		return EmailOutput{}, &Error{Field: "subject", Msg: "subject rendered empty"}
	}

	if t.HTML != "" {
		ht, _ := field("html", t.HTML)
		body, err := exec("html", ht, b, data, EscapeHTML, MaxEmailBytes)
		if err != nil {
			return EmailOutput{}, err
		}
		body = SanitizeHTML(body)
		if t.Layout != "none" {
			body, err = layout(body, in.Brand)
			if err != nil {
				return EmailOutput{}, err
			}
		}
		if len(body) > MaxEmailBytes {
			return EmailOutput{}, &Error{Field: "html", Msg: fmt.Sprintf("html is larger than %d KiB", MaxEmailBytes>>10)}
		}
		out.HTML = body
	}
	if t.Text != "" {
		tt, _ := field("text", t.Text)
		if out.Text, err = exec("text", tt, b, data, EscapeNone, MaxEmailBytes); err != nil {
			return EmailOutput{}, err
		}
	} else {
		out.Text = HTMLToText(out.HTML)
	}
	return out, nil
}

var (
	policyOnce sync.Once
	policy     *bluemonday.Policy
	httpsOnly  = regexp.MustCompile(`^https://`)
	dimension  = regexp.MustCompile(`^[0-9]{1,4}%?$`)
	colorAttr  = regexp.MustCompile(`^#[0-9a-fA-F]{3,6}$`)
	hex6       = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// emailPolicy is ADR-012 §5: text and table markup, https images, http(s)
// and mailto links, inline styles from an allowlist. No script, style,
// iframe, forms, event handlers, javascript: or data: URLs.
func emailPolicy() *bluemonday.Policy {
	policyOnce.Do(func() {
		p := bluemonday.NewPolicy()
		p.AllowElements("a", "abbr", "b", "blockquote", "br", "caption", "center", "code", "col", "colgroup",
			"dd", "div", "dl", "dt", "em", "h1", "h2", "h3", "h4", "h5", "h6", "hr", "i", "img", "li", "ol",
			"p", "pre", "s", "small", "span", "strong", "sub", "sup", "table", "tbody", "td", "tfoot", "th",
			"thead", "tr", "u", "ul")
		p.AllowURLSchemes("https", "http", "mailto")
		p.RequireParseableURLs(true)
		p.AllowAttrs("href", "title").OnElements("a")
		p.RequireNoFollowOnLinks(false)
		p.AllowAttrs("src").Matching(httpsOnly).OnElements("img")
		p.AllowAttrs("alt", "title").OnElements("img", "abbr")
		p.AllowAttrs("width", "height").Matching(dimension).OnElements("img", "table", "td", "th", "col")
		p.AllowAttrs("align").Matching(regexp.MustCompile(`^(left|right|center|justify)$`)).OnElements("p", "div", "table", "td", "th", "tr", "img", "h1", "h2", "h3", "h4", "h5", "h6")
		p.AllowAttrs("valign").Matching(regexp.MustCompile(`^(top|middle|bottom|baseline)$`)).OnElements("td", "th", "tr")
		p.AllowAttrs("colspan", "rowspan").Matching(regexp.MustCompile(`^[0-9]{1,3}$`)).OnElements("td", "th")
		p.AllowAttrs("cellpadding", "cellspacing", "border").Matching(regexp.MustCompile(`^[0-9]{1,2}$`)).OnElements("table")
		p.AllowAttrs("bgcolor").Matching(colorAttr).OnElements("table", "td", "th", "tr")
		p.AllowStyles("color", "background-color", "font-family", "font-size", "font-weight", "font-style",
			"line-height", "text-align", "text-decoration", "padding", "padding-top", "padding-right",
			"padding-bottom", "padding-left", "margin", "margin-top", "margin-right", "margin-bottom",
			"margin-left", "border", "border-top", "border-right", "border-bottom", "border-left",
			"border-radius", "border-collapse", "width", "max-width", "height", "vertical-align", "display").Globally()
		policy = p
	})
	return policy
}

// SanitizeHTML applies the email policy.
func SanitizeHTML(s string) string { return emailPolicy().Sanitize(s) }

var layoutTmpl = template.Must(template.New("layout").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"></head>
<body style="margin:0;padding:0;background-color:{{.Background}}">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background-color:{{.Background}}"><tr><td align="center">
<table role="presentation" width="600" cellpadding="0" cellspacing="0" style="max-width:600px;width:100%;color:{{.Text}};font-family:Arial,Helvetica,sans-serif">
<tr><td style="border-top:4px solid {{.Primary}};padding:24px">{{if .Logo}}<img src="{{.Logo}}" alt="{{.Name}}" height="40">{{else if .Name}}<strong>{{.Name}}</strong>{{end}}</td></tr>
<tr><td style="padding:0 24px 24px">{{.Body}}</td></tr>
{{if or .Footer .Legal .Support}}<tr><td style="padding:16px 24px;font-size:12px;color:#6b7280;border-top:1px solid #e5e7eb">{{if .Footer}}<p>{{.Footer}}</p>{{end}}{{if .Support}}<p><a href="{{.Support}}">{{.Support}}</a></p>{{end}}{{if .Legal}}<p>{{.Legal}}</p>{{end}}</td></tr>{{end}}
</table></td></tr></table></body></html>`))

type layoutData struct {
	Name, Logo, Footer, Legal, Support string
	Primary, Background, Text          template.CSS
	Body                               template.HTML
}

func str(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

// color returns a validated hex colour or the default (never tenant CSS).
func color(m map[string]any, k, def string) template.CSS {
	if c, ok := m[k].(string); ok && hex6.MatchString(c) {
		return template.CSS(c) //nolint:gosec // G203: validated #rrggbb
	}
	return template.CSS(def) //nolint:gosec // G203: constant
}

// layout wraps sanitized body HTML in the brand layout. Brand values are
// escaped by html/template; the logo must be https.
func layout(body string, brand map[string]any) (string, error) {
	colors, _ := brand["colors"].(map[string]any)
	d := layoutData{
		Name: str(brand, "display_name"), Footer: str(brand, "footer_text"), Legal: str(brand, "legal_text"),
		Primary: color(colors, "primary", "#111827"), Background: color(colors, "background", "#f3f4f6"),
		Text: color(colors, "text", "#111827"),
		Body: template.HTML(body), //nolint:gosec // G203: sanitized by SanitizeHTML just before
	}
	if l := str(brand, "logo_url"); httpsOnly.MatchString(l) {
		d.Logo = l
	}
	if s := str(brand, "support_url"); httpsOnly.MatchString(s) {
		d.Support = s
	}
	var buf bytes.Buffer
	if err := layoutTmpl.Execute(&buf, d); err != nil {
		return "", fmt.Errorf("render: email layout: %w", err)
	}
	return buf.String(), nil
}

// HTMLToText converts sanitized HTML to a plain-text part: block elements end
// lines, list items get "- ", links become "text (url)".
func HTMLToText(s string) string {
	if s == "" {
		return ""
	}
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	var href []string
	var linkText []strings.Builder
	skip := 0
	write := func(t string) {
		if len(linkText) > 0 {
			linkText[len(linkText)-1].WriteString(t)
			return
		}
		b.WriteString(t)
	}
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return tidyText(b.String())
		case html.TextToken:
			if skip == 0 {
				write(collapse(string(z.Text())))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "head", "style", "script", "title":
				if tt == html.StartTagToken {
					skip++
				}
			case "br":
				write("\n")
			case "li":
				write("\n- ")
			case "p", "div", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "table", "ul", "ol", "blockquote", "hr":
				write("\n")
			case "td", "th":
				write(" ")
			case "a":
				link := ""
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					if string(k) == "href" {
						link = string(v)
					}
				}
				if tt == html.StartTagToken {
					href = append(href, link)
					linkText = append(linkText, strings.Builder{})
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "head", "style", "script", "title":
				if skip > 0 {
					skip--
				}
			case "p", "div", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "table", "ul", "ol", "blockquote":
				write("\n")
			case "a":
				if n := len(href); n > 0 {
					text := strings.TrimSpace(linkText[n-1].String())
					link := href[n-1]
					href, linkText = href[:n-1], linkText[:n-1]
					switch {
					case link == "" || link == text || "mailto:"+text == link:
						write(text)
					case text == "":
						write(link)
					default:
						write(text + " (" + link + ")")
					}
				}
			}
		}
	}
}

var spaces = regexp.MustCompile(`[ \t\r\n\f]+`)

func collapse(s string) string { return spaces.ReplaceAllString(html.UnescapeString(s), " ") }

// tidyText trims each line and collapses runs of blank lines.
func tidyText(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
