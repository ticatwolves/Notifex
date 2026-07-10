package rest

import (
	"net/http"
	"notifex/internal/domain/app"

	"github.com/gin-gonic/gin"
)

type AppHandler struct {
	service *app.Service
}

func NewAppHandler(service *app.Service) *AppHandler {
	return &AppHandler{
		service: service,
	}
}

func (h *AppHandler) Register(router gin.IRouter) {
	apps := router.Group("/apps")
	{
		apps.POST("", h.Create)
		apps.GET("", h.List)
		apps.GET("/:slug", h.Get)
		apps.PUT("/:slug", h.Update)
		apps.DELETE("/:slug", h.Delete)
	}
}

func (h *AppHandler) Create(c *gin.Context) {
	ctx := c.Request.Context()
	var req app.AppCreateRequest
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

func (h *AppHandler) List(c *gin.Context) {
	ctx := c.Request.Context()
	resp, err := h.service.List(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *AppHandler) Get(c *gin.Context) {
	ctx := c.Request.Context()
	slug := c.Param("slug")
	resp, err := h.service.Get(ctx, slug)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *AppHandler) Update(c *gin.Context) {
	ctx := c.Request.Context()
	slug := c.Param("slug")
	var req app.AppUpdateRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	resp, err := h.service.Update(ctx, slug, req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *AppHandler) Delete(c *gin.Context) {
	ctx := c.Request.Context()
	slug := c.Param("slug")
	h.service.Delete(ctx, slug)
	c.JSON(http.StatusOK, gin.H{"message": "delete"})
}
