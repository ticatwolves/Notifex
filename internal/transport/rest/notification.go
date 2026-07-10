package rest

import (
	"net/http"
	"notifex/internal/domain/notification"

	"github.com/gin-gonic/gin"
)

type NotificationHandler struct {
	service *notification.Service
}

func NewNotificationHandler(service *notification.Service) *NotificationHandler {
	return &NotificationHandler{
		service: service,
	}
}

func (h *NotificationHandler) Register(router gin.IRouter) {
	apps := router.Group("/notifications")
	{
		apps.POST("/send", h.Send)
		apps.GET("/:template_id", h.List)
		apps.GET("/status/:id", h.Status)
	}
}

func (h *NotificationHandler) Send(c *gin.Context) {
	ctx := c.Request.Context()
	var req notification.SendRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	resp, err := h.service.Send(ctx, req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *NotificationHandler) List(c *gin.Context) {
	ctx := c.Request.Context()
	resp, err := h.service.List(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *NotificationHandler) Status(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	resp, err := h.service.Status(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}
