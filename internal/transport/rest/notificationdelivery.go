package rest

import (
	"net/http"
	"notifex/internal/domain/notificationdelivery"

	"github.com/gin-gonic/gin"
)

type NotificationDeliveryHandler struct {
	service *notificationdelivery.Service
}

func NewNotificationDeliveryHandler(service *notificationdelivery.Service) *NotificationDeliveryHandler {
	return &NotificationDeliveryHandler{
		service: service,
	}
}

func (h *NotificationDeliveryHandler) Register(router gin.IRouter) {
	apps := router.Group("/notification-delivery")
	{
		apps.GET("", h.List)
	}
}

func (h *NotificationDeliveryHandler) List(c *gin.Context) {
	ctx := c.Request.Context()
	resp, err := h.service.List(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}
