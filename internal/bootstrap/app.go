package bootstrap

import (
	"notifex/internal/domain/apikey"
	"notifex/internal/domain/app"
	"notifex/internal/domain/notification"
	"notifex/internal/domain/notificationdelivery"
	"notifex/internal/domain/notificationtemplate"
	"notifex/internal/domain/notificationtemplatecontent"
	"notifex/internal/ent/schema"
)

type App struct {
	DB *schema.Client
	// Will add Redis and Dispatcher later
	// Redis      *redis.Client
	// Dispatcher dispatcher.Dispatcher

	// Domain Services
	AppService                         *app.Service
	APIKeyService                      *apikey.Service
	NotificationTemplateService        *notificationtemplate.Service
	NotificationTemplatecontentService *notificationtemplatecontent.Service
	NotificationService                *notification.Service
	NotificationDeliveryService        *notificationdelivery.Service
}
