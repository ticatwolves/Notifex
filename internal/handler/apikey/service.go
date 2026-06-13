package apikey

import (
	"context"
	"notifex/ent"
	"notifex/ent/apikey"

	"github.com/google/uuid"
)

type APIKeyService struct {
	client *ent.Client
}

func NewAPIKeyService(client *ent.Client) *APIKeyService {
	return &APIKeyService{client: client}
}

func getEnv(env string) apikey.Environment {
	if env == "production" {
		return apikey.EnvironmentLive
	}
	return apikey.EnvironmentTest
}

func setKeyPrefix(c *APIKeyRequest) {
	if c.Environment == "production" {
		c.KeyPrefix = "ntk_live_*********"
	} else {
		c.KeyPrefix = "ntk_test_*********"
	}
}

func generateHash() string {
	return "hashed key"
}

func (s *APIKeyService) CreateAPIKey(apikeyBody *APIKeyRequest) *ent.APIKey {
	ctx := context.Background()
	setKeyPrefix(apikeyBody)
	apikey, err := s.client.APIKey.Create().
		SetAppID(apikeyBody.AppID).
		SetName(apikeyBody.Name).
		SetKeyPrefix(apikeyBody.KeyPrefix).
		SetKeyHash(generateHash()).
		SetEnvironment(getEnv(apikeyBody.Environment)).
		SetScopes(apikeyBody.Scopes).
		SetExpiresAt(apikeyBody.ExpiresAt).
		SetActive(apikeyBody.Active).
		Save(ctx)
	if err != nil {
		println(err.Error())
		return nil
	}
	return apikey
}

func (s *APIKeyService) GetAPIKeys(appID uuid.UUID) []*ent.APIKey {
	ctx := context.Background()
	apikeys, err := s.client.APIKey.Query().Where(apikey.AppID(appID)).All(ctx)
	if err != nil {
		println(err.Error())
		return nil
	}
	return apikeys
}

func (s *APIKeyService) GetAPIKeyByID(keyID uuid.UUID) *ent.APIKey {
	ctx := context.Background()
	apikey, err := s.client.APIKey.Query().Where(apikey.ID(keyID)).Only(ctx)
	if err != nil {
		println(err.Error())
		return nil
	}
	return apikey
}

func (s *APIKeyService) DeleteAPIKey(keyID uuid.UUID) error {
	ctx := context.Background()
	err := s.client.APIKey.DeleteOneID(keyID).Exec(ctx)
	if err != nil {
		println(err.Error())
		return err
	}
	return nil
}
