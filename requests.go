package main

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// MediaRequest is a viewer asking for a title that no enabled addon found.
// The admin sees every request across all viewers and resolves it manually
// (there's no automatic addon search here — that's what catalogs/search
// already do); resolving one notifies the requester via the notification
// feed.
type MediaRequest struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userID"`
	Query      string     `json:"query"`
	Status     string     `json:"status"` // "open", "resolved", "declined"
	Note       string     `json:"note,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

const (
	requestStatusOpen     = "open"
	requestStatusResolved = "resolved"
	requestStatusDeclined = "declined"
)

func isRequestStatus(value string) bool {
	return value == requestStatusOpen || value == requestStatusResolved || value == requestStatusDeclined
}

func (s *Store) CreateRequest(userID, query string) (MediaRequest, error) {
	request := MediaRequest{
		ID: randomID(), UserID: userID, Query: query, Status: requestStatusOpen, CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Requests = append(s.state.Requests, request)
	if err := s.persistLocked(); err != nil {
		return MediaRequest{}, err
	}
	return request, nil
}

func (s *Store) RequestsForUser(userID string) []MediaRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []MediaRequest
	for _, request := range s.state.Requests {
		if request.UserID == userID {
			result = append(result, request)
		}
	}
	return result
}

func (s *Store) AllRequests() []MediaRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]MediaRequest{}, s.state.Requests...)
}

// SetRequestStatus updates a request's status/note and returns the updated
// request so the caller (the HTTP handler) can notify the requester.
func (s *Store) SetRequestStatus(id, status, note string) (MediaRequest, error) {
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Requests {
		if s.state.Requests[i].ID == id {
			s.state.Requests[i].Status = status
			s.state.Requests[i].Note = note
			if status != requestStatusOpen {
				s.state.Requests[i].ResolvedAt = &now
			} else {
				s.state.Requests[i].ResolvedAt = nil
			}
			if err := s.persistLocked(); err != nil {
				return MediaRequest{}, err
			}
			return s.state.Requests[i], nil
		}
	}
	return MediaRequest{}, os.ErrNotExist
}

func (s *Store) DeleteRequest(userID, id string, isAdmin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Requests {
		if s.state.Requests[i].ID != id {
			continue
		}
		if !isAdmin && s.state.Requests[i].UserID != userID {
			return os.ErrNotExist
		}
		s.state.Requests = append(s.state.Requests[:i], s.state.Requests[i+1:]...)
		return s.persistLocked()
	}
	return os.ErrNotExist
}

func (h *Hub) registerRequestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/me/requests", h.requireSession(h.listMyRequests))
	mux.HandleFunc("POST /v1/me/requests", h.requireSession(h.createRequest))
	mux.HandleFunc("DELETE /v1/me/requests/{id}", h.requireSession(h.deleteMyRequest))
	mux.HandleFunc("GET /v1/requests", h.requireAdmin(h.listAllRequests))
	mux.HandleFunc("PATCH /v1/requests/{id}", h.requireAdmin(h.patchRequest))
}

func (h *Hub) listMyRequests(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r)
	requests := h.store.RequestsForUser(user.ID)
	if requests == nil {
		requests = []MediaRequest{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": requests})
}

func (h *Hub) createRequest(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Query string `json:"query"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "Ongeldige aanvraag.")
		return
	}
	query := strings.TrimSpace(input.Query)
	if query == "" || len(query) > 200 {
		writeError(w, http.StatusBadRequest, "Geef een titel van 1 tot 200 tekens op.")
		return
	}
	user, _ := userFromContext(r)
	request, err := h.store.CreateRequest(user.ID, query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Het verzoek kon niet worden opgeslagen.")
		return
	}
	writeJSON(w, http.StatusCreated, request)
}

func (h *Hub) deleteMyRequest(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r)
	if err := h.store.DeleteRequest(user.ID, r.PathValue("id"), false); err != nil {
		writeError(w, http.StatusNotFound, "Verzoek niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) listAllRequests(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"requests": h.store.AllRequests()})
}

func (h *Hub) patchRequest(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if decodeJSON(r, &input) != nil || !isRequestStatus(input.Status) {
		writeError(w, http.StatusBadRequest, "Gebruik status open, resolved of declined.")
		return
	}
	request, err := h.store.SetRequestStatus(r.PathValue("id"), input.Status, strings.TrimSpace(input.Note))
	if err != nil {
		writeError(w, http.StatusNotFound, "Verzoek niet gevonden.")
		return
	}
	if request.Status == requestStatusResolved {
		h.store.CreateNotification(request.UserID, "Aanvraag toegevoegd", request.Query+" is toegevoegd.")
	} else if request.Status == requestStatusDeclined {
		h.store.CreateNotification(request.UserID, "Aanvraag afgewezen", request.Query+" kon niet worden toegevoegd.")
	}
	writeJSON(w, http.StatusOK, request)
}
