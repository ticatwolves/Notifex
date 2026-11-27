// Package migrations embeds the versioned SQL migrations so cmd/migrate and
// integration tests apply exactly the reviewed files. Generate new ones with
// `make migration name=<snake_case>`; never edit an applied migration.
package migrations

import "embed"

// FS holds the *.up.sql files. Down files are kept for local experiments only:
// production is forward-only (expand/contract), and cmd/migrate has no down.
//
//go:embed *.up.sql
var FS embed.FS
