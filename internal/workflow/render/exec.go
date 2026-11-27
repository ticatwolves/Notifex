package render

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ErrBudget means a render exceeded a step or output limit (ADR-012 §2).
// It wraps ErrInvalid so callers can treat every template failure alike.
var ErrBudget = fmt.Errorf("%w: render budget exceeded", ErrInvalid)

// Escaper turns a value's text into output for its context.
type Escaper func(string) string

// Budget counts evaluation steps across every field of one render. The zero
// value is not usable; use NewBudget.
type Budget struct {
	ctx   context.Context
	left  int
	since int
}

// NewBudget returns a budget of MaxSteps bound to ctx.
func NewBudget(ctx context.Context) *Budget { return &Budget{ctx: ctx, left: MaxSteps} }

func (b *Budget) step() error {
	b.left--
	if b.left < 0 {
		return fmt.Errorf("%w: more than %d evaluation steps", ErrBudget, MaxSteps)
	}
	b.since++
	if b.since >= ctxEvery {
		b.since = 0
		if err := b.ctx.Err(); err != nil {
			return fmt.Errorf("render: %w", err)
		}
	}
	return nil
}

// limitedBuilder fails as soon as output would exceed max bytes.
type limitedBuilder struct {
	strings.Builder
	max int
}

func (w *limitedBuilder) write(s string) error {
	if w.Len()+len(s) > w.max {
		return fmt.Errorf("%w: output larger than %d bytes", ErrBudget, w.max)
	}
	w.WriteString(s)
	return nil
}

type frame struct {
	value          any
	index          int
	first, last    bool
	key            string
	hasIdx, hasKey bool
}

type execState struct {
	b      *Budget
	esc    Escaper
	out    *limitedBuilder
	root   any
	frames []frame // context frames pushed by each/with; [0] is the root
}

// Execute renders t over data (a JSON-like value: map[string]any, []any,
// string, json.Number, float64, bool, nil). Output larger than maxOut bytes,
// or more steps than the budget allows, is ErrBudget.
func (t *Template) Execute(b *Budget, data any, esc Escaper, maxOut int) (string, error) {
	st := &execState{b: b, esc: esc, out: &limitedBuilder{max: maxOut}, root: data, frames: []frame{{value: data}}}
	if err := st.run(t.nodes); err != nil {
		return "", err
	}
	return st.out.String(), nil
}

func (st *execState) run(nodes []node) error {
	for i := range nodes {
		n := &nodes[i]
		if err := st.b.step(); err != nil {
			return err
		}
		switch n.kind {
		case nText:
			if err := st.out.write(n.text); err != nil {
				return err
			}
		case nVar:
			v, _ := st.lookup(n.path)
			if err := st.out.write(st.esc(stringify(v))); err != nil {
				return err
			}
		case nBlock:
			if err := st.block(n); err != nil {
				return err
			}
		}
	}
	return nil
}

func (st *execState) block(n *node) error {
	v, _ := st.lookup(n.path)
	switch n.block {
	case bIf:
		if truthy(v) {
			return st.run(n.body)
		}
		return st.run(n.alt)
	case bUnless:
		if !truthy(v) {
			return st.run(n.body)
		}
		return st.run(n.alt)
	case bWith:
		if !truthy(v) {
			return st.run(n.alt)
		}
		return st.with(frame{value: v}, n.body)
	case bEach:
		return st.each(v, n)
	}
	return nil
}

func (st *execState) with(f frame, body []node) error {
	st.frames = append(st.frames, f)
	err := st.run(body)
	st.frames = st.frames[:len(st.frames)-1]
	return err
}

func (st *execState) each(v any, n *node) error {
	switch c := v.(type) {
	case []any:
		if len(c) == 0 {
			return st.run(n.alt)
		}
		for i, item := range c {
			if err := st.b.step(); err != nil {
				return err
			}
			f := frame{value: item, index: i, first: i == 0, last: i == len(c)-1, hasIdx: true}
			if err := st.with(f, n.body); err != nil {
				return err
			}
		}
	case map[string]any:
		if len(c) == 0 {
			return st.run(n.alt)
		}
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic output
		for i, k := range keys {
			if err := st.b.step(); err != nil {
				return err
			}
			f := frame{value: c[k], index: i, first: i == 0, last: i == len(keys)-1, key: k, hasIdx: true, hasKey: true}
			if err := st.with(f, n.body); err != nil {
				return err
			}
		}
	default:
		return st.run(n.alt)
	}
	return nil
}

// lookup resolves a path against the current frames. Missing is (nil,false).
func (st *execState) lookup(p path) (any, bool) {
	idx := len(st.frames) - 1 - p.parents
	if idx < 0 {
		return nil, false
	}
	f := st.frames[idx]
	var cur any
	switch p.private {
	case "":
		cur = f.value
	case "root":
		cur = st.root
	case "index":
		if !f.hasIdx {
			return nil, false
		}
		return json.Number(strconv.Itoa(f.index)), true
	case "first":
		if !f.hasIdx {
			return nil, false
		}
		return f.first, true
	case "last":
		if !f.hasIdx {
			return nil, false
		}
		return f.last, true
	case "key":
		if !f.hasKey {
			return nil, false
		}
		return f.key, true
	}
	for _, seg := range p.segs {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			cur = c[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// truthy follows Handlebars: false, nil, "", 0 and [] are falsy; {} is truthy.
func truthy(v any) bool {
	switch c := v.(type) {
	case nil:
		return false
	case bool:
		return c
	case string:
		return c != ""
	case json.Number:
		f, err := c.Float64()
		return err != nil || f != 0
	case float64:
		return c != 0
	case int:
		return c != 0
	case int64:
		return c != 0
	case []any:
		return len(c) > 0
	default:
		return true
	}
}

// stringify renders a value as text. Numbers keep their JSON form (no 1e+06);
// arrays and objects render as compact JSON.
func stringify(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case json.Number:
		return c.String()
	case bool:
		return strconv.FormatBool(c)
	case float64:
		if math.IsInf(c, 0) || math.IsNaN(c) {
			return ""
		}
		return strconv.FormatFloat(c, 'f', -1, 64)
	case int:
		return strconv.Itoa(c)
	case int64:
		return strconv.FormatInt(c, 10)
	default:
		b, err := json.Marshal(c)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// IsBudget reports whether err is a budget failure (as opposed to syntax).
func IsBudget(err error) bool { return errors.Is(err, ErrBudget) }
