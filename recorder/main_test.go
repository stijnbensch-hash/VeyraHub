package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordingIsPersistentPrivateAndIdempotent(t *testing.T) {
	root := t.TempDir()
	r, err := newRecorder(filepath.Join(root, "data"), filepath.Join(root, "files"), "internal-test-token", "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(r.routes())
	defer server.Close()

	start := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	payload, _ := json.Marshal(createRequest{Title: "Evening news", Channel: "Test TV", URL: "https://example.invalid/live?key=private", Start: start, End: start.Add(time.Hour)})
	post := func() publicRecording {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/recordings", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer internal-test-token")
		req.Header.Set("X-Veyra-User-ID", "viewer-1")
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 && response.StatusCode != 201 {
			t.Fatalf("create: %d", response.StatusCode)
		}
		var result struct {
			Recording publicRecording `json:"recording"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result.Recording
	}
	first, second := post(), post()
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("duplicate schedule created a second recording: %q %q", first.ID, second.ID)
	}

	list := func(owner string, admin bool) []publicRecording {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/recordings", nil)
		req.Header.Set("Authorization", "Bearer internal-test-token")
		req.Header.Set("X-Veyra-User-ID", owner)
		if admin {
			req.Header.Set("X-Veyra-Admin", "true")
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result struct {
			Recordings []publicRecording `json:"recordings"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result.Recordings
	}
	if got := list("viewer-2", false); len(got) != 0 {
		t.Fatalf("other viewer saw recordings: %+v", got)
	}
	if got := list("admin", true); len(got) != 1 {
		t.Fatalf("admin should see one recording: %+v", got)
	}

	reopened, err := newRecorder(filepath.Join(root, "data"), filepath.Join(root, "files"), "internal-test-token", "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.jobs[first.ID]; got == nil || got.SourceURL != "https://example.invalid/live?key=private" {
		t.Fatal("scheduled recording was not persisted")
	}
}

func TestRecorderRequiresInternalTokenAndHubIdentity(t *testing.T) {
	r, err := newRecorder(filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "files"), "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token, owner string
		want         int
	}{
		{"", "viewer", 401}, {"wrong", "viewer", 401}, {"secret", "", 403}, {"secret", "viewer", 200},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/recordings", nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		if tc.owner != "" {
			req.Header.Set("X-Veyra-User-ID", tc.owner)
		}
		response := httptest.NewRecorder()
		r.routes().ServeHTTP(response, req)
		if response.Code != tc.want {
			t.Fatalf("token=%q owner=%q: got %d want %d", tc.token, tc.owner, response.Code, tc.want)
		}
	}
}
