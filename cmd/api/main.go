package main

import (
	"log"
	"notifex/config"
	"notifex/internal/bootstrap"
	rest "notifex/internal/transport/rest"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}

	app, err := bootstrap.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	router := gin.Default()

	rest.RegisterRoutes(router, app)

	router.Run(":8001")

}
