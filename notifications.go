package main

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

var errNotificationNotFound = errors.New("notification not found")

// Notification is an in-hub message for one account, delivered by polling
// (GET /v1/me/notifications) rather than true push. Real APNs/FCM delivery
// needs push credentials this deployment doesn't have configured, so
// PushToken below only registers the device's token for when that's added;
// today's actual delivery mechanism is this feed. A Veyra client can poll
// it on a timer or when foregrounded, same as any other sync document.
type Notification struct {
	ID        string     `json:"id"`
	UserID    string     `json:"userID"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	CreatedAt time.Time  `json:"createdAt"`
	ReadAt    *time.Time `json:"readAt,omitempty"`
}

// PushToken is a device's registered push token, stored for future use once
// real push delivery (APNs) is wired up. Registering a token today has no
// visible effect beyond being stored; notifications still arrive via the
// poll-based feed.
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
