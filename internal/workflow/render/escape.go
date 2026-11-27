package render

import "strings"

// htmlEscaper escapes the same characters Handlebars does: & < > " ' ` =.
var htmlEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;", "`", "&#x60;", "=", "&#x3D;",
)

// slackEscaper escapes the three characters Slack requires in text.
var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// EscapeHTML is the escaper for HTML contexts.
func EscapeHTML(s string) string { return htmlEscaper.Replace(s) }

// EscapeNone is the escaper for plain-text contexts.
func EscapeNone(s string) string { return s }

// EscapeSlack is the escaper for Slack text (mrkdwn and plain_text).
func EscapeSlack(s string) string { return slackEscaper.Replace(s) }

// stripControl removes CR, LF, tabs and other control characters (header
// injection in email subjects) and trims the result.
func stripControl(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029:
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
