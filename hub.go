package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFiles embed.FS

const minPasswordLength = 8

type Hub struct {
	store    *Store
	username string
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
	BehaviorHints struct {
		Configurable          bool `json:"configurable"`
		ConfigurationRequired bool `json:"configurationRequired"`
	} `json:"behaviorHints"`
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

// HubSubtitle is a subtitle track offered by a subtitle-capable addon.
// Subtitles are their own capability (not part of HubStream) so any number
// of subtitle addons can contribute tracks for a title independently of
// which addon resolved the video stream itself.
type HubSubtitle struct {
	AddonID   string `json:"addonID"`
	AddonName string `json:"addonName"`
	Lang      string `json:"lang,omitempty"`
	Name      string `json:"name,omitempty"`
	URL       string `json:"url"`
}

type subtitleResponse struct {
	Subtitles []json.RawMessage `json:"subtitles"`
}

type ctxKey string

const (
	ctxUserKey    ctxKey = "user"
	ctxSessionKey ctxKey = "session"
)

// NewHub wires up the hub's HTTP handlers. bootstrapUsername/bootstrapPassword
// are used once, on first run, to create the initial admin account when the
// store holds no users yet; after that the admin signs in like any other
// account and these values are ignored.
func NewHub(store *Store, bootstrapUsername, bootstrapPassword string, client *http.Client) *Hub {
	if client == nil {
		client = &http.Client{Timeout: 25 * time.Second}
	}
	if !store.HasUsers() {
		if _, err := store.CreateUser(bootstrapUsername, bootstrapPassword, RoleAdmin); err != nil {
			slog.Error("cannot bootstrap admin account", "error", err)
		}
	}
	return &Hub{store: store, username: bootstrapUsername, client: client}
}

func (h *Hub) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.index)
	mux.HandleFunc("GET /health", h.health)
	mux.HandleFunc("POST /v1/auth/login", h.login)
	mux.HandleFunc("POST /v1/auth/refresh", h.refreshToken)
	mux.HandleFunc("POST /v1/auth/logout", h.requireSession(h.logout))
	mux.HandleFunc("GET /v1/status", h.requireAdmin(h.status))
	mux.HandleFunc("GET /v1/sessions", h.requireAdmin(h.listSessions))
	mux.HandleFunc("DELETE /v1/sessions/{id}", h.requireAdmin(h.revokeSession))
	mux.HandleFunc("GET /v1/addons", h.requireAdmin(h.listAddons))
	mux.HandleFunc("POST /v1/addons", h.requireAdmin(h.addAddon))
	mux.HandleFunc("PATCH /v1/addons/{id}", h.requireAdmin(h.patchAddon))
	mux.HandleFunc("POST /v1/addons/{id}/move", h.requireAdmin(h.moveAddon))
	mux.HandleFunc("DELETE /v1/addons/{id}", h.requireAdmin(h.deleteAddon))
	mux.HandleFunc("GET /v1/users", h.requireAdmin(h.listUsers))
	mux.HandleFunc("POST /v1/users", h.requireAdmin(h.addUser))
	mux.HandleFunc("PATCH /v1/users/{id}", h.requireAdmin(h.patchUser))
	mux.HandleFunc("DELETE /v1/users/{id}", h.requireAdmin(h.deleteUser))
	mux.HandleFunc("GET /v1/streams/{type}/{id}", h.requireAdmin(h.streams))
	mux.HandleFunc("GET /v1/subtitles/{type}/{id}", h.requireAdmin(h.subtitles))
	h.registerVeyraSyncRoutes(mux)
	h.registerNativeRoutes(mux)
	h.registerJellyfinRoutes(mux)
	return securityHeaders(mux)
}

// requireSession accepts any signed-in account (admin or viewer) and makes
// the resolved session/user available to the handler via the request context.
func (h *Hub) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		session, user, ok := h.store.SessionByAccessToken(token)
		if !ok {
			writeError(w, http.StatusUnauthorized, "Een geldige sessie is vereist. Log opnieuw in.")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserKey, user)
		ctx = context.WithValue(ctx, ctxSessionKey, session)
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin is like requireSession but additionally requires the admin role,
// for the hub's own management API.
func (h *Hub) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return h.requireSession(func(w http.ResponseWriter, r *http.Request) {
		user, _ := r.Context().Value(ctxUserKey).(User)
		if user.Role != RoleAdmin {
			writeError(w, http.StatusForbidden, "Alleen het beheeraccount mag dit doen.")
			return
		}
		next(w, r)
	})
}

func bearerToken(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

// addonCatalog returns the hub's shared addon catalog: the admin account's
// addon list. Addons are stored per-account in the data model, but only the
// admin manages them, so every consumer (admin dashboard, Jellyfin bridge,
// or native API — whichever account is asking) resolves against this same
// list rather than the requesting account's own, otherwise viewer accounts
// (which never have addons of their own) would see none at all.
func (h *Hub) addonCatalog() []Addon {
	adminID, ok := h.store.AdminUserID()
	if !ok {
		return nil
	}
	return h.store.AddonsForUser(adminID)
}

// recordAddonHealth is the addon-health counterpart of addonCatalog: health
// is always recorded against the admin's copy of the addon, regardless of
// which account's request triggered the call.
func (h *Hub) recordAddonHealth(id string, ok bool, message string) {
	adminID, found := h.store.AdminUserID()
	if !found {
		return
	}
	h.store.RecordAddonHealthForUser(adminID, id, ok, message)
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

// login exchanges a username/password for a fresh access/refresh token pair,
// scoped to a device. This replaces the old static-token login for both the
// admin account and viewer accounts.
func (h *Hub) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		DeviceID   string `json:"deviceId"`
		DeviceName string `json:"deviceName"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "Ongeldige aanvraag.")
		return
	}
	user, ok := h.store.Authenticate(strings.TrimSpace(input.Username), input.Password)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Gebruikersnaam of wachtwoord onjuist.")
		return
	}
	writeSessionResponse(w, h.store, user, input.DeviceID, input.DeviceName)
}

// refreshToken rotates a session's access/refresh tokens without asking for
// the password again, so a device can stay signed in.
func (h *Hub) refreshToken(w http.ResponseWriter, r *http.Request) {
	var input struct {
		RefreshToken string `json:"refreshToken"`
	}
	if decodeJSON(r, &input) != nil || input.RefreshToken == "" {
		writeError(w, http.StatusBadRequest, "Geef refreshToken op.")
		return
	}
	_, user, accessToken, refreshToken, err := h.store.RefreshSession(input.RefreshToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "De sessie is verlopen of ingetrokken. Log opnieuw in.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accessToken": accessToken, "refreshToken": refreshToken,
		"user": publicUser(user),
	})
}

func (h *Hub) logout(w http.ResponseWriter, r *http.Request) {
	session, _ := r.Context().Value(ctxSessionKey).(Session)
	if err := h.store.RevokeSession(session.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "Uitloggen is mislukt.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSessionResponse issues a new session for user/device and writes the
// standard login/refresh response shape.
func writeSessionResponse(w http.ResponseWriter, store *Store, user User, deviceID, deviceName string) {
	session, accessToken, refreshToken, err := store.CreateSession(user.ID, deviceID, deviceName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Aanmelden is mislukt.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accessToken": accessToken, "refreshToken": refreshToken,
		"accessExpiresAt": session.AccessExpiresAt, "refreshExpiresAt": session.RefreshExpiresAt,
		"user": publicUser(user),
	})
}

func publicUser(user User) map[string]any {
	return map[string]any{"id": user.ID, "username": user.Username, "role": user.Role, "enabled": user.Enabled}
}

func (h *Hub) status(w http.ResponseWriter, r *http.Request) {
	state := h.store.Snapshot()
	addons := h.addonCatalog()
	enabled := 0
	for _, addon := range addons {
		if addon.Enabled {
			enabled++
		}
	}
	viewers := 0
	for _, user := range state.Users {
		if user.Role == RoleViewer {
			viewers++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "Veyra Hub", "version": version, "nodeID": state.NodeID,
		"addonCount": len(addons), "enabledAddonCount": enabled,
		"viewerCount": viewers, "username": h.username, "jellyfinCompatible": true,
	})
}

func (h *Hub) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions := h.store.AllSessions()
	values := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		user, _ := h.store.UserByID(session.UserID)
		values = append(values, map[string]any{
			"id": session.ID, "userID": session.UserID, "username": user.Username,
			"deviceID": session.DeviceID, "deviceName": session.DeviceName,
			"createdAt": session.CreatedAt, "accessExpiresAt": session.AccessExpiresAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": values})
}

func (h *Hub) revokeSession(w http.ResponseWriter, r *http.Request) {
	if err := h.store.RevokeSession(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "Sessie niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) listUsers(w http.ResponseWriter, r *http.Request) {
	users := h.store.Snapshot().Users
	values := make([]map[string]any, 0, len(users))
	for _, user := range users {
		if user.Role == RoleAdmin {
			continue
		}
		values = append(values, map[string]any{
			"id": user.ID, "username": user.Username, "enabled": user.Enabled, "createdAt": user.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": values})
}

func (h *Hub) addUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "Ongeldige aanvraag.")
		return
	}
	username := strings.TrimSpace(input.Username)
	if len(username) < 3 || len(username) > 64 || strings.ContainsAny(username, "\r\n\t") {
		writeError(w, http.StatusBadRequest, "Gebruik een gebruikersnaam van 3 tot 64 tekens.")
		return
	}
	if len(input.Password) < minPasswordLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Gebruik een wachtwoord van minstens %d tekens.", minPasswordLength))
		return
	}
	user, err := h.store.CreateUser(username, input.Password, RoleViewer)
	if errors.Is(err, os.ErrExist) {
		writeError(w, http.StatusConflict, "Deze gebruikersnaam bestaat al.")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "De gebruiker kon niet worden opgeslagen.")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": user.ID, "username": user.Username, "enabled": user.Enabled, "createdAt": user.CreatedAt,
	})
}

func (h *Hub) patchUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Enabled  *bool   `json:"enabled"`
		Password *string `json:"password"`
	}
	if decodeJSON(r, &input) != nil || (input.Enabled == nil && input.Password == nil) {
		writeError(w, http.StatusBadRequest, "Geef enabled en/of password op.")
		return
	}
	id := r.PathValue("id")
	if input.Enabled != nil {
		if err := h.store.SetUserEnabled(id, *input.Enabled); err != nil {
			writeError(w, http.StatusNotFound, "Gebruiker niet gevonden.")
			return
		}
	}
	if input.Password != nil {
		if len(*input.Password) < minPasswordLength {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("Gebruik een wachtwoord van minstens %d tekens.", minPasswordLength))
			return
		}
		if err := h.store.SetUserPassword(id, *input.Password); err != nil {
			writeError(w, http.StatusNotFound, "Gebruiker niet gevonden.")
			return
		}
		// A password reset signs every device for this account out, so the
		// old password can no longer be used to keep a session alive.
		_ = h.store.RevokeUserSessions(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteUser(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "Gebruiker niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) listAddons(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"addons": h.addonCatalog()})
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
		if value.ID == "" || !isMediaType(value.Type) {
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
	baseURL := strings.TrimSuffix(base.String(), "/")
	configureURL := ""
	// Stremio-style addons signal a configuration page with behaviorHints;
	// by convention it lives at "<base>/configure". The hub never builds
	// its own settings form for this — Configure always opens the addon.
	if manifest.BehaviorHints.Configurable || manifest.BehaviorHints.ConfigurationRequired {
		configureURL = baseURL + "/configure"
	}
	addon := Addon{
		ID: manifest.ID, Name: manifest.Name, Description: strings.TrimSpace(manifest.Description),
		Version: strings.TrimSpace(manifest.Version), ManifestURL: manifestURL.String(),
		BaseURL: baseURL, ConfigureURL: configureURL, Resources: resources, Catalogs: catalogs,
		Enabled: true, AddedAt: time.Now().UTC(), Reachable: true,
	}
	admin, _ := r.Context().Value(ctxUserKey).(User)
	if err := h.store.PutAddonForUser(admin.ID, addon); err != nil {
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
	admin, _ := r.Context().Value(ctxUserKey).(User)
	if err := h.store.SetAddonEnabledForUser(admin.ID, r.PathValue("id"), *input.Enabled); err != nil {
		writeError(w, http.StatusNotFound, "Addon niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) deleteAddon(w http.ResponseWriter, r *http.Request) {
	admin, _ := r.Context().Value(ctxUserKey).(User)
	if err := h.store.DeleteAddonForUser(admin.ID, r.PathValue("id")); err != nil {
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
	admin, _ := r.Context().Value(ctxUserKey).(User)
	if err := h.store.MoveAddonForUser(admin.ID, r.PathValue("id"), input.Direction); err != nil {
		writeError(w, http.StatusNotFound, "Addon niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) streams(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := r.PathValue("id")
	if !isMediaType(mediaType) || id == "" {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype of id.")
		return
	}

	values := h.aggregateStreams(r.Context(), mediaType, id)
	writeJSON(w, http.StatusOK, map[string]any{"streams": values})
}

func (h *Hub) aggregateStreams(ctx context.Context, mediaType, id string) []HubStream {
	addons := h.addonCatalog()
	var enabled []Addon
	for _, addon := range addons {
		// Only ask addons that actually declared the "stream" capability.
		// Calling every enabled addon regardless of what it supports wastes
		// requests and pollutes health/status with failures from addons
		// that were never going to answer (e.g. a metadata-only addon).
		if addon.Enabled && contains(addon.Resources, "stream") {
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
			if err != nil {
				h.recordAddonHealth(addon.ID, false, sanitizeAddonError(err))
			} else {
				h.recordAddonHealth(addon.ID, true, "")
			}
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
	for index, addon := range addons {
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

func (h *Hub) subtitles(w http.ResponseWriter, r *http.Request) {
	mediaType := r.PathValue("type")
	id := r.PathValue("id")
	if !isMediaType(mediaType) || id == "" {
		writeError(w, http.StatusBadRequest, "Ongeldig mediatype of id.")
		return
	}

	values := h.aggregateSubtitles(r.Context(), mediaType, id)
	writeJSON(w, http.StatusOK, map[string]any{"subtitles": values})
}

// aggregateSubtitles is the subtitle equivalent of aggregateStreams: it asks
// every enabled addon that declared the "subtitle" capability, in parallel,
// and combines the results. Kept as its own capability (rather than folded
// into streams) so any number of subtitle addons can serve a title
// independently of which addon resolved the video itself.
func (h *Hub) aggregateSubtitles(ctx context.Context, mediaType, id string) []HubSubtitle {
	var enabled []Addon
	for _, addon := range h.addonCatalog() {
		if addon.Enabled && contains(addon.Resources, "subtitles") {
			enabled = append(enabled, addon)
		}
	}

	type result struct {
		subtitles []HubSubtitle
		err       error
	}
	results := make(chan result, len(enabled))
	var wg sync.WaitGroup
	for _, addon := range enabled {
		wg.Add(1)
		go func(addon Addon) {
			defer wg.Done()
			values, err := h.fetchSubtitles(ctx, addon, mediaType, id)
			if err != nil {
				h.recordAddonHealth(addon.ID, false, sanitizeAddonError(err))
			} else {
				h.recordAddonHealth(addon.ID, true, "")
			}
			results <- result{values, err}
		}(addon)
	}
	go func() { wg.Wait(); close(results) }()

	var values []HubSubtitle
	for result := range results {
		if result.err != nil {
			slog.Warn("addon subtitle request failed", "error", result.err)
			continue
		}
		values = append(values, result.subtitles...)
	}
	seen := map[string]bool{}
	deduplicated := make([]HubSubtitle, 0, len(values))
	for _, value := range values {
		if seen[value.URL] {
			continue
		}
		seen[value.URL] = true
		deduplicated = append(deduplicated, value)
	}
	return deduplicated
}

// fetchSubtitles calls a subtitle addon's Stremio-style
// "subtitles/{type}/{id}.json" endpoint.
func (h *Hub) fetchSubtitles(ctx context.Context, addon Addon, mediaType, id string) ([]HubSubtitle, error) {
	base, err := url.Parse(addon.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = path.Join(base.Path, "subtitles", mediaType, id+".json")
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
	var payload subtitleResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	values := make([]HubSubtitle, 0, len(payload.Subtitles))
	for _, raw := range payload.Subtitles {
		var source struct {
			ID, Lang, URL string
		}
		if json.Unmarshal(raw, &source) != nil {
			continue
		}
		subtitleURL, err := url.Parse(strings.TrimSpace(source.URL))
		if err != nil || (subtitleURL.Scheme != "http" && subtitleURL.Scheme != "https") || subtitleURL.Host == "" {
			continue
		}
		values = append(values, HubSubtitle{
			AddonID: addon.ID, AddonName: addon.Name, Lang: source.Lang, Name: source.ID, URL: subtitleURL.String(),
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

// sanitizeAddonError turns a raw error from calling an addon into a short,
// safe message for storage/display. Go's http client embeds the full
// request URL (which can carry an API key in its query string) in errors
// like "Get \"https://addon/x?token=…\": dial tcp: …", so this never passes
// err.Error() straight through — only a fixed set of generic categories.
func sanitizeAddonError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "time-out"
	case errors.Is(err, context.Canceled):
		return "verzoek geannuleerd"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "returned HTTP"):
		// Constructed by us (e.g. "<addon name> returned HTTP 502") and
		// never contains the request URL, so it's safe as-is.
		return message
	case strings.Contains(message, "no such host") || strings.Contains(message, "dial tcp") || strings.Contains(message, "connection refused"):
		return "kon addon niet bereiken"
	case strings.Contains(message, "invalid character") || strings.Contains(message, "unexpected end of JSON") || strings.Contains(message, "cannot unmarshal"):
		return "ongeldig antwoord van addon"
	default:
		return "aanvraag naar addon mislukt"
	}
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
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
