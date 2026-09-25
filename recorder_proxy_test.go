package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecorderProxyUsesHubSessionAndInternalToken(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/recordings" || r.Header.Get("Authorization") != "Bearer worker-secret" || r.Header.Get("X-Veyra-Admin") != "true" || r.Header.Get("X-Veyra-User-ID") == "" {
			t.Errorf("unexpected forwarded request: path=%s admin=%s owner-present=%t", r.URL.Path, r.Header.Get("X-Veyra-Admin"), r.Header.Get("X-Veyra-User-ID") != "")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"recordings": []any{}})
	}))
	defer worker.Close()
	t.Setenv("VEYRA_HUB_RECORDER_URL", worker.URL)
	t.Setenv("VEYRA_HUB_RECORDER_TOKEN", "worker-secret")

	store := newTestStore(t)
	hub := NewHub(store, "admin", "long-test-password", nil)
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	response, err := http.Get(server.URL + "/v1/recorder/recordings")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous access returned %d", response.StatusCode)
	}

	token := adminLogin(t, server.URL, "long-test-password")
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/recorder/recordings", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("authenticated access returned %d", response.StatusCode)
	}
}

func TestRecorderFileRouteAcceptsQueryToken(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/recordings/rec-1/file" {
			t.Errorf("unexpected forwarded path: %s", r.URL.Path)
		}
		w.Write([]byte("video-bytes"))
	}))
	defer worker.Close()
	t.Setenv("VEYRA_HUB_RECORDER_URL", worker.URL)
	t.Setenv("VEYRA_HUB_RECORDER_TOKEN", "worker-secret")

	store := newTestStore(t)
	hub := NewHub(store, "admin", "long-test-password", nil)
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	// No token at all: still rejected.
	response, err := http.Get(server.URL + "/v1/recorder/recordings/rec-1/file")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous file access returned %d", response.StatusCode)
	}

	token := adminLogin(t, server.URL, "long-test-password")
	response, err = http.Get(server.URL + "/v1/recorder/recordings/rec-1/file?access_token=" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("query-token file access returned %d", response.StatusCode)
	}
}

func TestRecorderProxyRejectsNonLoopbackUpstream(t *testing.T) {
	for _, address := range []string{"https://example.com:8480", "http://example.com:8480", "http://127.0.0.1:8480/path", "http://user:pass@127.0.0.1:8480"} {
		if proxy := newRecorderProxy(address, "secret"); proxy.base != nil {
			t.Fatalf("accepted unsafe recorder URL: %s", address)
		}
	}
}
