package postgres

import (
	"context"
	"fmt"
	"log"
	"notifex/internal/ent/schema"
	"notifex/internal/ent/schema/migrate"

	_ "github.com/lib/pq" // <-- Registers the "postgres" driver name
)

func DBClint(databaseURL string) (*schema.Client, error) {
	client, err := schema.Open("postgres", databaseURL)
	if err != nil {
		log.Fatalf("failed opening connection to postgres: %v", err)
		return nil, fmt.Errorf("open postgres: %w", err)

	}
	// defer client.Close()
	if err := client.Schema.Create(
		context.Background(),
		migrate.WithDropColumn(true),
		migrate.WithDropIndex(true)); err != nil {
		log.Fatalf("failed creating schema resources: %v", err)
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	return client, nil
}
