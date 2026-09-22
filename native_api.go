package main

import (
	"net/http"
	"strconv"
	"strings"
)

func (h *Hub) registerNativeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/catalogs", h.requireSession(h.nativeCatalogs))
	mux.HandleFunc("GET /api/v1/catalog/{addonID}/{type}/{catalogID}", h.requireSession(h.nativeCatalog))
	mux.HandleFunc("GET /api/v1/search", h.requireSession(h.nativeSearch))
	mux.HandleFunc("GET /api/v1/items/{type}/{id}", h.requireSession(h.nativeItem))
	mux.HandleFunc("GET /api/v1/items/{type}/{id}/streams", h.requireSession(h.nativeStreams))
	mux.HandleFunc("GET /api/v1/items/{type}/{id}/subtitles", h.requireSession(h.nativeSubtitles))
}

func (h *Hub) nativeCatalogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"catalogs": h.mediaCatalogs(),
	})
}

func (h *Hub) nativeCatalog(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	if mediaType != "movie" && mediaType != "series" {
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

	items, err := h.mediaCatalogItems(
		r.Context(),
		r.PathValue("addonID"),
		mediaType,
		r.PathValue("catalogID"),
		strings.TrimSpace(r.URL.Query().Get("search")),
		skip,
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

	items := h.mediaSearch(r.Context(), query)
	writeJSON(w, http.StatusOK, map[string]any{
		"query": query,
		"items": items,
		"count": len(items),
	})
}

func (h *Hub) nativeItem(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := strings.TrimSpace(r.PathValue("id"))

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

	if (mediaType != "movie" && mediaType != "series") || id == "" {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype of id.")
		return
	}

	values := h.aggregateStreams(r.Context(), mediaType, id)
	writeJSON(w, http.StatusOK, map[string]any{
		"streams": values,
		"count":   len(values),
	})
}

func (h *Hub) nativeSubtitles(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := strings.TrimSpace(r.PathValue("id"))

	if (mediaType != "movie" && mediaType != "series") || id == "" {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype of id.")
		return
	}

	values := h.aggregateSubtitles(r.Context(), mediaType, id)
	writeJSON(w, http.StatusOK, map[string]any{
		"subtitles": values,
		"count":     len(values),
	})
}
