package api

import (
	"notifex/ent"

	"github.com/gin-gonic/gin"
)

func NewRouter(client *ent.Client) *gin.Engine {
	router := gin.Default()
	APIHandlers := NewDBService(client)
	auth := router.Group("/api/v1/auth")
	auth.POST("/login", APIHandlers.Login)
	auth.POST("/register", APIHandlers.Register)
	auth.POST("/logout", APIHandlers.Logout)

	apps := router.Group("/api/v1/apps")
	apps.POST("/", APIHandlers.CreateApp)
	apps.GET("/", APIHandlers.GetApps)

	apikeys := router.Group("/api/v1/api-keys")
	apikeys.POST("/", APIHandlers.CreateAPIKey)
	apikeys.GET("/:app_id", APIHandlers.GetAPIKeys)
	apikeys.DELETE("/:id", APIHandlers.DeleteAPIKey)

	templates := router.Group("/api/v1/templates")
	templates.POST("/", APIHandlers.CreateTemplate)
	templates.GET("/:app_id", APIHandlers.GetTemplates)
	templates.GET("/:app_id/:id", APIHandlers.GetTemplateByID)
	templates.PUT("/:app_id/:id", APIHandlers.UpdateTemplate)
	templates.DELETE("/:app_id/:id", APIHandlers.DeleteTemplate)

	templateContent := router.Group("/api/v1/template-content")
	templateContent.POST("/", APIHandlers.CreateTemplateContent)
	templateContent.GET("/", APIHandlers.GetTemplateContent)
	templateContent.GET("/:id", APIHandlers.GetTemplateContentByID)
	templateContent.PUT("/:id", APIHandlers.UpdateTemplateContent)
	templateContent.DELETE("/:id", APIHandlers.DeleteTemplateContent)

	channels := router.Group("/api/v1/channels")
	channels.GET("/", APIHandlers.GetChannels)
	channels.POST("/:id", APIHandlers.ConfigureChannel)
	channels.PUT("/:id", APIHandlers.UpdateChannel)
	channels.DELETE("/:id", APIHandlers.DeleteChannel)

	notifications := router.Group("/api/v1/notifications")
	notifications.POST("/", APIHandlers.CreateNotification)
	notifications.GET("/", APIHandlers.GetNotifications)
	notifications.GET("/:id", APIHandlers.GetNotificationByID)
	notifications.PUT("/:id", APIHandlers.UpdateNotification)
	notifications.DELETE("/:id", APIHandlers.DeleteNotification)

	return router
}
