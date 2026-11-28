package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// MaxSchemaBytes caps a workflow's payload schema (encoded).
const MaxSchemaBytes = 64 << 10

const schemaURL = "urn:notifex:payload-schema"

// noLoader refuses every external reference: payload schemas are
// self-contained (no remote or file $ref: SSRF and local file reads).
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external $ref %q is not allowed; payload schemas must be self-contained", url)
}

// PayloadSchema is a compiled JSON Schema (2020-12) for trigger payloads.
type PayloadSchema struct {
	doc map[string]any
	sch *jsonschema.Schema
}

// CompileSchema checks and compiles a payload schema.
func CompileSchema(doc map[string]any) (*PayloadSchema, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fieldErr("payload_schema", "not valid JSON")
	}
	if len(raw) > MaxSchemaBytes {
		return nil, fieldErr("payload_schema", "larger than %d KiB", MaxSchemaBytes>>10)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fieldErr("payload_schema", "not valid JSON")
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	if err := c.AddResource(schemaURL, v); err != nil {
		return nil, fieldErr("payload_schema", "%s", schemaMsg(err))
	}
	sch, err := c.Compile(schemaURL)
	if err != nil {
		return nil, fieldErr("payload_schema", "%s", schemaMsg(err))
	}
	return &PayloadSchema{doc: doc, sch: sch}, nil
}

// Validate checks a payload (decoded with UseNumber, or via jsonschema's
// UnmarshalJSON) against the schema.
func (p *PayloadSchema) Validate(payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	if err := p.sch.Validate(v); err != nil {
		return errors.New(schemaMsg(err))
	}
	return nil
}

// Defines reports whether a path under payload (segments after "payload")
// is described by the schema. Unknown means true: only paths under an
// object schema that lists properties (and forbids or omits others) count
// as undefined.
func (p *PayloadSchema) Defines(segs []string) bool {
	cur := p.doc
	for _, seg := range segs {
		if items, ok := cur["items"].(map[string]any); ok && isIndex(seg) {
			cur = items
			continue
		}
		props, ok := cur["properties"].(map[string]any)
		if !ok {
			return true // nothing listed: can't tell
		}
		next, ok := props[seg].(map[string]any)
		if !ok {
			if ap, set := cur["additionalProperties"]; set && ap != false {
				return true // extra properties are described elsewhere
			}
			return false
		}
		cur = next
	}
	return true
}

func isIndex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// schemaMsg flattens a jsonschema error into one line.
func schemaMsg(err error) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "\n", "; ")), " ")
}
