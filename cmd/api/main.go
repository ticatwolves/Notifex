package api

import (
	"notifex/config"
	"notifex/ent"
	"notifex/internal/store/postgres"
)

type DBService struct {
	client *ent.Client
}

func NewDBService(client *ent.Client) *DBService {
	return &DBService{client: client}
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	client := postgres.DBClint(cfg.DatabaseURL)
	router := NewRouter(client)
	router.Run(":" + cfg.Port)
}
