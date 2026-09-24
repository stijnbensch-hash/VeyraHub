package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// doJSON performs an authenticated JSON request against a test server and
// decodes the response body into out (if non-nil). It fails the test on
// transport errors or an unexpected status code.
func doJSON(t *testing.T, method, url, token string, body any, wantStatus int, out any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	} else {
		reader = bytes.NewReader(nil)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s: expected status %d, got %d", method, url, wantStatus, response.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRankStreamsOrdersByQualityThenAddonOrder checks the heuristic ranking
// added on top of the pre-existing URL-based dedup: higher resolution wins
// regardless of addon order, and addon order is the deterministic fallback
// when nothing else distinguishes two streams.
func TestRankStreamsOrdersByQualityThenAddonOrder(t *testing.T) {
	streams := []HubStream{
		{AddonID: "b", URL: "https://x/b-720p.mp4", Title: "Movie 720p x264"},
		{AddonID: "a", URL: "https://x/a-2160p.mp4", Title: "Movie 2160p HEVC HDR"},
		{AddonID: "a", URL: "https://x/a-1080p.mp4", Title: "Movie 1080p"},
		{AddonID: "c", URL: "https://x/c-unknown.mp4", Title: "Movie"},
	}
	ranked := rankStreams(streams, map[string]int{"a": 0, "b": 1, "c": 2})
	if ranked[0].URL != "https://x/a-2160p.mp4" {
		t.Fatalf("expected the 4K/HDR stream first, got %s", ranked[0].URL)
	}
	if ranked[1].URL != "https://x/a-1080p.mp4" {
		t.Fatalf("expected 1080p second, got %s", ranked[1].URL)
	}
	if ranked[2].URL != "https://x/b-720p.mp4" {
		t.Fatalf("expected 720p third, got %s", ranked[2].URL)
	}
	if ranked[3].URL != "https://x/c-unknown.mp4" {
		t.Fatalf("expected the unparseable stream last, got %s", ranked[3].URL)
	}
}

func TestDedupeStreamMirrorsKeepsDifferentAddonsSeparate(t *testing.T) {
	values := []HubStream{
		{AddonID: "a", URL: "https://mirror1/x.mp4", Filename: "movie.mp4", VideoSize: 100},
		{AddonID: "a", URL: "https://mirror2/x.mp4", Filename: "movie.mp4", VideoSize: 100},
		{AddonID: "b", URL: "https://mirror3/x.mp4", Filename: "movie.mp4", VideoSize: 100},
	}
	result := dedupeStreamMirrors(values)
	if len(result) != 2 {
		t.Fatalf("expected the same-addon mirror to collapse (2 left), got %d", len(result))
	}
}

// addonServer spins up a minimal Stremio-style addon with one catalog whose
// items carry genres, for filter/collection tests.
func addonServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			json.NewEncoder(w).Encode(map[string]any{
				"id": "test.addon", "name": "Test bron", "version": "1.0",
				"resources": []string{"catalog", "meta", "stream"},
				"catalogs":  []any{map[string]any{"id": "popular", "type": "movie", "name": "Populair"}},
			})
		case "/catalog/movie/popular.json":
			json.NewEncoder(w).Encode(map[string]any{"metas": []any{
				map[string]any{"id": "tt1", "type": "movie", "name": "Actiefilm", "year": 2020, "genres": []string{"Action"}},
				map[string]any{"id": "tt2", "type": "movie", "name": "Kinderfilm", "year": 2022, "genres": []string{"Family"}},
			}})
		case "/stream/movie/tt1.json":
			json.NewEncoder(w).Encode(map[string]any{"streams": []any{
				map[string]any{"name": "1080p", "url": "https://media.example/tt1.m3u8"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func setupHubWithAddon(t *testing.T) (server *httptest.Server, addon *httptest.Server, adminToken string) {
	t.Helper()
	addon = addonServer(t)
	t.Cleanup(addon.Close)

	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", addon.Client())
	server = httptest.NewServer(hub.Routes())
	t.Cleanup(server.Close)

	adminToken = adminLogin(t, server.URL, "secret-password")
	doJSON(t, http.MethodPost, server.URL+"/v1/addons", adminToken,
		map[string]string{"manifestURL": addon.URL + "/manifest.json"}, http.StatusCreated, nil)
	return
}

// TestContentFilterExcludesGenreFromNativeCatalog covers feature 2: a
// viewer's own genre filter hides matching items in native catalog
// requests, without an admin needing to touch the shared addon catalog.
func TestContentFilterExcludesGenreFromNativeCatalog(t *testing.T) {
	server, _, adminToken := setupHubWithAddon(t)

	doJSON(t, http.MethodPost, server.URL+"/v1/users", adminToken,
		map[string]string{"username": "kid", "password": "kid-password"}, http.StatusCreated, nil)
	var login struct {
		AccessToken string `json:"accessToken"`
	}
	doJSON(t, http.MethodPost, server.URL+"/v1/auth/login", "",
		map[string]string{"username": "kid", "password": "kid-password"}, http.StatusOK, &login)

	doJSON(t, http.MethodPut, server.URL+"/v1/me/filters", login.AccessToken,
		map[string]any{"excludedGenres": []string{"Action"}}, http.StatusOK, nil)

	var catalogs struct {
		Catalogs []MediaCatalog `json:"catalogs"`
	}
	doJSON(t, http.MethodGet, server.URL+"/api/v1/catalogs", login.AccessToken, nil, http.StatusOK, &catalogs)
	if len(catalogs.Catalogs) != 1 {
		t.Fatalf("expected one catalog, got %d", len(catalogs.Catalogs))
	}
	catalog := catalogs.Catalogs[0]

	var items struct {
		Items []MediaItem `json:"items"`
	}
	doJSON(t, http.MethodGet,
		server.URL+"/api/v1/catalog/"+catalog.AddonID+"/"+catalog.Type+"/"+catalog.ID,
		login.AccessToken, nil, http.StatusOK, &items)

	if len(items.Items) != 1 || items.Items[0].ID != "tt2" {
		t.Fatalf("expected only the non-Action item to remain, got %+v", items.Items)
	}
}

// TestSmartCollectionAggregatesAcrossCatalogsByRule covers feature 3.
func TestSmartCollectionAggregatesAcrossCatalogsByRule(t *testing.T) {
	server, _, adminToken := setupHubWithAddon(t)

	var collection SmartCollection
	doJSON(t, http.MethodPost, server.URL+"/v1/collections", adminToken,
		map[string]any{"name": "Familiefilms", "mediaType": "movie", "genre": "Family"}, http.StatusCreated, &collection)

	var items struct {
		Items []MediaItem `json:"items"`
	}
	doJSON(t, http.MethodGet, server.URL+"/api/v1/collections/"+collection.ID+"/items", adminToken, nil, http.StatusOK, &items)
	if len(items.Items) != 1 || items.Items[0].ID != "tt2" {
		t.Fatalf("expected only the Family-genre item in the collection, got %+v", items.Items)
	}
}

// TestProfileScreenTimeBlocksStreamsOnceLimitReached covers feature 4.
func TestProfileScreenTimeBlocksStreamsOnceLimitReached(t *testing.T) {
	server, _, adminToken := setupHubWithAddon(t)

	var profile Profile
	doJSON(t, http.MethodPost, server.URL+"/v1/me/profiles", adminToken,
		map[string]any{"name": "Kids", "isKid": true, "dailyLimitMinutes": 30}, http.StatusCreated, &profile)

	// Under the limit: streams should be reachable.
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/items/movie/tt1/streams", nil)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("X-Veyra-Profile-Id", profile.ID)
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("expected streams to be reachable under the limit: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()

	var usage map[string]any
	doJSON(t, http.MethodPost, server.URL+"/v1/me/profiles/"+profile.ID+"/usage", adminToken,
		map[string]any{"minutes": 45}, http.StatusOK, &usage)
	if usage["limitReached"] != true {
		t.Fatalf("expected limitReached=true after exceeding the daily limit, got %+v", usage)
	}

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/api/v1/items/movie/tt1/streams", nil)
	request.Header.Set("Authorization", "Bearer "+adminToken)
	request.Header.Set("X-Veyra-Profile-Id", profile.ID)
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("expected streams to be blocked once the daily limit is reached: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()
}

// TestAddonHealthTransitionNotifiesAdmin covers feature 5's server-side
// trigger: an addon flipping from reachable to unreachable creates a
// notification for the admin account.
func TestAddonHealthTransitionNotifiesAdmin(t *testing.T) {
	server, addon, adminToken := setupHubWithAddon(t)

	// First call already happened in setupHubWithAddon via addAddon itself
	// (Reachable: true at creation), so the first *stream* call here is
	// still "reachable" and shouldn't notify yet.
	doJSON(t, http.MethodGet, server.URL+"/v1/streams/movie/tt1", adminToken, nil, http.StatusOK, nil)

	addon.Close() // now every subsequent addon call fails

	doJSON(t, http.MethodGet, server.URL+"/v1/streams/movie/tt1", adminToken, nil, http.StatusOK, nil)

	var notifications struct {
		Notifications []Notification `json:"notifications"`
	}
	doJSON(t, http.MethodGet, server.URL+"/v1/me/notifications", adminToken, nil, http.StatusOK, &notifications)
	found := false
	for _, notification := range notifications.Notifications {
		if notification.Title == "Addon onbereikbaar" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an 'Addon onbereikbaar' notification, got %+v", notifications.Notifications)
	}
}

// TestLocalSourceListsAndServesFile covers feature 6's local-folder path
// (the WebDAV path needs a real WebDAV server and isn't covered here).
func TestLocalSourceListsAndServesFile(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", nil)
	server := httptest.NewServer(hub.Routes())
	defer server.Close()
	adminToken := adminLogin(t, server.URL, "secret-password")

	mediaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(mediaDir, "movie.mp4"), []byte("fake-video-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	var source Source
	doJSON(t, http.MethodPost, server.URL+"/v1/sources", adminToken,
		map[string]string{"name": "Eigen films", "kind": "local", "rootPath": mediaDir}, http.StatusCreated, &source)

	var files struct {
		Files []sourceFile `json:"files"`
	}
	doJSON(t, http.MethodGet, server.URL+"/api/v1/sources/"+source.ID+"/files", adminToken, nil, http.StatusOK, &files)
	if len(files.Files) != 1 || files.Files[0].RelativePath != "movie.mp4" {
		t.Fatalf("expected one file movie.mp4, got %+v", files.Files)
	}

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sources/"+source.ID+"/file/movie.mp4?api_key="+adminToken, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("expected the file to be served: %v status=%v", err, response.StatusCode)
	}
	defer response.Body.Close()
}

// TestRequestLifecycleNotifiesRequester covers feature 7: a viewer's
// request, once the admin resolves it, generates a notification for that
// viewer specifically (not for the admin, not for other viewers).
func TestRequestLifecycleNotifiesRequester(t *testing.T) {
	server, _, adminToken := setupHubWithAddon(t)

	doJSON(t, http.MethodPost, server.URL+"/v1/users", adminToken,
		map[string]string{"username": "viewer", "password": "viewer-password"}, http.StatusCreated, nil)
	var login struct {
		AccessToken string `json:"accessToken"`
	}
	doJSON(t, http.MethodPost, server.URL+"/v1/auth/login", "",
		map[string]string{"username": "viewer", "password": "viewer-password"}, http.StatusOK, &login)

	var created MediaRequest
	doJSON(t, http.MethodPost, server.URL+"/v1/me/requests", login.AccessToken,
		map[string]string{"query": "Een film die nergens te vinden is"}, http.StatusCreated, &created)

	doJSON(t, http.MethodPatch, server.URL+"/v1/requests/"+created.ID, adminToken,
		map[string]string{"status": "resolved"}, http.StatusOK, nil)

	var notifications struct {
		Notifications []Notification `json:"notifications"`
	}
	doJSON(t, http.MethodGet, server.URL+"/v1/me/notifications", login.AccessToken, nil, http.StatusOK, &notifications)
	if len(notifications.Notifications) != 1 || notifications.Notifications[0].Title != "Aanvraag toegevoegd" {
		t.Fatalf("expected the requester to be notified, got %+v", notifications.Notifications)
	}

	var adminNotifications struct {
		Notifications []Notification `json:"notifications"`
	}
	doJSON(t, http.MethodGet, server.URL+"/v1/me/notifications", adminToken, nil, http.StatusOK, &adminNotifications)
	if len(adminNotifications.Notifications) != 0 {
		t.Fatalf("expected the admin to not receive the requester's own notification, got %+v", adminNotifications.Notifications)
	}
}
