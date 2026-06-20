package api

import (
	"net/http"
	"notifex/internal/handler/app"

	"github.com/gin-gonic/gin"
)

func (h *DBService) CreateApp(c *gin.Context) {
	var appBody app.AppRequest
	if err := c.BindJSON(&appBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	appService := app.NewAppService(h.client)
	c.JSON(http.StatusOK, gin.H{"data": appService.CreateApp(&appBody)})
}

func (h *DBService) GetApps(c *gin.Context) {
	appService := app.NewAppService(h.client)
	c.JSON(http.StatusOK, gin.H{"data": appService.GetApps()})
}
