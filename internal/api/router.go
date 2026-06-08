package api

import (
	"notifex/ent"
	"notifex/internal/api/handler"

	"github.com/gin-gonic/gin"
)

func NewRouter(client *ent.Client) *gin.Engine {
	router := gin.Default()
	auth := router.Group("/api/v1/auth")
	auth.POST("/login", handler.Login)
	auth.POST("/register", handler.Register)
	auth.POST("/refresh", handler.Refresh)
	auth.POST("/logout", handler.Logout)

	return router
}
