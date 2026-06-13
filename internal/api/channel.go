package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *DBService) GetChannels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"method": "GetChannels"})
}

func (h *DBService) UpdateChannel(c *gin.Context) {
	id := c.Param("id")
	c.JSON(http.StatusOK, gin.H{"method": "UpdateChannel", "id": id})
}

func (h *DBService) DeleteChannel(c *gin.Context) {
	id := c.Param("id")
	c.JSON(http.StatusOK, gin.H{"method": "DeleteChannel", "id": id})
}

func (h *DBService) ConfigureChannel(c *gin.Context) {
	id := c.Param("id")
	c.JSON(http.StatusOK, gin.H{"method": "ConfigureChannel", "id": id})
}
