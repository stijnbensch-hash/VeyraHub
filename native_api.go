package main

import (
	"net/http"
	"strconv"
	"strings"
)

// resolveNativeMediaID accepts either a plain lookup id (e.g. the IMDb id
// convention VeyraHubNativeClient uses for addon-catalog items) or a
// hubItemID blob (what an item that came through the Jellyfin bridge
// carries as its id — see encodeHubID/decodeHubID in jellyfin.go) and
// returns the id/type an addon's own stream endpoint actually expects.
//
// Without this, a mediaserver-shelf item without an IMDb id — a live
// sports event from an addon like SeriousSportSync is the case that
// surfaced this — reaches this API with its encoded hubItemID as `id`.
// aggregateStreams forwards that `id` to every addon's stream endpoint
// verbatim, so an addon that only understands its own catalog ids (e.g.
// "nfl:2026-...") gets a base64 blob it can't parse and returns zero
// streams — the addon IS called (it passes the "stream" resource check),
// it just never gets an id it recognizes.
func resolveNativeMediaID(mediaType, id string) (string, string) {
	decoded, err := decodeHubID(id)
	if err != nil || decoded.MediaID == "" {
		return mediaType, id
	}
	resolvedType := mediaType
	if decoded.MediaType != "" {
		resolvedType = decoded.MediaType
	}
	return resolvedType, decoded.MediaID
}

func (h *Hub) registerNativeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/catalogs", h.requireSession(h.nativeCatalogs))
	mux.HandleFunc("GET /api/v1/catalog/{addonID}/{type}/{catalogID}", h.requireSession(h.nativeCatalog))
	mux.HandleFunc("GET /api/v1/search", h.requireSession(h.nativeSearch))
	mux.HandleFunc("GET /api/v1/items/{type}/{id}", h.requireSession(h.nativeItem))
	mux.HandleFunc("GET /api/v1/items/{type}/{id}/streams", h.requireSession(h.nativeStreams))
	mux.HandleFunc("GET /api/v1/items/{type}/{id}/subtitles", h.requireSession(h.nativeSubtitles))
	mux.HandleFunc("GET /api/v1/items/{type}/{id}/progress", h.requireSession(h.nativeGetProgress))
	mux.HandleFunc("PUT /api/v1/items/{type}/{id}/progress", h.requireSession(h.nativePutProgress))
	mux.HandleFunc("DELETE /api/v1/items/{type}/{id}/progress", h.requireSession(h.nativeDeleteProgress))
}

func (h *Hub) nativeCatalogs(w http.ResponseWriter, r *http.Request) {
	values := h.mediaCatalogs()

	addonID := strings.TrimSpace(r.URL.Query().Get("addonID"))
	mediaType := strings.TrimSpace(r.URL.Query().Get("type"))

	if mediaType != "" && !isMediaType(mediaType) {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype.")
		return
	}

	filtered := make([]MediaCatalog, 0, len(values))

	for _, catalog := range values {
		if addonID != "" && catalog.AddonID != addonID {
			continue
		}
		if mediaType != "" && catalog.Type != mediaType {
			continue
		}
		filtered = append(filtered, catalog)
	}

	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			writeError(w, http.StatusBadRequest, "Ongeldige offset.")
			return
		}
		offset = value
	}

	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 500 {
			writeError(w, http.StatusBadRequest, "Limit moet tussen 1 en 500 liggen.")
			return
		}
		limit = value
	}

	total := len(filtered)

	if offset > total {
		offset = total
	}

	end := offset + limit
	if end > total {
		end = total
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"catalogs": filtered[offset:end],
		"count":    end - offset,
		"total":    total,
		"offset":   offset,
		"limit":    limit,
	})
}

func (h *Hub) nativeCatalog(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")

	if !isMediaType(mediaType) {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype.")
		return
	}

	skip := 0
	if value := strings.TrimSpace(r.URL.Query().Get("skip")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "Ongeldige skip-waarde.")
			return
		}
		skip = parsed
	}

	user, _ := userFromContext(r)
	items, err := h.mediaCatalogItems(
		r.Context(),
		r.PathValue("addonID"),
		mediaType,
		r.PathValue("catalogID"),
		strings.TrimSpace(r.URL.Query().Get("search")),
		skip,
		h.store.ContentFilterForUser(user.ID),
	)
	if err != nil {
		writeError(w, http.StatusNotFound, "Catalogus niet gevonden of tijdelijk niet beschikbaar.")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"count": len(items),
	})
}

func (h *Hub) nativeSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	if query == "" {
		writeError(w, http.StatusBadRequest, "Geef een zoekterm op.")
		return
	}

	user, _ := userFromContext(r)
	items := h.mediaSearch(r.Context(), query, h.store.ContentFilterForUser(user.ID))

	writeJSON(w, http.StatusOK, map[string]any{
		"query": query,
		"items": items,
		"count": len(items),
	})
}

func (h *Hub) nativeItem(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := strings.TrimSpace(r.PathValue("id"))

	if !isMediaType(mediaType) {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype.")
		return
	}

	if id == "" {
		writeError(w, http.StatusBadRequest, "Media-id ontbreekt.")
		return
	}

	mediaType, id = resolveNativeMediaID(mediaType, id)

	item, err := h.mediaMetadata(
		r.Context(),
		strings.TrimSpace(r.URL.Query().Get("addonID")),
		mediaType,
		id,
	)
	if err != nil {
		writeError(w, http.StatusNotFound, "Media-item niet gevonden.")
		return
	}

	writeJSON(w, http.StatusOK, item)
}

func (h *Hub) nativeStreams(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := strings.TrimSpace(r.PathValue("id"))

	if !isMediaType(mediaType) || id == "" {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype of id.")
		return
	}
	if h.profileLimitExceeded(r) {
		writeError(w, http.StatusForbidden, "De dagelijkse kijklimiet van dit profiel is bereikt.")
		return
	}

	mediaType, id = resolveNativeMediaID(mediaType, id)

	values := h.aggregateStreams(r.Context(), mediaType, id)

	writeJSON(w, http.StatusOK, map[string]any{
		"streams": values,
		"count":   len(values),
	})
}

// nativeGetProgress returns the signed-in user's stored resume position for
// one title, if any — read by the client when starting playback so it can
// offer/resume from where the same account left off on any device.
func (h *Hub) nativeGetProgress(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := strings.TrimSpace(r.PathValue("id"))
	if !isMediaType(mediaType) || id == "" {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype of id.")
		return
	}
	mediaType, id = resolveNativeMediaID(mediaType, id)

	user, _ := userFromContext(r)
	progress, ok := h.store.ProgressForUser(user.ID, mediaType, id)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"found":           true,
		"positionSeconds": progress.PositionSeconds,
		"durationSeconds": progress.DurationSeconds,
		"updatedAt":       progress.UpdatedAt,
	})
}

// nativePutProgress records the client's current playback position for one
// title, sent periodically while playing (and once more at the end/on
// stop). The hub never measures playback time itself — this is exactly
// what the client reports.
func (h *Hub) nativePutProgress(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := strings.TrimSpace(r.PathValue("id"))
	if !isMediaType(mediaType) || id == "" {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype of id.")
		return
	}
	mediaType, id = resolveNativeMediaID(mediaType, id)

	var input struct {
		PositionSeconds float64 `json:"positionSeconds"`
		DurationSeconds float64 `json:"durationSeconds"`
	}
	if decodeJSON(r, &input) != nil || input.PositionSeconds < 0 {
		writeError(w, http.StatusBadRequest, "Ongeldige kijkpositie.")
		return
	}

	user, _ := userFromContext(r)
	progress, err := h.store.SetProgressForUser(user.ID, mediaType, id, input.PositionSeconds, input.DurationSeconds)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Kijkpositie opslaan is mislukt.")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"positionSeconds": progress.PositionSeconds,
		"durationSeconds": progress.DurationSeconds,
		"finished":        progress.Finished,
		"updatedAt":       progress.UpdatedAt,
	})
}

// nativeDeleteProgress clears a stored resume position — e.g. the client
// explicitly restarted a title from the beginning rather than resuming.
func (h *Hub) nativeDeleteProgress(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := strings.TrimSpace(r.PathValue("id"))
	if !isMediaType(mediaType) || id == "" {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype of id.")
		return
	}
	mediaType, id = resolveNativeMediaID(mediaType, id)

	user, _ := userFromContext(r)
	if err := h.store.ClearProgressForUser(user.ID, mediaType, id); err != nil {
		writeError(w, http.StatusInternalServerError, "Kijkpositie verwijderen is mislukt.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) nativeSubtitles(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := strings.TrimSpace(r.PathValue("id"))

	if !isMediaType(mediaType) || id == "" {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype of id.")
		return
	}

	mediaType, id = resolveNativeMediaID(mediaType, id)

	values := h.aggregateSubtitles(r.Context(), mediaType, id)

	writeJSON(w, http.StatusOK, map[string]any{
		"subtitles": values,
		"count":     len(values),
	})
}
