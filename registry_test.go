package main

// registry_test.go covers the addon/catalogue registry extensions: capability
// detection, configurable-addon detection, catalogue-specific genres,
// catalogue enable/disable, catalogue ordering (incl. persistence across a
// manifest refresh and a store reload), per-addon timeout/health, and
// backward-compatible loading of catalogs persisted before these fields
// existed.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// registryAddonServer serves a manifest with two catalogs that each
// advertise their own, different genre list -- the key case for
// "catalogue-specific genres must not be pooled into one global list" -- is
// configurable (so ConfigureURL/Capabilities.Configure can be asserted),
// and supports catalog/meta/stream/subtitles resources so every capability
// flag has something to be true for.
func registryAddonServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			json.NewEncoder(w).Encode(map[string]any{
				"id": "registry.addon", "name": "Registry bron", "version": "1.0",
				"resources": []string{"catalog", "meta", "stream", "subtitles"},
				"catalogs": []any{
					map[string]any{
						"id": "trending", "type": "movie", "name": "Trending Movies",
						"extra": []any{
							map[string]any{"name": "genre", "options": []string{"Action", "Comedy", "Horror", "Action"}},
							map[string]any{"name": "search"},
						},
					},
					map[string]any{
						"id": "popular-series", "type": "series", "name": "Popular Series",
						"extra": []any{
							map[string]any{"name": "genre", "options": []string{"Drama", "Crime", "Sci-Fi"}},
						},
					},
				},
				"behaviorHints": map[string]any{"configurable": true},
			})
		case "/catalog/movie/trending.json":
			json.NewEncoder(w).Encode(map[string]any{"metas": []any{
				map[string]any{"id": "tt1", "type": "movie", "name": "Film een", "year": 2024},
			}})
		case "/catalog/series/popular-series.json":
			json.NewEncoder(w).Encode(map[string]any{"metas": []any{
				map[string]any{"id": "tt2", "type": "series", "name": "Serie een", "year": 2024},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func setupRegistryHub(t *testing.T) (server *httptest.Server, addon *httptest.Server, adminToken string, hub *Hub) {
	t.Helper()
	addon = registryAddonServer(t)
	t.Cleanup(addon.Close)

	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub = NewHub(store, "admin", "secret-password", addon.Client())
	server = httptest.NewServer(hub.Routes())
	t.Cleanup(server.Close)

	adminToken = adminLogin(t, server.URL, "secret-password")
	doJSON(t, http.MethodPost, server.URL+"/v1/addons", adminToken,
		map[string]string{"manifestURL": addon.URL + "/manifest.json"}, http.StatusCreated, nil)
	return
}

func registryTestAddon(t *testing.T, server *httptest.Server, adminToken string) AddonView {
	t.Helper()
	var list struct {
		Addons []AddonView `json:"addons"`
	}
	doJSON(t, http.MethodGet, server.URL+"/v1/addons", adminToken, nil, http.StatusOK, &list)
	if len(list.Addons) != 1 {
		t.Fatalf("expected one addon, got %d", len(list.Addons))
	}
	return list.Addons[0]
}

// TestCapabilityDetectionIsResourceBased checks that capabilities are read
// off the manifest's declared resources/behaviorHints, never hardcoded by
// addon name, and that an addon which doesn't declare a resource shows
// false/unknown for it rather than a guess.
func TestCapabilityDetectionIsResourceBased(t *testing.T) {
	server, _, adminToken, _ := setupRegistryHub(t)
	addon := registryTestAddon(t, server, adminToken)

	caps := addon.Capabilities
	if !caps.Catalog || !caps.Meta || !caps.Stream || !caps.Subtitles {
		t.Fatalf("expected every declared resource capability to be true, got %#v", caps)
	}
	if !caps.Configure {
		t.Fatalf("expected Configure to be true for a behaviorHints.configurable addon, got %#v", caps)
	}
}

// TestConfigurableAddonDetectionWithoutHint checks the other side: an
// addon whose manifest doesn't set behaviorHints.configurable gets no
// ConfigureURL and Capabilities.Configure is false -- the hub never
// invents a configuration page for an addon that didn't advertise one.
func TestConfigurableAddonDetectionWithoutHint(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manifest.json" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "plain.addon", "name": "Eenvoudige bron", "version": "1.0",
			"resources": []string{"catalog"},
			"catalogs":  []any{map[string]any{"id": "all", "type": "movie", "name": "Alles"}},
		})
	}))
	defer plain.Close()

	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", plain.Client())
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	adminToken := adminLogin(t, server.URL, "secret-password")
	doJSON(t, http.MethodPost, server.URL+"/v1/addons", adminToken,
		map[string]string{"manifestURL": plain.URL + "/manifest.json"}, http.StatusCreated, nil)

	addon := registryTestAddon(t, server, adminToken)
	if addon.ConfigureURL != "" {
		t.Fatalf("expected no configureURL, got %q", addon.ConfigureURL)
	}
	if addon.Capabilities.Configure {
		t.Fatal("expected Capabilities.Configure to be false without behaviorHints.configurable")
	}
}

// TestCatalogueSpecificGenresAreNotPooled is the core case for "catalogue-
// specific genres": two catalogs on the same addon each declare their own
// genre list, and the hub must keep them apart rather than merging them
// into one global list. Also checks duplicate genre entries within one
// catalog's own list are deduplicated.
func TestCatalogueSpecificGenresAreNotPooled(t *testing.T) {
	server, _, adminToken, _ := setupRegistryHub(t)
	addon := registryTestAddon(t, server, adminToken)

	if len(addon.Catalogs) != 2 {
		t.Fatalf("expected two catalogs, got %d", len(addon.Catalogs))
	}
	byID := map[string]AddonCatalog{}
	for _, catalog := range addon.Catalogs {
		byID[catalog.ID] = catalog
	}

	trending, ok := byID["trending"]
	if !ok {
		t.Fatal("expected a 'trending' catalog")
	}
	wantTrending := []string{"Action", "Comedy", "Horror"}
	if !stringSlicesEqual(trending.Genres, wantTrending) {
		t.Fatalf("expected trending genres %v (deduplicated), got %v", wantTrending, trending.Genres)
	}

	series, ok := byID["popular-series"]
	if !ok {
		t.Fatal("expected a 'popular-series' catalog")
	}
	wantSeries := []string{"Drama", "Crime", "Sci-Fi"}
	if !stringSlicesEqual(series.Genres, wantSeries) {
		t.Fatalf("expected popular-series genres %v, got %v", wantSeries, series.Genres)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCatalogueEnableDisable checks that disabling one catalog hides it
// from client-facing catalog listings (native API) without touching the
// addon itself or the other catalog.
func TestCatalogueEnableDisable(t *testing.T) {
	server, _, adminToken, _ := setupRegistryHub(t)
	addon := registryTestAddon(t, server, adminToken)

	doJSON(t, http.MethodPatch,
		server.URL+"/v1/addons/"+addon.ID+"/catalogs/movie/trending", adminToken,
		map[string]bool{"enabled": false}, http.StatusNoContent, nil)

	var catalogs struct {
		Catalogs []MediaCatalog `json:"catalogs"`
	}
	doJSON(t, http.MethodGet, server.URL+"/api/v1/catalogs", adminToken, nil, http.StatusOK, &catalogs)
	if len(catalogs.Catalogs) != 1 || catalogs.Catalogs[0].ID != "popular-series" {
		t.Fatalf("expected only the still-enabled catalog, got %#v", catalogs.Catalogs)
	}

	// The addon itself, and the admin registry view, must still show both
	// catalogs -- disabling one never removes it or the addon.
	updated := registryTestAddon(t, server, adminToken)
	if !updated.Enabled {
		t.Fatal("disabling a catalog must not disable the addon")
	}
	if len(updated.Catalogs) != 2 {
		t.Fatalf("expected the disabled catalog to remain in the registry, got %d catalogs", len(updated.Catalogs))
	}
}

// TestCatalogueOrderingSurvivesRestart moves a catalog and checks the new
// order is both reflected immediately and still there after the store is
// reopened (simulating a container restart).
func TestCatalogueOrderingSurvivesRestart(t *testing.T) {
	addon := registryAddonServer(t)
	defer addon.Close()

	dataPath := filepath.Join(t.TempDir(), "hub.json")
	store, err := NewStore(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", addon.Client())
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	adminToken := adminLogin(t, server.URL, "secret-password")
	doJSON(t, http.MethodPost, server.URL+"/v1/addons", adminToken,
		map[string]string{"manifestURL": addon.URL + "/manifest.json"}, http.StatusCreated, nil)
	before := registryTestAddon(t, server, adminToken)
	if before.Catalogs[0].ID != "trending" || before.Catalogs[1].ID != "popular-series" {
		t.Fatalf("unexpected initial order: %#v", before.Catalogs)
	}

	// Move "popular-series" (index 1) left, swapping with "trending".
	doJSON(t, http.MethodPost,
		server.URL+"/v1/addons/"+before.ID+"/catalogs/series/popular-series/move", adminToken,
		map[string]int{"direction": -1}, http.StatusNoContent, nil)

	after := registryTestAddon(t, server, adminToken)
	if after.Catalogs[0].ID != "popular-series" || after.Catalogs[1].ID != "trending" {
		t.Fatalf("expected the move to swap catalog order, got %#v", after.Catalogs)
	}

	reopened, err := NewStore(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	adminID, _ := reopened.AdminUserID()
	addons := reopened.AddonsForUser(adminID)
	if len(addons) != 1 || len(addons[0].Catalogs) != 2 || addons[0].Catalogs[0].ID != "popular-series" {
		t.Fatalf("expected catalog order to survive a store reload, got %#v", addons)
	}
}

// TestManifestRefreshPreservesCatalogueSettings is the critical safety
// property for refresh: an admin's catalog enable/disable choice and
// chosen order must survive a manifest refresh that re-fetches the same
// catalogs, and a brand-new catalog must be appended (enabled, at the
// end) without disturbing the existing ones.
func TestManifestRefreshPreservesCatalogueSettings(t *testing.T) {
	// A mutable manifest: refreshable serves the two-catalog manifest
	// first, then (after addThirdCatalog flips) a three-catalog one with
	// the same two plus a new "new-arrivals" catalog -- same shape as an
	// addon gaining a catalog upstream between refreshes.
	addThirdCatalog := false
	addon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manifest.json" {
			http.NotFound(w, r)
			return
		}
		catalogs := []any{
			map[string]any{"id": "trending", "type": "movie", "name": "Trending Movies"},
			map[string]any{"id": "popular-series", "type": "series", "name": "Popular Series"},
		}
		if addThirdCatalog {
			catalogs = append(catalogs, map[string]any{"id": "new-arrivals", "type": "movie", "name": "New Arrivals"})
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "refresh.addon", "name": "Refresh bron", "version": "1.0",
			"resources": []string{"catalog"},
			"catalogs":  catalogs,
		})
	}))
	defer addon.Close()

	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", addon.Client())
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	adminToken := adminLogin(t, server.URL, "secret-password")
	doJSON(t, http.MethodPost, server.URL+"/v1/addons", adminToken,
		map[string]string{"manifestURL": addon.URL + "/manifest.json"}, http.StatusCreated, nil)
	before := registryTestAddon(t, server, adminToken)

	// Disable "trending" and move "popular-series" to the front.
	doJSON(t, http.MethodPatch,
		server.URL+"/v1/addons/"+before.ID+"/catalogs/movie/trending", adminToken,
		map[string]bool{"enabled": false}, http.StatusNoContent, nil)
	doJSON(t, http.MethodPost,
		server.URL+"/v1/addons/"+before.ID+"/catalogs/series/popular-series/move", adminToken,
		map[string]int{"direction": -1}, http.StatusNoContent, nil)

	addThirdCatalog = true
	doJSON(t, http.MethodPost, server.URL+"/v1/addons/"+before.ID+"/refresh", adminToken, nil, http.StatusOK, nil)

	after := registryTestAddon(t, server, adminToken)
	if len(after.Catalogs) != 3 {
		t.Fatalf("expected 3 catalogs after refresh (2 preserved + 1 new), got %d: %#v", len(after.Catalogs), after.Catalogs)
	}
	if after.Catalogs[0].ID != "popular-series" || after.Catalogs[1].ID != "trending" {
		t.Fatalf("expected the preserved order [popular-series, trending, ...], got %#v", after.Catalogs)
	}
	if after.Catalogs[1].IsEnabled() {
		t.Fatal("expected 'trending' to still be disabled after refresh")
	}
	if !after.Catalogs[0].IsEnabled() {
		t.Fatal("expected 'popular-series' to remain enabled after refresh")
	}
	if after.Catalogs[2].ID != "new-arrivals" || !after.Catalogs[2].IsEnabled() {
		t.Fatalf("expected the new catalog appended at the end, enabled by default, got %#v", after.Catalogs[2])
	}
}

// TestManifestRefreshFailurePreservesPreviousData checks that a refresh
// which can't reach the addon at all leaves the last known-good manifest
// (catalogs, settings) completely untouched, and reports the failure
// rather than silently clearing the addon's data.
func TestManifestRefreshFailurePreservesPreviousData(t *testing.T) {
	up := true
	addon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/manifest.json" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "flaky.addon", "name": "Wisselvallige bron", "version": "1.0",
			"resources": []string{"catalog"},
			"catalogs":  []any{map[string]any{"id": "all", "type": "movie", "name": "Alles"}},
		})
	}))
	defer addon.Close()

	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", addon.Client())
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	adminToken := adminLogin(t, server.URL, "secret-password")
	doJSON(t, http.MethodPost, server.URL+"/v1/addons", adminToken,
		map[string]string{"manifestURL": addon.URL + "/manifest.json"}, http.StatusCreated, nil)
	before := registryTestAddon(t, server, adminToken)

	up = false
	doJSON(t, http.MethodPost, server.URL+"/v1/addons/"+before.ID+"/refresh", adminToken, nil, http.StatusBadGateway, nil)

	after := registryTestAddon(t, server, adminToken)
	if len(after.Catalogs) != 1 || after.Catalogs[0].ID != "all" {
		t.Fatalf("expected the last known-good catalog list to survive a failed refresh, got %#v", after.Catalogs)
	}
}

// TestBackwardCompatibleCatalogDefaultsToEnabled loads a hub.json written
// before the catalog Enabled/Genres fields existed and checks such a
// catalog is treated as enabled (the required safe default) and still
// shows up in client-facing catalog listings.
func TestBackwardCompatibleCatalogDefaultsToEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.json")
	legacy := `{
		"nodeID": "legacy-node",
		"users": [{"id": "admin-1", "username": "admin", "passwordHash": "pbkdf2-sha256$1$00$00", "role": "admin", "enabled": true}],
		"userAddons": [{"userID": "admin-1", "addons": [{
			"id": "old.addon", "name": "Oude bron", "manifestURL": "https://old.example/manifest.json",
			"baseURL": "https://old.example", "resources": ["catalog"], "enabled": true,
			"catalogs": [{"id": "all", "type": "movie", "name": "Alles"}]
		}]}]
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", nil)

	addons := store.AddonsForUser("admin-1")
	if len(addons) != 1 || len(addons[0].Catalogs) != 1 {
		t.Fatalf("expected the legacy addon/catalog to load, got %#v", addons)
	}
	if !addons[0].Catalogs[0].IsEnabled() {
		t.Fatal("expected a catalog persisted before the Enabled field existed to default to enabled")
	}

	catalogs := hub.mediaCatalogs()
	if len(catalogs) != 1 || catalogs[0].ID != "all" {
		t.Fatalf("expected the legacy catalog to still be offered to clients, got %#v", catalogs)
	}
}

// TestAddonHealthTracksTimeoutAndConsecutiveFailures checks the richer
// health model: a per-addon timeout produces Status "timeout", and repeated
// failures push Status to "offline" after enough consecutive failures,
// without ever touching Enabled.
func TestAddonHealthTracksTimeoutAndConsecutiveFailures(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.json" {
			json.NewEncoder(w).Encode(map[string]any{
				"id": "slow.addon", "name": "Trage bron", "version": "1.0",
				"resources": []string{"catalog", "stream"},
				"catalogs":  []any{map[string]any{"id": "all", "type": "movie", "name": "Alles"}},
			})
			return
		}
		// Every other route (catalog/stream fetches) hangs until the test
		// closes `block`, well past the test's short addon timeout.
		<-block
	}))
	// Registered in this order so close(block) (which unblocks the
	// handler goroutine above) runs BEFORE slow.Close() -- Close() blocks
	// until outstanding handlers return, so closing the server first
	// while the handler is still parked on `<-block` would deadlock the
	// test itself.
	defer slow.Close()
	defer close(block)

	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", slow.Client())
	hub.SetAddonTimeout(100 * time.Millisecond)
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	adminToken := adminLogin(t, server.URL, "secret-password")
	doJSON(t, http.MethodPost, server.URL+"/v1/addons", adminToken,
		map[string]string{"manifestURL": slow.URL + "/manifest.json"}, http.StatusCreated, nil)
	addon := registryTestAddon(t, server, adminToken)
	if addon.Status != "unknown" {
		t.Fatalf("expected a freshly added addon to start at Status \"unknown\", got %q", addon.Status)
	}

	start := time.Now()
	doJSON(t, http.MethodGet, server.URL+"/v1/streams/movie/tt1", adminToken, nil, http.StatusOK, nil)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("expected the per-addon timeout to bound the request well under 5s, took %s", elapsed)
	}

	addon = registryTestAddon(t, server, adminToken)
	if addon.Reachable {
		t.Fatal("expected the addon to be marked unreachable after a timeout")
	}
	if addon.Status != "timeout" {
		t.Fatalf("expected Status \"timeout\" after a per-addon-deadline failure, got %q", addon.Status)
	}
	if addon.ConsecutiveFailures < 1 {
		t.Fatalf("expected ConsecutiveFailures to be tracked, got %d", addon.ConsecutiveFailures)
	}
	if !addon.Enabled {
		t.Fatal("a health failure must never disable the addon itself")
	}
}

// TestOneSlowAddonDoesNotBlockHealthyAddons is the core "parallel addon
// requests" guarantee: aggregating streams across a hanging addon and a
// healthy one must still return the healthy addon's result, and must not
// take anywhere near as long as the slow addon's own hang.
func TestOneSlowAddonDoesNotBlockHealthyAddons(t *testing.T) {
	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest.json" {
			json.NewEncoder(w).Encode(map[string]any{
				"id": "slow.addon", "name": "Trage bron", "version": "1.0",
				"resources": []string{"stream"},
				"catalogs":  []any{},
			})
			return
		}
		<-block
	}))
	// See the comment in TestAddonHealthTracksTimeoutAndConsecutiveFailures
	// for why close(block) must be deferred after (so it runs before)
	// slow.Close().
	defer slow.Close()
	defer close(block)

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			json.NewEncoder(w).Encode(map[string]any{
				"id": "fast.addon", "name": "Snelle bron", "version": "1.0",
				"resources": []string{"stream"},
				"catalogs":  []any{},
			})
		case "/stream/movie/tt1.json":
			json.NewEncoder(w).Encode(map[string]any{"streams": []any{
				map[string]any{"name": "1080p", "url": "https://media.example/tt1.m3u8"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fast.Close()

	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A single shared client reaches both test servers fine (different
	// hosts); the hub's addon timeout is what needs to be short here.
	hub := NewHub(store, "admin", "secret-password", http.DefaultClient)
	hub.SetAddonTimeout(100 * time.Millisecond)
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	adminToken := adminLogin(t, server.URL, "secret-password")
	doJSON(t, http.MethodPost, server.URL+"/v1/addons", adminToken,
		map[string]string{"manifestURL": slow.URL + "/manifest.json"}, http.StatusCreated, nil)
	doJSON(t, http.MethodPost, server.URL+"/v1/addons", adminToken,
		map[string]string{"manifestURL": fast.URL + "/manifest.json"}, http.StatusCreated, nil)

	start := time.Now()
	var payload struct {
		Streams []HubStream `json:"streams"`
	}
	doJSON(t, http.MethodGet, server.URL+"/v1/streams/movie/tt1", adminToken, nil, http.StatusOK, &payload)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("expected the slow addon's timeout to not inflate the overall request time, took %s", elapsed)
	}
	if len(payload.Streams) != 1 || payload.Streams[0].AddonName != "Snelle bron" {
		t.Fatalf("expected the healthy addon's stream despite the other addon hanging, got %#v", payload.Streams)
	}
}
