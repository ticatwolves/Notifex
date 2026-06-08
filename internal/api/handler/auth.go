package handler

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

type LoginRequestBody struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string
}

type RegisterRequestBody struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string
	Name     string
}

func Register(c *gin.Context) {
	var registerBody RegisterRequestBody
	if err := c.ShouldBindJSON(&registerBody); err != nil || registerBody.Email == "" || registerBody.Password == "" || registerBody.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	fmt.Printf("Register called with email: %s, password: %s, name: %s\n", registerBody.Email, registerBody.Password, registerBody.Name)
	c.JSON(http.StatusOK, gin.H{"method": "Register"})
}

func Login(c *gin.Context) {
	var loginBody LoginRequestBody
	if err := c.BindJSON(&loginBody); err != nil || loginBody.Email == "" || loginBody.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	fmt.Printf("Login called with email: %s and password: %s\n", loginBody.Email, loginBody.Password)
	c.JSON(http.StatusOK, gin.H{"method": "Login"})
}

func Refresh(c *gin.Context) {
	fmt.Println("Refresh called", c.Request.Body)
	c.JSON(http.StatusOK, gin.H{"method": "Refresh"})
}

func Logout(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"method": "Logout"})
}
