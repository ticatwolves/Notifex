package postgres

import (
	"context"
	"log"
	"notifex/ent"
	"notifex/ent/migrate"

	_ "github.com/lib/pq" // <-- Registers the "postgres" driver name
)

func DBClint(databaseURL string) *ent.Client {
	client, err := ent.Open("postgres", databaseURL)
	if err != nil {
		log.Fatalf("failed opening connection to postgres: %v", err)
	}
	// defer client.Close()
	if err := client.Schema.Create(
		context.Background(),
		migrate.WithDropColumn(true),
		migrate.WithDropIndex(true)); err != nil {
		log.Fatalf("failed creating schema resources: %v", err)
	}
	return client
}
