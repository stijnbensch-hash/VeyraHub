package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type Hub struct {
	store    *Store
	username string
	token    string
	client   *http.Client
}

type addonManifest struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Version     string          `json:"version"`
	Resources   json.RawMessage `json:"resources"`
	Catalogs    []struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Name  string `json:"name"`
		Extra []struct {
			Name string `json:"name"`
		} `json:"extra"`
	} `json:"catalogs"`
}

type streamResponse struct {
	Streams []json.RawMessage `json:"streams"`
}

type HubStream struct {
	AddonID     string `json:"addonID"`
	AddonName   string `json:"addonName"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url"`
	Filename    string `json:"filename,omitempty"`
	VideoSize   int64  `json:"videoSize,omitempty"`
}

func NewHub(store *Store, username, token string, client *http.Client) *Hub {
	if client == nil {
		client = &http.Client{Timeout: 25 * time.Second}
	}
	return &Hub{store: store, username: username, token: token, client: client}
}

func (h *Hub) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.index)
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("GET /v1/status", h.auth(h.status))
	mux.HandleFunc("GET /v1/addons", h.auth(h.listAddons))
	mux.HandleFunc("POST /v1/addons", h.auth(h.addAddon))
	mux.HandleFunc("PATCH /v1/addons/{id}", h.auth(h.patchAddon))
	mux.HandleFunc("POST /v1/addons/{id}/move", h.auth(h.moveAddon))
	mux.HandleFunc("DELETE /v1/addons/{id}", h.auth(h.deleteAddon))
	mux.HandleFunc("GET /v1/streams/{type}/{id}", h.auth(h.streams))
	h.registerJellyfinRoutes(mux)
	return securityHeaders(mux)
}

func (h *Hub) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if len(provided) != len(h.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(h.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "Een geldig API-token is vereist.")
			return
		}
		next(w, r)
	}
}

func (h *Hub) index(w http.ResponseWriter, r *http.Request) {
	data, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Beheerpagina ontbreekt.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (h *Hub) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "Veyra Hub"})
}

func (h *Hub) status(w http.ResponseWriter, r *http.Request) {
	state := h.store.Snapshot()
	enabled := 0
	for _, addon := range state.Addons {
		if addon.Enabled {
			enabled++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "Veyra Hub", "version": version, "nodeID": state.NodeID,
		"addonCount": len(state.Addons), "enabledAddonCount": enabled,
		"username": h.username, "jellyfinCompatible": true,
	})
}

func (h *Hub) listAddons(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"addons": h.store.Snapshot().Addons})
}

func (h *Hub) addAddon(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ManifestURL string `json:"manifestURL"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "Ongeldige aanvraag.")
		return
	}
	manifestURL, err := validateEndpoint(input.ManifestURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	request, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, manifestURL.String(), nil)
	request.Header.Set("Accept", "application/json")
	response, err := h.client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, "De addonmanifest kon niet worden opgehaald.")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("De addon gaf HTTP-status %d.", response.StatusCode))
		return
	}
	var manifest addonManifest
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&manifest); err != nil {
		writeError(w, http.StatusBadGateway, "De addonmanifest is geen geldige JSON.")
		return
	}
	manifest.ID = strings.TrimSpace(manifest.ID)
	manifest.Name = strings.TrimSpace(manifest.Name)
	resources := resourceNames(manifest.Resources)
	if manifest.ID == "" || manifest.Name == "" || len(resources) == 0 {
		writeError(w, http.StatusBadRequest, "De manifest moet een id, naam en ondersteunde resource bevatten.")
		return
	}
	base := *manifestURL
	base.Path = strings.TrimSuffix(base.Path, "/manifest.json")
	base.RawQuery = ""
	base.Fragment = ""
	catalogs := make([]AddonCatalog, 0, len(manifest.Catalogs))
	for _, value := range manifest.Catalogs {
		if value.ID == "" || (value.Type != "movie" && value.Type != "series") {
			continue
		}
		extra := make([]string, 0, len(value.Extra))
		for _, option := range value.Extra {
			if option.Name != "" {
				extra = append(extra, option.Name)
			}
		}
		name := strings.TrimSpace(value.Name)
		if name == "" {
			name = manifest.Name
		}
		catalogs = append(catalogs, AddonCatalog{ID: value.ID, Type: value.Type, Name: name, Extra: extra})
	}
	addon := Addon{
		ID: manifest.ID, Name: manifest.Name, Description: strings.TrimSpace(manifest.Description),
		Version: strings.TrimSpace(manifest.Version), ManifestURL: manifestURL.String(),
		BaseURL: strings.TrimSuffix(base.String(), "/"), Resources: resources, Catalogs: catalogs,
		Enabled: true, AddedAt: time.Now().UTC(),
	}
	if err := h.store.PutAddon(addon); err != nil {
		writeError(w, http.StatusInternalServerError, "De addon kon niet worden opgeslagen.")
		return
	}
	writeJSON(w, http.StatusCreated, addon)
}

func (h *Hub) patchAddon(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &input); err != nil || input.Enabled == nil {
		writeError(w, http.StatusBadRequest, "Geef enabled op.")
		return
	}
	if err := h.store.SetEnabled(r.PathValue("id"), *input.Enabled); err != nil {
		writeError(w, http.StatusNotFound, "Addon niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) deleteAddon(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteAddon(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "Addon niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) moveAddon(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Direction int `json:"direction"`
	}
	if decodeJSON(r, &input) != nil || (input.Direction != -1 && input.Direction != 1) {
		writeError(w, http.StatusBadRequest, "Gebruik richting -1 of 1.")
		return
	}
	if err := h.store.MoveAddon(r.PathValue("id"), input.Direction); err != nil {
		writeError(w, http.StatusNotFound, "Addon niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) streams(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := r.PathValue("id")
	if (mediaType != "movie" && mediaType != "series") || id == "" {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype of id.")
		return
	}

	values := h.aggregateStreams(r.Context(), mediaType, id)
	writeJSON(w, http.StatusOK, map[string]any{"streams": values})
}

func (h *Hub) aggregateStreams(ctx context.Context, mediaType, id string) []HubStream {
	state := h.store.Snapshot()
	var enabled []Addon
	for _, addon := range state.Addons {
		if addon.Enabled {
			enabled = append(enabled, addon)
		}
	}

	type result struct {
		streams []HubStream
		err     error
	}
	results := make(chan result, len(enabled))
	var wg sync.WaitGroup
	for _, addon := range enabled {
		wg.Add(1)
		go func(addon Addon) {
			defer wg.Done()
			values, err := h.fetchStreams(ctx, addon, mediaType, id)
			results <- result{values, err}
		}(addon)
	}
	go func() { wg.Wait(); close(results) }()

	var values []HubStream
	for result := range results {
		if result.err != nil {
			slog.Warn("addon stream request failed", "error", result.err)
			continue
		}
		values = append(values, result.streams...)
	}
	values = deduplicateStreams(values)
	rank := map[string]int{}
	for index, addon := range state.Addons {
		rank[addon.ID] = index
	}
	sort.SliceStable(values, func(i, j int) bool { return rank[values[i].AddonID] < rank[values[j].AddonID] })
	return values
}

func (h *Hub) fetchStreams(ctx context.Context, addon Addon, mediaType, id string) ([]HubStream, error) {
	base, err := url.Parse(addon.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = path.Join(base.Path, "stream", mediaType, id+".json")
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	request.Header.Set("Accept", "application/json")
	response, err := h.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, fmt.Errorf("%s returned HTTP %d", addon.Name, response.StatusCode)
	}
	var payload streamResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	values := make([]HubStream, 0, len(payload.Streams))
	for _, raw := range payload.Streams {
		var source struct {
			Name, Title, Description, URL string
			BehaviorHints                 struct {
				Filename  string `json:"filename"`
				VideoSize int64  `json:"videoSize"`
			} `json:"behaviorHints"`
		}
		if json.Unmarshal(raw, &source) != nil {
			continue
		}
		streamURL, err := url.Parse(strings.TrimSpace(source.URL))
		if err != nil || (streamURL.Scheme != "http" && streamURL.Scheme != "https") || streamURL.Host == "" {
			continue
		}
		values = append(values, HubStream{
			AddonID: addon.ID, AddonName: addon.Name, Name: source.Name, Title: source.Title,
			Description: source.Description, URL: streamURL.String(), Filename: source.BehaviorHints.Filename,
			VideoSize: source.BehaviorHints.VideoSize,
		})
	}
	return values, nil
}

func validateEndpoint(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("Gebruik een geldige HTTP(S)-manifest-URL.")
	}
	if parsed.User != nil {
		return nil, errors.New("Inloggegevens horen niet in de manifest-URL.")
	}
	if !strings.HasSuffix(strings.ToLower(parsed.Path), "/manifest.json") {
		return nil, errors.New("De URL moet eindigen op manifest.json.")
	}
	if parsed.Scheme == "http" && !isPrivateHost(parsed.Hostname()) {
		return nil, errors.New("Publieke addons moeten HTTPS gebruiken; HTTP is alleen toegestaan op je privénetwerk.")
	}
	return parsed, nil
}

func isPrivateHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback())
}

func supportsStreams(raw json.RawMessage) bool {
	for _, name := range resourceNames(raw) {
		if name == "stream" {
			return true
		}
	}
	return false
}

func resourceNames(raw json.RawMessage) []string {
	seen := map[string]bool{}
	var result []string
	var names []string
	if json.Unmarshal(raw, &names) == nil {
		for _, name := range names {
			if !seen[name] {
				seen[name] = true
				result = append(result, name)
			}
		}
	}
	var values []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &values) == nil {
		for _, value := range values {
			if value.Name != "" && !seen[value.Name] {
				seen[value.Name] = true
				result = append(result, value.Name)
			}
		}
	}
	return result
}

func deduplicateStreams(values []HubStream) []HubStream {
	seen := map[string]bool{}
	result := make([]HubStream, 0, len(values))
	for _, value := range values {
		if seen[value.URL] {
			continue
		}
		seen[value.URL] = true
		result = append(result, value)
	}
	return result
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
