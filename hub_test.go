package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestAddonAndStreamAggregation(t *testing.T) {
	addon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			json.NewEncoder(w).Encode(map[string]any{
				"id": "test.addon", "name": "Test bron", "version": "1.0",
				"resources": []string{"catalog", "meta", "stream"},
				"catalogs":  []any{map[string]any{"id": "popular", "type": "movie", "name": "Populair"}},
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
	hub := NewHub(store, "admin", "secret", addon.Client())
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	body, _ := json.Marshal(map[string]string{"manifestURL": addon.URL + "/manifest.json"})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/addons", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("add addon failed: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/v1/streams/movie/tt123", nil)
	request.Header.Set("Authorization", "Bearer secret")
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

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/System/Info/Public", nil)
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("system info failed: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()

	authBody, _ := json.Marshal(map[string]string{"Username": "admin", "Pw": "secret"})
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/Users/AuthenticateByName", bytes.NewReader(authBody))
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("Jellyfin auth failed: %v status=%v", err, response.StatusCode)
	}
	response.Body.Close()

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/Users/user/Views", nil)
	request.Header.Set("X-Emby-Token", "secret")
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

	request, _ = http.NewRequest(http.MethodGet, server.URL+"/Users/user/Items?ParentId="+views.Items[0].ID+"&IncludeItemTypes=Movie", nil)
	request.Header.Set("X-Emby-Token", "secret")
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

func TestAuthenticationRequired(t *testing.T) {
	store, _ := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	request := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	response := httptest.NewRecorder()
	NewHub(store, "admin", "secret", nil).Routes().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", response.Code)
	}
}
