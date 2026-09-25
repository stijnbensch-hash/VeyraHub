package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

var errNotificationNotFound = errors.New("notification not found")

// Notification is an in-hub message for one account, delivered by polling
// (GET /v1/me/notifications) and, when the hub has APNs credentials
// configured (see apns.go and Hub.notify), also pushed to the account's
// registered devices. Push delivery is best-effort on top of this feed, not
// a replacement for it: a Veyra client can still poll this feed on a timer
// or when foregrounded, same as any other sync document.
type Notification struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userID"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	CreatedAt time.Time  `json:"createdAt"`
	ReadAt    *time.Time `json:"readAt,omitempty"`
}

// PushToken is a device's registered push token, used by Hub.notify to
// deliver a push notification via APNs whenever the hub has push
// credentials configured. Notifications always land in the poll-based feed
// regardless; a registered token only adds push delivery on top.
type PushToken struct {
	UserID    string    `json:"userID"`
	DeviceID  string    `json:"deviceID"`
	Token     string    `json:"token"`
	Platform  string    `json:"platform,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (s *Store) CreateNotification(userID, title, body string) Notification {
	notification := Notification{
		ID: randomID(), UserID: userID, Title: title, Body: body, CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Notifications = append(s.state.Notifications, notification)
	_ = s.persistLocked()
	return notification
}

func (s *Store) NotificationsForUser(userID string) []Notification {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []Notification
	for _, notification := range s.state.Notifications {
		if notification.UserID == userID {
			result = append(result, notification)
		}
	}
	return result
}

func (s *Store) MarkNotificationRead(userID, id string) error {
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Notifications {
		if s.state.Notifications[i].ID == id && s.state.Notifications[i].UserID == userID {
			s.state.Notifications[i].ReadAt = &now
			return s.persistLocked()
		}
	}
	return errNotificationNotFound
}

func (s *Store) UpsertPushToken(token PushToken) error {
	token.UpdatedAt = time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.PushTokens {
		if s.state.PushTokens[i].UserID == token.UserID && s.state.PushTokens[i].DeviceID == token.DeviceID {
			s.state.PushTokens[i] = token
			return s.persistLocked()
		}
	}
	s.state.PushTokens = append(s.state.PushTokens, token)
	return s.persistLocked()
}

func (s *Store) PushTokensForUser(userID string) []PushToken {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []PushToken
	for _, token := range s.state.PushTokens {
		if token.UserID == userID {
			result = append(result, token)
		}
	}
	return result
}

// notify records a notification in the poll-based feed and, when the hub
// has APNs configured (Hub.apns != nil), also attempts to push it to every
// device the user has registered a push token for. Push delivery is
// best-effort: it runs per device in its own goroutine and only ever logs a
// failure, never blocking or failing the caller — the caller's own action
// (an addon health change, a request being resolved, ...) has already
// succeeded by the time notify is called.
func (h *Hub) notify(userID, title, body string) Notification {
	notification := h.store.CreateNotification(userID, title, body)
	if h.apns == nil {
		return notification
	}
	for _, token := range h.store.PushTokensForUser(userID) {
		go h.sendPush(token, title, body)
	}
	return notification
}

func (h *Hub) sendPush(token PushToken, title, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.apns.send(ctx, token.Token, title, body); err != nil {
		slog.Warn("apns delivery failed", "deviceID", token.DeviceID, "error", err)
	}
}

func (h *Hub) registerNotificationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/me/notifications", h.requireSession(h.listMyNotifications))
	mux.HandleFunc("POST /v1/me/notifications/{id}/read", h.requireSession(h.markNotificationRead))
	mux.HandleFunc("POST /v1/me/push-token", h.requireSession(h.registerPushToken))
}

func (h *Hub) listMyNotifications(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r)
	notifications := h.store.NotificationsForUser(user.ID)
	if notifications == nil {
		notifications = []Notification{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": notifications})
}

func (h *Hub) markNotificationRead(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r)
	if err := h.store.MarkNotificationRead(user.ID, r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "Melding niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) registerPushToken(w http.ResponseWriter, r *http.Request) {
	var input struct {
		DeviceID string `json:"deviceId"`
		Token    string `json:"token"`
		Platform string `json:"platform"`
	}
	if decodeJSON(r, &input) != nil || strings.TrimSpace(input.DeviceID) == "" || strings.TrimSpace(input.Token) == "" {
		writeError(w, http.StatusBadRequest, "Geef deviceId en token op.")
		return
	}
	user, _ := userFromContext(r)
	if err := h.store.UpsertPushToken(PushToken{
		UserID: user.ID, DeviceID: strings.TrimSpace(input.DeviceID),
		Token: strings.TrimSpace(input.Token), Platform: strings.TrimSpace(input.Platform),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "Het pushtoken kon niet worden opgeslagen.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
