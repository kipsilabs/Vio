package handlers

import (
	"github.com/Silo-Server/silo-server/internal/notifications"
)

<<<<<<< HEAD
// webhookResponse is the API view of a webhook. It never includes the
// destination URL (Discord webhook tokens are bearer credentials in the URL
// path) or the signing secret — only url_host for identification.
type webhookResponse struct {
	ID                     string     `json:"id"`
	Name                   string     `json:"name"`
	Type                   string     `json:"type"`
	URLHost                string     `json:"url_host"`
	Enabled                bool       `json:"enabled"`
	NotifyFavorites        bool       `json:"notify_favorites"`
	NotifyWatchlist        bool       `json:"notify_watchlist"`
	NotifyContinueWatching bool       `json:"notify_continue_watching"`
	NotifyNextUp           bool       `json:"notify_next_up"`
	NotifyRequests         bool       `json:"notify_requests"`
	NotifyRatings          bool       `json:"notify_ratings"`
	ConsecutiveFailures    int        `json:"consecutive_failures"`
	DisabledReason         *string    `json:"disabled_reason"`
	LastSuccessAt          *time.Time `json:"last_success_at"`
	LastFailureAt          *time.Time `json:"last_failure_at"`
	LastFailureStatus      *int       `json:"last_failure_status"`
	LastFailureMessage     *string    `json:"last_failure_message"`
	// SigningSecret is present only in create / rotate-secret responses.
	SigningSecret string `json:"signing_secret,omitempty"`
}

func webhookToResponse(hook notifications.Webhook) webhookResponse {
	return webhookResponse{
		ID:                     hook.ID,
		Name:                   hook.Name,
		Type:                   hook.Type,
		URLHost:                hook.URLHost,
		Enabled:                hook.Enabled,
		NotifyFavorites:        hook.NotifyFavorites,
		NotifyWatchlist:        hook.NotifyWatchlist,
		NotifyContinueWatching: hook.NotifyContinueWatching,
		NotifyNextUp:           hook.NotifyNextUp,
		NotifyRequests:         hook.NotifyRequests,
		NotifyRatings:          hook.NotifyRatings,
		ConsecutiveFailures:    hook.ConsecutiveFailures,
		DisabledReason:         hook.DisabledReason,
		LastSuccessAt:          hook.LastSuccessAt,
		LastFailureAt:          hook.LastFailureAt,
		LastFailureStatus:      hook.LastFailureStatus,
		LastFailureMessage:     hook.LastFailureMessage,
	}
}

type webhookRequest struct {
	Name                   *string `json:"name"`
	URL                    *string `json:"url"`
	Type                   *string `json:"type"`
	Enabled                *bool   `json:"enabled"`
	NotifyFavorites        *bool   `json:"notify_favorites"`
	NotifyWatchlist        *bool   `json:"notify_watchlist"`
	NotifyContinueWatching *bool   `json:"notify_continue_watching"`
	NotifyNextUp           *bool   `json:"notify_next_up"`
	NotifyRequests         *bool   `json:"notify_requests"`
	NotifyRatings          *bool   `json:"notify_ratings"`
}

func (r webhookRequest) toInput() notifications.WebhookInput {
	return notifications.WebhookInput{
		Name:                   r.Name,
		URL:                    r.URL,
		Type:                   r.Type,
		Enabled:                r.Enabled,
		NotifyFavorites:        r.NotifyFavorites,
		NotifyWatchlist:        r.NotifyWatchlist,
		NotifyContinueWatching: r.NotifyContinueWatching,
		NotifyNextUp:           r.NotifyNextUp,
		NotifyRequests:         r.NotifyRequests,
		NotifyRatings:          r.NotifyRatings,
	}
}

=======
>>>>>>> upstream/main
func (h *NotificationsHandler) webhooks() *notifications.WebhookService {
	if h == nil || h.system == nil {
		return nil
	}
	return h.system.Webhooks
}
