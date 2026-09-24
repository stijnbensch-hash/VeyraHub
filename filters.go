package main

import (
	"net/http"
	"strings"
	"time"
)

// ContentFilter is a viewer's own content exclusions, applied wherever the
// hub returns catalog/search results for that account (native API and the
// Jellyfin-compatible bridge alike). Set by the viewer for themselves, not
// by the admin — this is personal taste, not moderation.
type ContentFilter struct {
	UserID         string    `json:"userID"`
	ExcludedGenres []string  `json:"excludedGenres,omitempty"`
	MinYear        int       `json:"minYear,omitempty"`
	MaxYear        int       `json:"maxYear,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt,omitempty"`
}

func (f ContentFilter) isEmpty() bool {
	return len(f.ExcludedGenres) == 0 && f.MinYear == 0 && f.MaxYear == 0
}

// allows reports whether a media item (by genre/year) passes this filter.
// Items with no genre/year information from the addon are never excluded
// on that basis — an addon that doesn't report genres shouldn't have all of
// its content hidden by a genre filter.
func (f ContentFilter) allows(genres []string, year int) bool {
	if f.isEmpty() {
		return true
	}
	if len(f.ExcludedGenres) > 0 && len(genres) > 0 {
		for _, genre := range genres {
			for _, excluded := range f.ExcludedGenres {
				if strings.EqualFold(genre, excluded) {
					return false
				}
			}
		}
	}
	if year > 0 {
		if f.MinYear > 0 && year < f.MinYear {
			return false
		}
		if f.MaxYear > 0 && year > f.MaxYear {
			return false
		}
	}
	return true
}

func (s *Store) contentFilterIndexLocked(userID string) int {
	for i := range s.state.ContentFilters {
		if s.state.ContentFilters[i].UserID == userID {
			return i
		}
	}
	return -1
}

// ContentFilterForUser returns a viewer's filter, or a zero (pass-through)
// filter if they haven't set one.
func (s *Store) ContentFilterForUser(userID string) ContentFilter {
	s.mu.RLock()
	defer s.mu.RUnlock()

	index := s.contentFilterIndexLocked(userID)
	if index < 0 {
		return ContentFilter{UserID: userID}
	}
	return s.state.ContentFilters[index]
}

// SetContentFilterForUser replaces a viewer's filter wholesale (the API
// surface is a single PUT, mirroring VeyraSync's document-replace model).
func (s *Store) SetContentFilterForUser(filter ContentFilter) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	filter.UpdatedAt = time.Now().UTC()

	index := s.contentFilterIndexLocked(filter.UserID)
	if index >= 0 {
		s.state.ContentFilters[index] = filter
	} else {
		s.state.ContentFilters = append(s.state.ContentFilters, filter)
	}
	return s.persistLocked()
}

// filterMetas removes catalog/search results excluded by filter. Used by
// both the native API (core.go) and the Jellyfin-compatible bridge
// (jellyfin.go) so the two surfaces stay consistent.
func filterMetas(metas []stremioMeta, filter ContentFilter) []stremioMeta {
	if filter.isEmpty() {
		return metas
	}
	result := make([]stremioMeta, 0, len(metas))
	for _, meta := range metas {
		if filter.allows(meta.Genres, metaYear(meta)) {
			result = append(result, meta)
		}
	}
	return result
}

// userFromContext resolves the signed-in account for the current request,
// for handlers reached via requireSession or jellyfinProtected (both of
// which attach the user to the request context).
func userFromContext(r *http.Request) (User, bool) {
	user, ok := r.Context().Value(ctxUserKey).(User)
	return user, ok
}

func (h *Hub) registerFilterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/me/filters", h.requireSession(h.getMyFilters))
	mux.HandleFunc("PUT /v1/me/filters", h.requireSession(h.putMyFilters))
}

func (h *Hub) getMyFilters(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r)
	writeJSON(w, http.StatusOK, h.store.ContentFilterForUser(user.ID))
}

func (h *Hub) putMyFilters(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ExcludedGenres []string `json:"excludedGenres"`
		MinYear        int      `json:"minYear"`
		MaxYear        int      `json:"maxYear"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "Ongeldige aanvraag.")
		return
	}
	user, _ := userFromContext(r)
	filter := ContentFilter{
		UserID:         user.ID,
		ExcludedGenres: normalizeGenreList(input.ExcludedGenres),
		MinYear:        input.MinYear,
		MaxYear:        input.MaxYear,
	}
	if err := h.store.SetContentFilterForUser(filter); err != nil {
		writeError(w, http.StatusInternalServerError, "De filters konden niet worden opgeslagen.")
		return
	}
	writeJSON(w, http.StatusOK, filter)
}

func normalizeGenreList(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
