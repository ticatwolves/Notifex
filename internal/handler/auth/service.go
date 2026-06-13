package auth

import (
	"context"
	"log"
	"notifex/ent"
)

type AuthService struct {
	client *ent.Client
}

func NewAuthService(client *ent.Client) *AuthService {
	return &AuthService{client: client}
}

func (h *AuthService) Login(authData *LoginRequest) *LoginResponse {
	return &LoginResponse{
		AccessToken:  "mock-access-token",
		RefreshToken: "mock-refresh-token",
	}
}

func (h *AuthService) Register(registerData *RegisterRequest) *RegisterResponse {
	ctx := context.Background()
	u, err := h.client.User.
		Create().
		SetEmail(registerData.Email).
		SetPasswordHash(registerData.Password).
		SetFullName(registerData.Name).
		Save(ctx)
	if err != nil {
		log.Println(err)
		return nil
	}
	log.Println("user was created: ", u)
	return &RegisterResponse{
		Message: "User registered successfully",
	}
}

func Logout() {

}
