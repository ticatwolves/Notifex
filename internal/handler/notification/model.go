package notification

type Notification struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Message   string `json:"message"`
	ChannelID string `json:"channel_id"`
	SentAt    string `json:"sent_at"`
}
