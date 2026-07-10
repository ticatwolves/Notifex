package rest

import (
	"net/http"
	"notifex/internal/domain/apikey"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type APIKeyHandler struct {
	service *apikey.Service
}

func NewAPIKeyHandler(service *apikey.Service) *APIKeyHandler {
	return &APIKeyHandler{
		service: service,
	}
}

func (h *APIKeyHandler) Register(router gin.IRouter) {
	apps := router.Group("/api-keys")
	{
		apps.POST("", h.Create)
		apps.GET("", h.List)
		apps.GET("/:id", h.Get)
		apps.PUT("/:id", h.Update)
		apps.DELETE("/:id", h.Delete)
	}
}

func (h *APIKeyHandler) Create(c *gin.Context) {
	ctx := c.Request.Context()
	var req apikey.APIKeyRequest
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

func (h *APIKeyHandler) List(c *gin.Context) {
	ctx := c.Request.Context()
	appID := c.Query("app_id")
	if appID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing app_id parameter"})
		return
	}
	resp, err := h.service.List(ctx, uuid.MustParse(appID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *APIKeyHandler) Get(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	resp, err := h.service.Get(ctx, uuid.MustParse(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *APIKeyHandler) Update(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	var req apikey.APIKeyRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	resp, err := h.service.Update(ctx, uuid.MustParse(id), req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *APIKeyHandler) Delete(c *gin.Context) {
	ctx := c.Request.Context()
	id := c.Param("id")
	err := h.service.Delete(ctx, uuid.MustParse(id))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "API key deleted successfully"})
}
