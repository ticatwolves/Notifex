// Package render is the sandboxed template renderer (ADR-012): a strict
// Handlebars subset parsed into an AST and evaluated over plain JSON values
// with step, depth and output budgets. Templates can't call functions, reach
// Go methods, include partials or emit unescaped HTML.
package render

import (
	"errors"
	"fmt"
	"strings"
)

// Limits are the ADR-012 §2 budgets.
const (
	MaxSource   = 64 << 10 // per template field
	MaxDepth    = 32       // block nesting
	MaxSteps    = 100_000  // nodes visited, including loop iterations
	maxPathLen  = 256
	maxSegments = 32
	maxParents  = 8 // ../ levels
	ctxEvery    = 1024
)

// Error is a template problem with its location (1-based line and column).
// Field is set by the channel renderers ("subject", "html", "blocks[2].text").
type Error struct {
	Field  string
	Line   int
	Column int
	Msg    string
	cause  error // e.g. ErrBudget
}

// Unwrap exposes the cause (errors.Is(err, ErrBudget)).
func (e *Error) Unwrap() error { return e.cause }

func (e *Error) Error() string {
	loc := ""
	if e.Line > 0 {
		loc = fmt.Sprintf(" at %d:%d", e.Line, e.Column)
	}
	if e.Field != "" {
		return fmt.Sprintf("render: %s%s: %s", e.Field, loc, e.Msg)
	}
	return fmt.Sprintf("render%s: %s", loc, e.Msg)
}

// Errors is one or more template problems (one per field at most).
type Errors []*Error

func (es Errors) Error() string {
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// ErrInvalid is matched by every template problem (errors.Is).
var ErrInvalid = errors.New("render: invalid template")

// Is lets errors.Is(err, ErrInvalid) match *Error and Errors.
func (e *Error) Is(target error) bool { return target == ErrInvalid }

// Is matches ErrInvalid.
func (es Errors) Is(target error) bool { return target == ErrInvalid }

// Unwrap exposes each problem (errors.Is(err, ErrBudget) on any of them).
func (es Errors) Unwrap() []error {
	out := make([]error, len(es))
	for i, e := range es {
		out[i] = e
	}
	return out
}

// ---- AST

type nodeKind uint8

const (
	nText nodeKind = iota
	nVar
	nBlock
)

type blockKind uint8

const (
	bIf blockKind = iota
	bUnless
	bEach
	bWith
)

var blockNames = map[string]blockKind{"if": bIf, "unless": bUnless, "each": bEach, "with": bWith}

type node struct {
	kind  nodeKind
	text  string // nText
	path  path   // nVar, nBlock
	block blockKind
	body  []node
	alt   []node // {{else}} branch
	line  int
	col   int
}

// path is a parsed reference: parents (../ count), an optional private
// variable (@index, @root…) and member segments.
type path struct {
	parents int
	private string // "", "index", "first", "last", "key", "root"
	segs    []string
	raw     string
}

// Template is a parsed template.
type Template struct {
	nodes []node
	paths []string
}

// Paths lists the data paths the template references, as written (for
// schema warnings).
func (t *Template) Paths() []string { return append([]string(nil), t.paths...) }

// Parse parses src. Anything outside the ADR-012 subset is an *Error with a
// location.
func Parse(src string) (*Template, error) {
	if len(src) > MaxSource {
		return nil, &Error{Msg: fmt.Sprintf("template is larger than %d KiB", MaxSource>>10)}
	}
	p := &parser{src: src, line: 1, col: 1}
	nodes, end, err := p.parseUntil(0)
	if err != nil {
		return nil, err
	}
	if end != nil {
		return nil, p.errAt(end.line, end.col, "{{"+end.raw+"}} without a matching block")
	}
	return &Template{nodes: nodes, paths: p.paths}, nil
}

type parser struct {
	src       string
	pos       int
	line, col int
	paths     []string
}

// closer is an {{else}} or {{/x}} that ends the current node list.
type closer struct {
	isElse    bool
	name      string
	raw       string
	line, col int
}

func (p *parser) errAt(line, col int, msg string) *Error {
	return &Error{Line: line, Column: col, Msg: msg}
}

// advance moves pos forward by n bytes, tracking line and column.
func (p *parser) advance(n int) {
	for i := 0; i < n && p.pos < len(p.src); i++ {
		if p.src[p.pos] == '\n' {
			p.line++
			p.col = 1
		} else if p.src[p.pos]&0xC0 != 0x80 { // count runes, not bytes
			p.col++
		}
		p.pos++
	}
}

func (p *parser) parseUntil(depth int) ([]node, *closer, error) {
	if depth > MaxDepth {
		return nil, nil, p.errAt(p.line, p.col, fmt.Sprintf("blocks nested deeper than %d", MaxDepth))
	}
	var out []node
	for p.pos < len(p.src) {
		i := strings.Index(p.src[p.pos:], "{{")
		if i < 0 {
			out = append(out, node{kind: nText, text: p.src[p.pos:]})
			p.advance(len(p.src) - p.pos)
			break
		}
		if i > 0 {
			out = append(out, node{kind: nText, text: p.src[p.pos : p.pos+i]})
			p.advance(i)
		}
		line, col := p.line, p.col
		rest := p.src[p.pos:]

		// Comments.
		if strings.HasPrefix(rest, "{{!--") {
			j := strings.Index(rest, "--}}")
			if j < 0 {
				return nil, nil, p.errAt(line, col, "unclosed comment {{!-- … --}}")
			}
			p.advance(j + 4)
			continue
		}
		if strings.HasPrefix(rest, "{{!") {
			j := strings.Index(rest, "}}")
			if j < 0 {
				return nil, nil, p.errAt(line, col, "unclosed comment {{! … }}")
			}
			p.advance(j + 2)
			continue
		}
		if strings.HasPrefix(rest, "{{{") {
			return nil, nil, p.errAt(line, col, "{{{ … }}} (unescaped output) is not supported")
		}
		j := strings.Index(rest, "}}")
		if j < 0 {
			return nil, nil, p.errAt(line, col, "unclosed {{")
		}
		raw := rest[2:j]
		p.advance(j + 2)
		tag := strings.TrimSpace(raw)

		switch {
		case tag == "":
			return nil, nil, p.errAt(line, col, "empty {{ }}")
		case strings.ContainsAny(tag[:1], ">&^*~=(\"'") || strings.HasPrefix(tag, "#>") || strings.HasPrefix(tag, "#*"):
			return nil, nil, p.errAt(line, col, unsupported(tag))
		case strings.HasSuffix(tag, "~") || strings.HasPrefix(tag, "~"):
			return nil, nil, p.errAt(line, col, "whitespace control (~) is not supported")
		case tag == "else":
			return out, &closer{isElse: true, raw: tag, line: line, col: col}, nil
		case strings.HasPrefix(tag, "/"):
			return out, &closer{name: strings.TrimSpace(tag[1:]), raw: tag, line: line, col: col}, nil
		case strings.HasPrefix(tag, "#"):
			n, err := p.parseBlock(tag, line, col, depth)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, n)
		default:
			pth, err := p.parsePath(tag, line, col)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, node{kind: nVar, path: pth, line: line, col: col})
		}
	}
	return out, nil, nil
}

func unsupported(tag string) string {
	switch tag[0] {
	case '>':
		return "partials ({{> …}}) are not supported"
	case '&':
		return "unescaped output ({{& …}}) is not supported"
	case '^':
		return "inverse sections ({{^ …}}) are not supported; use {{#unless}} or {{else}}"
	case '~':
		return "whitespace control (~) is not supported"
	default:
		return fmt.Sprintf("unsupported expression {{%s}}", truncate(tag, 40))
	}
}

func (p *parser) parseBlock(tag string, line, col, depth int) (node, error) {
	fields := strings.Fields(tag[1:])
	if len(fields) == 0 {
		return node{}, p.errAt(line, col, "block without a name")
	}
	name := fields[0]
	kind, ok := blockNames[name]
	if !ok {
		return node{}, p.errAt(line, col, fmt.Sprintf("unknown block {{#%s}}; supported: if, unless, each, with", truncate(name, 20)))
	}
	if len(fields) != 2 {
		return node{}, p.errAt(line, col, fmt.Sprintf("{{#%s}} takes exactly one path (helpers and arguments are not supported)", name))
	}
	pth, err := p.parsePath(fields[1], line, col)
	if err != nil {
		return node{}, err
	}
	n := node{kind: nBlock, block: kind, path: pth, line: line, col: col}
	body, end, err := p.parseUntil(depth + 1)
	if err != nil {
		return node{}, err
	}
	n.body = body
	if end != nil && end.isElse {
		alt, end2, err := p.parseUntil(depth + 1)
		if err != nil {
			return node{}, err
		}
		if end2 != nil && end2.isElse {
			return node{}, p.errAt(end2.line, end2.col, "more than one {{else}} in a block")
		}
		n.alt, end = alt, end2
	}
	if end == nil {
		return node{}, p.errAt(line, col, fmt.Sprintf("{{#%s}} is never closed", name))
	}
	if end.name != name {
		return node{}, p.errAt(end.line, end.col, fmt.Sprintf("{{/%s}} closes {{#%s}} opened at %d:%d", truncate(end.name, 20), name, line, col))
	}
	return n, nil
}

func isSegChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func (p *parser) parsePath(s string, line, col int) (path, error) {
	bad := func(msg string) (path, error) { return path{}, p.errAt(line, col, msg) }
	if strings.ContainsAny(s, " \t\r\n") {
		return bad(fmt.Sprintf("{{%s}}: helpers and arguments are not supported", truncate(s, 40)))
	}
	if len(s) > maxPathLen {
		return bad("path is too long")
	}
	pth := path{raw: s}
	rest := s
	for strings.HasPrefix(rest, "../") {
		pth.parents++
		rest = rest[3:]
	}
	if pth.parents > maxParents {
		return bad(fmt.Sprintf("more than %d ../ levels", maxParents))
	}
	if strings.HasPrefix(rest, "@") {
		name := rest[1:]
		if k := strings.IndexByte(name, '.'); k >= 0 {
			name, rest = name[:k], name[k+1:]
		} else {
			rest = ""
		}
		switch name {
		case "index", "first", "last", "key":
			if rest != "" {
				return bad(fmt.Sprintf("@%s has no members", name))
			}
		case "root":
			if pth.parents > 0 {
				return bad("../@root is not valid")
			}
		default:
			return bad(fmt.Sprintf("unknown variable @%s", truncate(name, 20)))
		}
		pth.private = name
	} else if rest == "this" || rest == "." {
		rest = ""
	} else if strings.HasPrefix(rest, "this.") {
		rest = rest[5:]
	}
	if rest != "" {
		for _, seg := range strings.Split(rest, ".") {
			if seg == "" {
				return bad(fmt.Sprintf("malformed path %q", truncate(s, 40)))
			}
			for i := 0; i < len(seg); i++ {
				if !isSegChar(seg[i]) {
					return bad(fmt.Sprintf("path %q: only letters, digits, _ and - are allowed in names", truncate(s, 40)))
				}
			}
			pth.segs = append(pth.segs, seg)
		}
		if len(pth.segs) > maxSegments {
			return bad("path has too many segments")
		}
	}
	p.paths = append(p.paths, s)
	return pth, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
