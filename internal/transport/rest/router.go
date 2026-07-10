package rest

import (
	"notifex/internal/bootstrap"

	"github.com/gin-gonic/gin"
)

func RegisterRoutes(router *gin.Engine, app *bootstrap.App) {
	v1 := router.Group("/api/v1")
	NewAppHandler(app.AppService).Register(v1)
	NewAPIKeyHandler(app.APIKeyService).Register(v1)
	NewTemplateHandler(app.NotificationTemplateService).Register(v1)
	NewTemplateContentHandler(app.NotificationTemplatecontentService).Register(v1)
	NewNotificationHandler(app.NotificationService).Register(v1)
	NewNotificationDeliveryHandler(app.NotificationDeliveryService).Register(v1)
}
