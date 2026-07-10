package rest

import (
	"net/http"
	"notifex/internal/domain/notificationtemplatecontent"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type TemplateContentHandler struct {
	service *notificationtemplatecontent.Service
}

func NewTemplateContentHandler(service *notificationtemplatecontent.Service) *TemplateContentHandler {
	return &TemplateContentHandler{
		service: service,
	}
}

func (h *TemplateContentHandler) Register(router gin.IRouter) {
	apps := router.Group("/templates-content")
	{
		apps.POST("", h.Create)
		apps.GET("", h.List)
		apps.GET("/:id", h.Get)
		apps.PUT("/:id", h.Update)
		apps.DELETE("/:id", h.Delete)
	}
}

func (h *TemplateContentHandler) Create(c *gin.Context) {
	ctx := c.Request.Context()
	var req notificationtemplatecontent.TemplateContentRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	resp, err := h.service.Create(ctx, req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *TemplateContentHandler) List(c *gin.Context) {
	ctx := c.Request.Context()
	resp, err := h.service.List(ctx, uuid.MustParse(c.Query("template_id")))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *TemplateContentHandler) Get(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	resp, err := h.service.Get(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *TemplateContentHandler) Update(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	var req notificationtemplatecontent.TemplateContentRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	resp, err := h.service.Update(ctx, id, req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *TemplateContentHandler) Delete(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.service.Delete(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Template content deleted successfully"})
}
