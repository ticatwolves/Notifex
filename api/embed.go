// Package api embeds the API contracts so tests and the server use exactly
// the reviewed files.
package api

import _ "embed"

// OpenAPI is api/openapi.yaml, the source of truth for the REST API.
//
//go:embed openapi.yaml
var OpenAPI []byte
