package api

import (
	"net/http"

	"notifex/internal/handler/auth"

	"github.com/gin-gonic/gin"
)

func (h *DBService) Register(c *gin.Context) {
	var registerBody auth.RegisterRequest
	if err := c.ShouldBindJSON(&registerBody); err != nil || registerBody.Email == "" || registerBody.Password == "" || registerBody.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	registerService := auth.NewAuthService(h.client)
	registerResponse := registerService.Register(&registerBody)

	c.JSON(http.StatusOK, gin.H{"data": registerResponse})
}

func (h *DBService) Login(c *gin.Context) {
	var loginBody auth.LoginRequest
	if err := c.BindJSON(&loginBody); err != nil || loginBody.Email == "" || loginBody.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	loginService := auth.NewAuthService(h.client)
	loginResponse := loginService.Login(&loginBody)
	c.JSON(http.StatusOK, gin.H{"data": loginResponse})
}

func (h *DBService) Logout(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"method": "Logout"})
}
