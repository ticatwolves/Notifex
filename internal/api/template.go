package api

import (
	"net/http"
	"notifex/internal/handler/template"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (h *DBService) CreateTemplate(c *gin.Context) {
	var templateBody template.TemplateRequest
	if err := c.BindJSON(&templateBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	templateService := template.NewTemplateService(h.client)
	c.JSON(http.StatusOK, gin.H{"data": templateService.CreateTemplate(&templateBody)})
}

func (h *DBService) GetTemplates(c *gin.Context) {
	appId := c.Param("app_id")
	parsedAppID, err := uuid.Parse(appId)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Query param 'app_id' must be a valid UUID"})
		return
	}
	templateService := template.NewTemplateService(h.client)
	c.JSON(http.StatusOK, gin.H{"data": templateService.GetTemplatesByAppID(&parsedAppID)})
}

func (h *DBService) GetTemplateByID(c *gin.Context) {
	templateId := c.Param("id")
	parsedtTemplateId, err := uuid.Parse(templateId)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Query param 'app_id' must be a valid UUID"})
		return
	}
	templateService := template.NewTemplateService(h.client)
	c.JSON(http.StatusOK, gin.H{"data": templateService.GetTemplateByID(parsedtTemplateId)})
}

func (h *DBService) UpdateTemplate(c *gin.Context) {
	var templateBody template.TemplateRequest
	if err := c.BindJSON(&templateBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	templateService := template.NewTemplateService(h.client)
	c.JSON(http.StatusOK, gin.H{"data": templateService.CreateTemplate(&templateBody)})
}

func (h *DBService) DeleteTemplate(c *gin.Context) {
	templateId := c.Param("id")
	parsedtTemplateId, err := uuid.Parse(templateId)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Query param 'app_id' must be a valid UUID"})
		return
	}
	templateService := template.NewTemplateService(h.client)
	templateService.DeleteTemplateByID(parsedtTemplateId)
	c.JSON(http.StatusOK, gin.H{"data": parsedtTemplateId})
}
