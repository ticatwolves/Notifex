package api

import "notifex/ent"

type DBService struct {
	client *ent.Client
}

func NewDBService(client *ent.Client) *DBService {
	return &DBService{client: client}
}
