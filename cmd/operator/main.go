// Command operator provisions platform operator accounts (ADR-011 §5). There
// is no signup endpoint for operators: this CLI is the only way to create one.
//
//	operator create -email ops@example.com -name "Ada"   # password read from stdin
//	operator disable -email ops@example.com
//
// `create` prints the TOTP enrollment URI (it contains the secret) exactly
// once. Run it from a trusted terminal against the database (for example
// through a bastion port-forward). Never run it as an ECS task: the output
// would end up in CloudWatch Logs.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ticatwolves/notifex/internal/app"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/config"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/db"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/operator"
	"github.com/ticatwolves/notifex/internal/store/entstore"
)

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) < 2 || (os.Args[1] != "create" && os.Args[1] != "disable") {
		fmt.Fprintln(os.Stderr, "usage: operator create -email E [-name N] | operator disable -email E")
		return 2
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	email := fs.String("email", "", "operator email")
	name := fs.String("name", "", "display name")
	if err := fs.Parse(os.Args[2:]); err != nil || *email == "" {
		fmt.Fprintln(os.Stderr, "operator: -email is required")
		return 2
	}

	cfg, err := config.Load("operator-cli", app.Version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "operator: config:", err)
		return 2
	}
	log := observability.NewLogger(cfg, os.Stderr)
	if cfg.Deployed() {
		// No KMS KeyProvider until Phase 8, and never the local key outside local.
		log.Error("operator CLI needs the KMS KeyProvider (Phase 8) outside NOTIFEX_ENV=local")
		return 2
	}
	kp, err := crypto.NewLocalFromBase64(cfg.LocalMasterKey)
	if err != nil {
		log.Error("NOTIFEX_LOCAL_MASTER_KEY", slog.Any("error", err))
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sqlDB, err := db.Open(ctx, cfg.DatabaseURL, db.PoolConfig{})
	if err != nil {
		log.Error("open database", slog.Any("error", err))
		return 1
	}
	defer func() { _ = sqlDB.Close() }()
	svc := operator.NewService(entstore.New(sqlDB), kp, auth.NewPasswordHasher(1), log, operator.DefaultPolicy)

	switch cmd {
	case "disable":
		if err := svc.Disable(ctx, *email); err != nil {
			log.Error("disable failed", slog.Any("error", err))
			return 1
		}
		fmt.Println("operator disabled; all sessions revoked")
	case "create":
		fmt.Fprint(os.Stderr, "password (12-128 chars, read from stdin): ")
		pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && pw == "" {
			log.Error("read password", slog.Any("error", err))
			return 2
		}
		op, uri, err := svc.Create(ctx, *email, *name, strings.TrimRight(pw, "\r\n"))
		if err != nil {
			log.Error("create failed", slog.Any("error", err))
			return 1
		}
		fmt.Printf("operator %s created.\n\nAdd this to an authenticator app now; it is shown only once:\n\n%s\n", op.Email, uri)
	}
	return 0
}
