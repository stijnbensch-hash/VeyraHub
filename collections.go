package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
)

// SmartCollection is an admin-defined virtual library: instead of one
// addon's fixed catalog, its contents are computed on every request by
// pulling matching items from every enabled catalog of the given media
// type across all addons, and filtering them by the collection's own
// rules. It appears in the Jellyfin bridge and native API exactly like an
// addon-backed library, so Veyra needs no special handling for it.
type SmartCollection struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	MediaType string    `json:"mediaType"`
	Genre     string    `json:"genre,omitempty"`
	MinYear   int       `json:"minYear,omitempty"`
	MaxYear   int       `json:"maxYear,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func (c SmartCollection) matches(genres []string, year int) bool {
	if c.Genre != "" {
		found := false
		for _, genre := range genres {
			if strings.EqualFold(genre, c.Genre) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if c.MinYear > 0 && year > 0 && year < c.MinYear {
		return false
	}
	if c.MaxYear > 0 && year > 0 && year > c.MaxYear {
		return false
	}
	return true
}

func (s *Store) SmartCollections() []SmartCollection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]SmartCollection{}, s.state.SmartCollections...)
}

func (s *Store) SmartCollectionByID(id string) (SmartCollection, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, collection := range s.state.SmartCollections {
		if collection.ID == id {
			return collection, true
		}
	}
	return SmartCollection{}, false
}

func (s *Store) CreateSmartCollection(collection SmartCollection) (SmartCollection, error) {
	collection.ID = randomID()
	collection.CreatedAt = time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.SmartCollections = append(s.state.SmartCollections, collection)
	if err := s.persistLocked(); err != nil {
		return SmartCollection{}, err
	}
	return collection, nil
}

func (s *Store) DeleteSmartCollection(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.SmartCollections {
		if s.state.SmartCollections[i].ID == id {
			s.state.SmartCollections = append(s.state.SmartCollections[:i], s.state.SmartCollections[i+1:]...)
			return s.persistLocked()
		}
	}
	return os.ErrNotExist
}

func (h *Hub) registerCollectionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/collections", h.requireSession(h.listCollections))
	mux.HandleFunc("POST /v1/collections", h.requireAdmin(h.createCollection))
	mux.HandleFunc("DELETE /v1/collections/{id}", h.requireAdmin(h.deleteCollection))
	mux.HandleFunc("GET /api/v1/collections/{id}/items", h.requireSession(h.nativeCollectionItems))
}

func (h *Hub) listCollections(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"collections": h.store.SmartCollections()})
}

func (h *Hub) createCollection(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name      string `json:"name"`
		MediaType string `json:"mediaType"`
		Genre     string `json:"genre"`
		MinYear   int    `json:"minYear"`
		MaxYear   int    `json:"maxYear"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "Ongeldige aanvraag.")
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "Geef de collectie een naam.")
		return
	}
	if !isMediaType(input.MediaType) {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype.")
		return
	}
	collection, err := h.store.CreateSmartCollection(SmartCollection{
		Name: name, MediaType: input.MediaType, Genre: strings.TrimSpace(input.Genre),
		MinYear: input.MinYear, MaxYear: input.MaxYear,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "De collectie kon niet worden opgeslagen.")
		return
	}
	writeJSON(w, http.StatusCreated, collection)
}

func (h *Hub) deleteCollection(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteSmartCollection(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "Collectie niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// smartCollectionMetas gathers every addon-catalog meta of the collection's
// media type and keeps only the ones matching its rules.
func (h *Hub) smartCollectionMetas(ctx context.Context, collection SmartCollection) []stremioMeta {
	var result []stremioMeta
	for _, addon := range h.addonCatalog() {
		if !addon.Enabled || !contains(addon.Resources, "catalog") {
			continue
		}
		for _, catalog := range addon.Catalogs {
			if catalog.Type != collection.MediaType {
				continue
			}
			metas, err := h.fetchCatalog(ctx, addon, catalog, "", 0)
			if err != nil {
				continue
			}
			for _, meta := range metas {
				if meta.ID == "" || !isMediaType(meta.Type) {
					continue
				}
				if collection.matches(meta.Genres, metaYear(meta)) {
					result = append(result, meta)
				}
			}
		}
	}
	return result
}

func (h *Hub) nativeCollectionItems(w http.ResponseWriter, r *http.Request) {
	collection, ok := h.store.SmartCollectionByID(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Collectie niet gevonden.")
		return
	}
	metas := h.smartCollectionMetas(r.Context(), collection)
	items := mediaItemsFromMetas("", metas)
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

// jellyfinSmartCollectionItems is the Jellyfin-bridge counterpart of
// nativeCollectionItems: same underlying metas, translated to Jellyfin
// item shape and additionally passed through the requesting viewer's own
// content filter (a smart collection is admin-curated; a viewer's personal
// exclusions still apply on top of it).
func (h *Hub) jellyfinSmartCollectionItems(ctx context.Context, collectionID string, filter ContentFilter) []map[string]any {
	collection, ok := h.store.SmartCollectionByID(collectionID)
	if !ok {
		return nil
	}
	metas := filterMetas(h.smartCollectionMetas(ctx, collection), filter)

	// Each meta may have come from a different addon; the AddonID isn't
	// tracked per-meta by smartCollectionMetas, so items resolve their
	// stream/meta source generically (empty preferred addon) the same way
	// search results already do.
	return h.jellyfinItemsFromMetas("", metas)
}
