package api

import (
	"github.com/gin-gonic/gin"
)

func (h *DBService) CreateTemplateContent(c *gin.Context) {
	c.JSON(200, gin.H{"message": "CreateTemplate called"})
}

func (h *DBService) GetTemplateContent(c *gin.Context) {
	c.JSON(200, gin.H{"message": "CreateTemplate called"})
}

func (h *DBService) GetTemplateContentByID(c *gin.Context) {
	c.JSON(200, gin.H{"message": "CreateTemplate called"})
}

func (h *DBService) UpdateTemplateContent(c *gin.Context) {
	c.JSON(200, gin.H{"message": "CreateTemplate called"})
}

func (h *DBService) DeleteTemplateContent(c *gin.Context) {
	c.JSON(200, gin.H{"message": "CreateTemplate called"})
}
