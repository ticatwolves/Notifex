package trigger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/store"
)

// RunView is a run with its notifications' current state.
type RunView struct {
	Run           store.Run
	Notifications []Outcome
}

// GetRun returns a run of the caller's application environment.
func (s *Service) GetRun(ctx context.Context, p auth.Principal, id uuid.UUID) (RunView, error) {
	run, err := s.st.Runs().GetForEnv(ctx, p.TenantID, p.ApplicationID, p.EnvironmentID, id)
	if err != nil {
		return RunView{}, fmt.Errorf("get run: %w", err)
	}
	outs, err := s.outcomes(ctx, p.TenantID, run)
	if err != nil {
		return RunView{}, err
	}
	return RunView{Run: run, Notifications: outs}, nil
}

// NotificationView is a notification's metadata and timeline. It never
// includes the payload, raw recipient or rendered content.
type NotificationView struct {
	Notification store.Notification
	Events       []store.NotificationEvent
}

// GetNotification returns a notification of the caller's application
// environment with its timeline.
func (s *Service) GetNotification(ctx context.Context, p auth.Principal, id uuid.UUID) (NotificationView, error) {
	n, err := s.st.Notifications().Get(ctx, p.TenantID, id)
	if err != nil {
		return NotificationView{}, fmt.Errorf("get notification: %w", err)
	}
	if n.ApplicationID != p.ApplicationID || n.EnvironmentID != p.EnvironmentID {
		return NotificationView{}, fmt.Errorf("get notification: %w", store.ErrNotFound)
	}
	evs, err := s.st.Events().List(ctx, p.TenantID, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return NotificationView{}, fmt.Errorf("get notification: %w", err)
	}
	// The view never carries ciphertext either.
	n.Recipient, n.RenderedContent, n.VendorResponse, n.RecipientHash = nil, nil, nil, nil
	return NotificationView{Notification: n, Events: evs}, nil
}
