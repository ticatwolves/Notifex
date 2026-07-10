package notification

type SendRequest struct {
	NotificationID string      `json:"notification_id"`
	Payload        interface{} `json:"payload"`
	Context        interface{} `json:"context"`
}

type SendResponse struct {
	RequestID string `json:"request_id"`
}
