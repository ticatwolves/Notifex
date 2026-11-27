package render

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

// Output limits per channel (ADR-012 §3).
const (
	MaxSubjectChars = 998
	MaxEmailBytes   = 512 << 10
	MaxSMSSegments  = 3
	MaxSlackBytes   = 40 << 10
	MaxSlackBlocks  = 50
	MaxWebhookBytes = 256 << 10
	maxJSONDepth    = 64
)

// Input is the render context (ADR-012 §1). Every value must be plain JSON
// data; callers decode with UseNumber so numbers keep their text.
type Input struct {
	Payload   map[string]any
	Recipient map[string]any
	Brand     map[string]any
	Workflow  map[string]any
}

func (in Input) data() map[string]any {
	nz := func(m map[string]any) map[string]any {
		if m == nil {
			return map[string]any{}
		}
		return m
	}
	return map[string]any{
		"payload": nz(in.Payload), "recipient": nz(in.Recipient), "brand": nz(in.Brand), "workflow": nz(in.Workflow),
	}
}

// field parses src and tags any error with the field name.
func field(name, src string) (*Template, error) {
	t, err := Parse(src)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			e.Field = name
			return nil, e
		}
		return nil, err
	}
	return t, nil
}

// exec renders a parsed field and tags failures with the field name.
func exec(name string, t *Template, b *Budget, data any, esc Escaper, maxOut int) (string, error) {
	out, err := t.Execute(b, data, esc, maxOut)
	if err != nil && errors.Is(err, ErrInvalid) {
		return "", &Error{Field: name, Msg: strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "), cause: err}
	}
	return out, err
}

// collect returns nil for no errors, else Errors.
func collect(errs []*Error) error {
	if len(errs) == 0 {
		return nil
	}
	return Errors(errs)
}

func asError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Msg: err.Error()}
}

// ctxOf keeps call sites short.
func ctxOf(ctx context.Context) *Budget { return NewBudget(ctx) }

func runeLen(s string) int { return utf8.RuneCountInString(s) }
