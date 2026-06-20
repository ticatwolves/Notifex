package api

import (
	"net/http"
	"notifex/internal/handler/apikey"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (h *DBService) CreateAPIKey(c *gin.Context) {
	var apiKeyBody apikey.APIKeyRequest
	if err := c.BindJSON(&apiKeyBody); err != nil {
		print(err.Error())
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	apiKeyService := apikey.NewAPIKeyService(h.client)
	c.JSON(http.StatusOK, gin.H{"data": apiKeyService.CreateAPIKey(&apiKeyBody)})
}

func (h *DBService) GetAPIKeys(c *gin.Context) {
	appId := c.Param("app_id")
	parsedUUID, err := uuid.Parse(appId)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Query param 'app_id' must be a valid UUID"})
		return
	}
	apiKeyService := apikey.NewAPIKeyService(h.client)
	c.JSON(http.StatusOK, gin.H{"data": apiKeyService.GetAPIKeys(parsedUUID)})
}

func (h *DBService) DeleteAPIKey(c *gin.Context) {
	id := c.Param("id")
	parsedUUID, err := uuid.Parse(id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Query param 'app_id' must be a valid UUID"})
		return
	}
	apikeyService := apikey.NewAPIKeyService(h.client)
	apikeyService.DeleteAPIKey(parsedUUID)
	c.JSON(http.StatusOK, gin.H{"data": id})
}
