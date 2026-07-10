package bootstrap

import (
	"fmt"
	"notifex/config"

	"notifex/internal/domain/apikey"
	"notifex/internal/domain/app"
	"notifex/internal/domain/notification"
	"notifex/internal/domain/notificationdelivery"
	"notifex/internal/domain/notificationtemplate"
	"notifex/internal/domain/notificationtemplatecontent"

	"notifex/internal/store/postgres"
)

func New(cfg *config.Config) (*App, error) {

	db, err := postgres.DBClint(cfg.DatabaseURL)
	if err != nil {
		fmt.Println("Error connecting to the database:", err)
		return nil, err
	}

	// Will add Redis and Dispatcher later
	// Redis
	// redisClient := redis.New(cfg.Redis)

	// Dispatcher
	// dispatcher := dispatcher.New(redisClient)

	// Services
	appService := app.New(db)
	apiKeyService := apikey.New(db)
	notificationTemplateService := notificationtemplate.New(db)
	notificationTemplatecontentService := notificationtemplatecontent.New(db)
	notificationDeliveryService := notificationdelivery.New(db)
	notificationService := notification.New(
		db,
		// templateService,
		// contentService,
		// dispatcher,
	)

	return &App{
		DB: db,
		// Redis:      redisClient,
		// Dispatcher: dispatcher,

		AppService:                         appService,
		APIKeyService:                      apiKeyService,
		NotificationTemplateService:        notificationTemplateService,
		NotificationTemplatecontentService: notificationTemplatecontentService,
		NotificationService:                notificationService,
		NotificationDeliveryService:        notificationDeliveryService,
	}, nil
}
