package main

import (
	"notifex/config"
	"notifex/internal/api"
	"notifex/internal/store/postgres"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	client := postgres.DBClint(cfg.DatabaseURL)
	router := api.NewRouter(client)
	router.Run(":" + cfg.Port)
}
