//go:build ignore

// Command main generates versioned SQL migrations from the ent schema.
//
//	go run -mod=mod ./ent/migrate/main.go -name add_widgets   # write a new migration
//	go run -mod=mod ./ent/migrate/main.go -check              # CI: fail if schema changed without a migration
//
// It replays migrations/ into a throwaway Postgres (testcontainers, or
// -dev-url), diffs against ent/schema plus the foreign keys declared in
// ent/schema/constraints.go, and writes golang-migrate files plus atlas.sum.
// Generated files are reviewed like code: never edit an applied migration.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	atlas "ariga.io/atlas/sql/schema"
	"ariga.io/atlas/sql/sqltool"
	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	entschema "entgo.io/ent/dialect/sql/schema"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/sbshobhit/notifex/ent/migrate"
	"github.com/sbshobhit/notifex/ent/schema"
)

// Keep in sync with docker-compose.yml and internal/store/storetest.
const devImage = "postgres:18-alpine@sha256:0115267ab5a509022df9319c77be365cef335adb71556c75f60f5360ee247e71"

func main() {
	name := flag.String("name", "", "migration name (snake_case)")
	check := flag.Bool("check", false, "fail if the schema differs from migrations/ (writes nothing)")
	devURL := flag.String("dev-url", "", "empty Postgres to replay into (default: start a container)")
	dir := flag.String("dir", "migrations", "migrations directory")
	flag.Parse()
	if *check == (*name != "") {
		log.Fatal("usage: -name <snake_case> | -check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := run(ctx, *name, *check, *devURL, *dir); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, name string, check bool, devURL, dir string) error {
	if devURL == "" {
		pg, err := postgres.Run(ctx, devImage,
			postgres.WithDatabase("dev"), postgres.WithUsername("dev"), postgres.WithPassword("dev"),
			postgres.BasicWaitStrategies())
		if err != nil {
			return fmt.Errorf("start dev postgres: %w", err)
		}
		defer func() { _ = pg.Terminate(context.WithoutCancel(ctx)) }()
		if devURL, err = pg.ConnectionString(ctx, "sslmode=disable"); err != nil {
			return err
		}
	}

	target := dir
	if check {
		tmp, err := os.MkdirTemp("", "notifex-migrations-check-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		if err := copyDir(dir, tmp); err != nil {
			return err
		}
		target, name = tmp, "check"
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	before, err := listDir(target)
	if err != nil {
		return err
	}

	mdir, err := sqltool.NewGolangMigrateDir(target)
	if err != nil {
		return fmt.Errorf("migrations dir: %w", err)
	}
	db, err := sql.Open("pgx", devURL)
	if err != nil {
		return err
	}
	defer db.Close()
	drv := entsql.OpenDB(dialect.Postgres, db)
	err = migrate.NewSchema(drv).NamedDiff(ctx, name,
		entschema.WithDir(mdir),
		entschema.WithMigrationMode(entschema.ModeReplay),
		entschema.WithDialect(dialect.Postgres),
		entschema.WithFormatter(sqltool.GolangMigrateFormatter),
		entschema.WithDropColumn(false), // destructive changes are written by hand, expand/contract
		entschema.WithDropIndex(true),
		entschema.WithDiffHook(addForeignKeys),
	)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}

	after, err := listDir(target)
	if err != nil {
		return err
	}
	var added []string
	for f := range after {
		if !before[f] && f != "atlas.sum" {
			added = append(added, f)
		}
	}
	switch {
	case check && len(added) > 0:
		return errors.New("ent schema has changes without a migration; run `make migration name=<name>` and commit it")
	case check:
		fmt.Println("migrations are in sync with ent/schema")
	case len(added) == 0:
		fmt.Println("no schema changes")
	default:
		fmt.Println("wrote:", strings.Join(added, ", "))
	}
	return nil
}

// addForeignKeys adds the declared foreign keys to the desired schema, so the
// diff creates them and never sees them as drift.
func addForeignKeys(next entschema.Differ) entschema.Differ {
	return entschema.DiffFunc(func(current, desired *atlas.Schema) ([]atlas.Change, error) {
		for _, fk := range schema.ForeignKeys() {
			t, ok := desired.Table(fk.Table)
			if !ok {
				return nil, fmt.Errorf("fk %s: no table %s", fk.Name, fk.Table)
			}
			rt, ok := desired.Table(fk.RefTable)
			if !ok {
				return nil, fmt.Errorf("fk %s: no table %s", fk.Name, fk.RefTable)
			}
			f := atlas.NewForeignKey(fk.Name).SetTable(t).SetRefTable(rt).SetOnUpdate(atlas.NoAction)
			switch fk.OnDelete {
			case "", "RESTRICT":
				f.SetOnDelete(atlas.Restrict)
			case "CASCADE":
				f.SetOnDelete(atlas.Cascade)
			default:
				return nil, fmt.Errorf("fk %s: unsupported on delete %q", fk.Name, fk.OnDelete)
			}
			for _, c := range fk.Columns {
				col, ok := t.Column(c)
				if !ok {
					return nil, fmt.Errorf("fk %s: no column %s.%s", fk.Name, fk.Table, c)
				}
				f.AddColumns(col)
			}
			for _, c := range fk.RefColumns {
				col, ok := rt.Column(c)
				if !ok {
					return nil, fmt.Errorf("fk %s: no column %s.%s", fk.Name, fk.RefTable, c)
				}
				f.AddRefColumns(col)
			}
			t.AddForeignKeys(f)
		}
		return next.Diff(current, desired)
	})
}

func listDir(dir string) (map[string]bool, error) {
	out := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out, nil
}

func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
