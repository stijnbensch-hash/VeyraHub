package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateEndpoint(t *testing.T) {
	valid := []string{"https://example.com/manifest.json", "http://127.0.0.1:7000/manifest.json", "http://addon.local/manifest.json"}
	for _, value := range valid {
		if _, err := validateEndpoint(value); err != nil {
			t.Errorf("expected %s to be valid: %v", value, err)
		}
	}
	invalid := []string{"http://example.com/manifest.json", "https://user:pass@example.com/manifest.json", "https://example.com/other.json"}
	for _, value := range invalid {
		if _, err := validateEndpoint(value); err == nil {
			t.Errorf("expected %s to be invalid", value)
		}
	}
}

// adminLogin logs in with the bootstrap admin account and returns an access token.
func adminLogin(t *testing.T, serverURL, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": password})
	request, _ := http.NewRequest(http.MethodPost, serverURL+"/v1/auth/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("admin login failed: %v status=%v", err, response.StatusCode)
	}
	defer response.Body.Close()
	var payload struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.AccessToken == "" {
		t.Fatal("expected a non-empty access token")
	}
	return payload.AccessToken
}

func TestAddonAndStreamAggregation(t *testing.T) {
	addon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			json.NewEncoder(w).Encode(map[string]any{
				"id": "test.addon", "name": "Test bron", "version": "1.0",
				"resources":     []string{"catalog", "meta", "stream"},
				"catalogs":      []any{map[string]any{"id": "popular", "type": "movie", "name": "Populair"}},
				"behaviorHints": map[string]any{"configurable": true},
			})
		case "/catalog/movie/popular.json":
			json.NewEncoder(w).Encode(map[string]any{"metas": []any{
				map[string]any{"id": "tt123", "type": "movie", "name": "Voorbeeldfilm", "year": 2026},
			}})
		case "/stream/movie/tt123.json":
			json.NewEncoder(w).Encode(map[string]any{"streams": []any{
				map[string]any{"name": "4K", "url": "https://media.example/movie.m3u8", "behaviorHints": map[string]any{"videoSize": 1234}},
				map[string]any{"name": "Torrent", "infoHash": "abc"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer addon.Close()

	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", addon.Client())
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	accessToken := adminLogin(t, server.URL, "secret-password")

	body, _ := json.Marshal(map[string]string{"manifestURL": addon.URL + "/manifest.json"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/addons", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("add addon failed: %v status=%v", err, response.StatusCode)
	}
	var createdAddon struct {
		ConfigureURL string `json:"configureURL"`
	}
	if err := json.NewDecoder(response.Body).Decode(&createdAddon); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if createdAddon.ConfigureURL != addon.URL+"/configure" {
		t.Fatalf("expected configureURL to be derived from behaviorHints, got %q", createdAddon.ConfigureURL)
	}

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/streams/movie/tt123", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload struct {
		Streams []HubStream `json:"streams"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Streams) != 1 {
		t.Fatalf("expected one direct stream, got %d", len(payload.Streams))
	}
	if payload.Streams[0].AddonName != "Test bron" {
		t.Fatalf("unexpected addon name: %s", payload.Streams[0].AddonName)
	}

	// A successful stream call must be reflected in the addon's health.
	request, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/addons", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("list addons failed: %v status=%v", err, response.StatusCode)
	}
	var addonList struct {
		Addons []struct {
			Reachable     bool   `json:"reachable"`
			LastSuccessAt string `json:"lastSuccessAt"`
		} `json:"addons"`
	}
	if err := json.NewDecoder(response.Body).Decode(&addonList); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(addonList.Addons) != 1 || !addonList.Addons[0].Reachable || addonList.Addons[0].LastSuccessAt == "" {
		t.Fatalf("expected addon health to show a successful call: %#v", addonList.Addons)
	}

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/System/Info/Public", nil)
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("system info failed: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()

	authBody, _ := json.Marshal(map[string]string{"Username": "admin", "Pw": "secret-password"})
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/Users/AuthenticateByName", bytes.NewReader(authBody))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("Jellyfin auth failed: %v status=%v", err, response.StatusCode)
	}
	var jellyfinAuth struct {
		User struct {
			ID string `json:"Id"`
		} `json:"User"`
		AccessToken string `json:"AccessToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&jellyfinAuth); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if jellyfinAuth.AccessToken == "" {
		t.Fatal("expected a Jellyfin session access token")
	}

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/Users/"+jellyfinAuth.User.ID+"/Views", nil)
	request.Header.Set("X-Emby-Token", jellyfinAuth.AccessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var views struct {
		Items []struct {
			ID string `json:"Id"`
		} `json:"Items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&views); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(views.Items) != 1 {
		t.Fatalf("expected one media library, got %d", len(views.Items))
	}

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/Users/"+jellyfinAuth.User.ID+"/Items?ParentId="+views.Items[0].ID+"&IncludeItemTypes=Movie", nil)
	request.Header.Set("X-Emby-Token", jellyfinAuth.AccessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var items struct {
		Items []map[string]any `json:"Items"`
	}
	if err := json.NewDecoder(response.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(items.Items) != 1 || items.Items[0]["Name"] != "Voorbeeldfilm" {
		t.Fatalf("unexpected library items: %#v", items.Items)
	}
}

func TestAddonHealthRecordsSanitizedFailure(t *testing.T) {
	addonURL := "http://127.0.0.1:1" // nothing listens here: connection refused
	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Seed a broken addon directly (bypassing addAddon, which would fail to
	// fetch its manifest) so we can exercise the stream-failure health path.
	if err := store.PutAddon(Addon{
		ID: "broken", Name: "Kapotte bron", BaseURL: addonURL,
		ManifestURL: addonURL + "/manifest.json", Resources: []string{"stream"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", nil)
	server := httptest.NewServer(hub.Routes())
	defer server.Close()
	accessToken := adminLogin(t, server.URL, "secret-password")

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/streams/movie/tt999", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/addons", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	var addonList struct {
		Addons []struct {
			Reachable   bool   `json:"reachable"`
			LastErrorAt string `json:"lastErrorAt"`
			LastError   string `json:"lastError"`
		} `json:"addons"`
	}
	if err := json.Unmarshal(body, &addonList); err != nil {
		t.Fatal(err)
	}
	if len(addonList.Addons) != 1 || addonList.Addons[0].Reachable || addonList.Addons[0].LastErrorAt == "" {
		t.Fatalf("expected addon health to show a failed call: %#v", addonList.Addons)
	}
	if addonList.Addons[0].LastError == "" || strings.Contains(addonList.Addons[0].LastError, addonURL) {
		t.Fatalf("lastError should be a sanitized message, not the raw addon URL: %q", addonList.Addons[0].LastError)
	}
}

func TestStreamAggregationOnlyCallsStreamCapableAddons(t *testing.T) {
	var metaOnlyHits int
	metaOnlyAddon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metaOnlyHits++
		http.NotFound(w, r)
	}))
	defer metaOnlyAddon.Close()

	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutAddon(Addon{
		ID: "meta-only", Name: "Metadata-only bron", BaseURL: metaOnlyAddon.URL,
		ManifestURL: metaOnlyAddon.URL + "/manifest.json", Resources: []string{"catalog", "meta"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	hub := NewHub(store, "admin", "secret-password", nil)
	server := httptest.NewServer(hub.Routes())
	defer server.Close()
	accessToken := adminLogin(t, server.URL, "secret-password")

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/streams/movie/tt1", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("stream request failed: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()

	if metaOnlyHits != 0 {
		t.Fatalf("expected the metadata-only addon to never be called for streams, got %d hits", metaOnlyHits)
	}
}

func TestSubtitleAggregationOnlyCallsSubtitleCapableAddons(t *testing.T) {
	var streamOnlyHits int
	streamOnlyAddon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streamOnlyHits++
		http.NotFound(w, r)
	}))
	defer streamOnlyAddon.Close()

	subtitleAddon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/subtitles/movie/tt1.json" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"subtitles": []any{
			map[string]any{"id": "nl", "lang": "nl", "url": "https://subs.example/tt1-nl.srt"},
		}})
	}))
	defer subtitleAddon.Close()

	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutAddon(Addon{
		ID: "stream-only", Name: "Stream-only bron", BaseURL: streamOnlyAddon.URL,
		ManifestURL: streamOnlyAddon.URL + "/manifest.json", Resources: []string{"stream"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutAddon(Addon{
		ID: "subs", Name: "Ondertitelbron", BaseURL: subtitleAddon.URL,
		ManifestURL: subtitleAddon.URL + "/manifest.json", Resources: []string{"subtitles"}, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	hub := NewHub(store, "admin", "secret-password", nil)
	server := httptest.NewServer(hub.Routes())
	defer server.Close()
	accessToken := adminLogin(t, server.URL, "secret-password")

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/subtitles/movie/tt1", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("subtitle request failed: %v status=%v", err, response.StatusCode)
	}
	var payload struct {
		Subtitles []HubSubtitle `json:"subtitles"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	if len(payload.Subtitles) != 1 || payload.Subtitles[0].Lang != "nl" {
		t.Fatalf("expected one Dutch subtitle track, got %#v", payload.Subtitles)
	}
	if streamOnlyHits != 0 {
		t.Fatalf("expected the stream-only addon to never be called for subtitles, got %d hits", streamOnlyHits)
	}
}

func TestAuthenticationRequired(t *testing.T) {
	store, _ := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	request := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	response := httptest.NewRecorder()
	NewHub(store, "admin", "secret-password", nil).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", response.Code)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	store, _ := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	server := httptest.NewServer(NewHub(store, "admin", "secret-password", nil).Routes())
	defer server.Close()

	body, _ := json.Marshal(map[string]string{"username": "admin", "password": "wrong-password"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/auth/login", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()
}

func TestRefreshAndLogoutRotateAndRevokeSessions(t *testing.T) {
	store, _ := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	server := httptest.NewServer(NewHub(store, "admin", "secret-password", nil).Routes())
	defer server.Close()

	loginBody, _ := json.Marshal(map[string]string{"username": "admin", "password": "secret-password", "deviceId": "mac-1", "deviceName": "MacBook"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/auth/login", bytes.NewReader(loginBody))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %v status=%v", err, response.StatusCode)
	}
	var login struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&login); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	refreshBody, _ := json.Marshal(map[string]string{"refreshToken": login.RefreshToken})
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/auth/refresh", bytes.NewReader(refreshBody))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("refresh failed: %v status=%v", err, response.StatusCode)
	}
	var refreshed struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&refreshed); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	// The old access token from before the refresh must no longer work.
	request, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/status", nil)
	request.Header.Set("Authorization", "Bearer "+login.AccessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected old access token to be rejected after refresh: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()

	// The rotated access token must work.
	request, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/status", nil)
	request.Header.Set("Authorization", "Bearer "+refreshed.AccessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("expected refreshed access token to work: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()

	// Logging out revokes the session; the access token stops working.
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/auth/logout", nil)
	request.Header.Set("Authorization", "Bearer "+refreshed.AccessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("logout failed: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/status", nil)
	request.Header.Set("Authorization", "Bearer "+refreshed.AccessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected access token to be rejected after logout: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()
}

func TestViewerAccountCanUseJellyfinButNotAdminAPI(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewHub(store, "admin", "admin-secret-password", nil).Routes())
	defer server.Close()

	adminToken := adminLogin(t, server.URL, "admin-secret-password")

	createBody, _ := json.Marshal(map[string]string{"username": "vriend", "password": "vriend-wachtwoord"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/users", bytes.NewReader(createBody))
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("create viewer failed: %v status=%v", err, response.StatusCode)
	}
	var created struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if created.ID == "" {
		t.Fatalf("unexpected credentials: id=%q", created.ID)
	}

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/users", nil)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	listBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if bytes.Contains(listBody, []byte("vriend-wachtwoord")) || bytes.Contains(listBody, []byte("passwordHash")) {
		t.Fatal("viewer list exposed credential material")
	}

	authBody, _ := json.Marshal(map[string]string{"Username": created.Username, "Pw": "vriend-wachtwoord"})
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/Users/AuthenticateByName", bytes.NewReader(authBody))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("viewer Jellyfin auth failed: %v status=%v", err, response.StatusCode)
	}
	var jellyfinAuth struct {
		AccessToken string `json:"AccessToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&jellyfinAuth); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/Users/"+created.ID+"/Views", nil)
	request.Header.Set("X-Emby-Token", jellyfinAuth.AccessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("viewer Jellyfin request failed: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/status", nil)
	request.Header.Set("Authorization", "Bearer "+jellyfinAuth.AccessToken)
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer reached admin API: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()
}
