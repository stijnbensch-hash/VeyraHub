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
		case "/stream/movie/tt3.json":
			json.NewEncoder(w).Encode(map[string]any{"streams": []any{
				map[string]any{"name": "1080p", "url": "https://media.example/tt3-1080p.m3u8"},
				map[string]any{"name": "4K", "url": "https://media.example/tt3-4k.m3u8"},
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

	// A different id than the first call: streams are now short-TTL cached
	// by (mediaType, id), so re-requesting tt1 here would just return the
	// already-cached (still "reachable") result without re-querying the
	// addon at all, and the transition below would never fire.
	doJSON(t, http.MethodGet, server.URL+"/v1/streams/movie/tt2", adminToken, nil, http.StatusOK, nil)

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

// TestStreamRedirectsToRequestedMediaSource covers task #26: the Jellyfin
// compatibility /Videos/{itemID}/stream endpoint must redirect to the exact
// MediaSource a client picked in PlaybackInfo (via ?MediaSourceId=...),
// not always the first stream any addon happened to return.
func TestStreamRedirectsToRequestedMediaSource(t *testing.T) {
	server, _, _ := setupHubWithAddon(t)

	// PlaybackInfo/stream sit behind the Jellyfin-compatibility auth (a
	// media session from /Users/AuthenticateByName), not the hub's own /v1
	// admin session — same as a real Jellyfin/Emby client.
	authBody, _ := json.Marshal(map[string]string{"Username": "admin", "Pw": "secret-password"})
	authRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/Users/AuthenticateByName", bytes.NewReader(authBody))
	authRequest.Header.Set("Content-Type", "application/json")
	authResponse, err := http.DefaultClient.Do(authRequest)
	if err != nil || authResponse.StatusCode != http.StatusOK {
		t.Fatalf("Jellyfin auth failed: %v status=%v", err, authResponse.StatusCode)
	}
	var jellyfinAuth struct {
		AccessToken string `json:"AccessToken"`
	}
	if err := json.NewDecoder(authResponse.Body).Decode(&jellyfinAuth); err != nil {
		t.Fatal(err)
	}
	authResponse.Body.Close()
	mediaToken := jellyfinAuth.AccessToken

	itemID := encodeHubID(hubItemID{Kind: "item", AddonID: "test.addon", MediaType: "movie", MediaID: "tt3"})

	playbackInfoRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/Items/"+itemID+"/PlaybackInfo", nil)
	playbackInfoRequest.Header.Set("X-Emby-Token", mediaToken)
	playbackInfoResponse, err := http.DefaultClient.Do(playbackInfoRequest)
	if err != nil || playbackInfoResponse.StatusCode != http.StatusOK {
		t.Fatalf("PlaybackInfo failed: %v status=%v", err, playbackInfoResponse.StatusCode)
	}
	var playbackInfo struct {
		MediaSources []map[string]any `json:"MediaSources"`
	}
	if err := json.NewDecoder(playbackInfoResponse.Body).Decode(&playbackInfo); err != nil {
		t.Fatal(err)
	}
	playbackInfoResponse.Body.Close()
	if len(playbackInfo.MediaSources) != 2 {
		t.Fatalf("expected 2 media sources for tt3, got %d", len(playbackInfo.MediaSources))
	}

	// PlaybackInfo's own MediaSource order (after ranking) is what "the
	// first stream" means; read it back rather than assuming which of the
	// two (1080p/4K) ranks first.
	firstPath, _ := playbackInfo.MediaSources[0]["Path"].(string)
	secondPath, _ := playbackInfo.MediaSources[1]["Path"].(string)
	if firstPath == "" || secondPath == "" || firstPath == secondPath {
		t.Fatalf("expected two distinct MediaSource paths, got %q and %q", firstPath, secondPath)
	}

	// Without a MediaSourceId, /stream keeps its old default of the first
	// (ranked) stream — clients that never called PlaybackInfo still work.
	noSourceClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/Videos/"+itemID+"/stream", nil)
	request.Header.Set("X-Emby-Token", mediaToken)
	response, err := noSourceClient.Do(request)
	if err != nil || response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected a redirect with no MediaSourceId: %v status=%v", err, response.StatusCode)
	}
	if got := response.Header.Get("Location"); got != firstPath {
		t.Fatalf("expected the default (first ranked) stream %q, got %q", firstPath, got)
	}

	// The second MediaSource's id should redirect to the second stream, not
	// the first — this is the actual bug being fixed.
	secondSourceID, _ := playbackInfo.MediaSources[1]["Id"].(string)
	request, _ = http.NewRequest(http.MethodGet, server.URL+"/Videos/"+itemID+"/stream?MediaSourceId="+secondSourceID, nil)
	request.Header.Set("X-Emby-Token", mediaToken)
	response, err = noSourceClient.Do(request)
	if err != nil || response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("expected a redirect with a MediaSourceId: %v status=%v", err, response.StatusCode)
	}
	if got := response.Header.Get("Location"); got != secondPath {
		t.Fatalf("expected the chosen (second) stream %q, got %q", secondPath, got)
	}
}

// TestPlaybackProgressRoundTrips covers task #25: a client can report a
// resume position and read it back, a position near the reported duration
// is treated as finished (and no longer offered as "resume"), and an
// explicit clear removes it.
func TestPlaybackProgressRoundTrips(t *testing.T) {
	server, _, adminToken := setupHubWithAddon(t)

	var initial struct {
		Found bool `json:"found"`
	}
	doJSON(t, http.MethodGet, server.URL+"/api/v1/items/movie/tt1/progress", adminToken, nil, http.StatusOK, &initial)
	if initial.Found {
		t.Fatalf("expected no progress yet, got %+v", initial)
	}

	// Below the minimum resumable position: stored, but not reported back
	// as something to resume from.
	var tooShort struct {
		PositionSeconds float64 `json:"positionSeconds"`
	}
	doJSON(t, http.MethodPut, server.URL+"/api/v1/items/movie/tt1/progress", adminToken,
		map[string]any{"positionSeconds": 5, "durationSeconds": 6000}, http.StatusOK, &tooShort)
	var afterTooShort struct {
		Found bool `json:"found"`
	}
	doJSON(t, http.MethodGet, server.URL+"/api/v1/items/movie/tt1/progress", adminToken, nil, http.StatusOK, &afterTooShort)
	if afterTooShort.Found {
		t.Fatalf("expected a 5s position to not be resumable, got %+v", afterTooShort)
	}

	// A real mid-playback position round-trips.
	doJSON(t, http.MethodPut, server.URL+"/api/v1/items/movie/tt1/progress", adminToken,
		map[string]any{"positionSeconds": 120, "durationSeconds": 6000}, http.StatusOK, nil)
	var midway struct {
		Found           bool    `json:"found"`
		PositionSeconds float64 `json:"positionSeconds"`
		DurationSeconds float64 `json:"durationSeconds"`
	}
	doJSON(t, http.MethodGet, server.URL+"/api/v1/items/movie/tt1/progress", adminToken, nil, http.StatusOK, &midway)
	if !midway.Found || midway.PositionSeconds != 120 || midway.DurationSeconds != 6000 {
		t.Fatalf("expected the stored position to round-trip, got %+v", midway)
	}

	// A position within 95% of the duration counts as finished and is no
	// longer offered as a resume point.
	var nearEnd struct {
		Finished bool `json:"finished"`
	}
	doJSON(t, http.MethodPut, server.URL+"/api/v1/items/movie/tt1/progress", adminToken,
		map[string]any{"positionSeconds": 5900, "durationSeconds": 6000}, http.StatusOK, &nearEnd)
	if !nearEnd.Finished {
		t.Fatalf("expected a near-end position to be marked finished")
	}
	var afterFinish struct {
		Found bool `json:"found"`
	}
	doJSON(t, http.MethodGet, server.URL+"/api/v1/items/movie/tt1/progress", adminToken, nil, http.StatusOK, &afterFinish)
	if afterFinish.Found {
		t.Fatalf("expected a finished title to no longer be resumable, got %+v", afterFinish)
	}

	// A separate title's progress is independent, and an explicit clear
	// removes it.
	doJSON(t, http.MethodPut, server.URL+"/api/v1/items/movie/tt2/progress", adminToken,
		map[string]any{"positionSeconds": 300, "durationSeconds": 6000}, http.StatusOK, nil)
	doJSON(t, http.MethodDelete, server.URL+"/api/v1/items/movie/tt2/progress", adminToken, nil, http.StatusNoContent, nil)
	var afterClear struct {
		Found bool `json:"found"`
	}
	doJSON(t, http.MethodGet, server.URL+"/api/v1/items/movie/tt2/progress", adminToken, nil, http.StatusOK, &afterClear)
	if afterClear.Found {
		t.Fatalf("expected a cleared position to no longer be found, got %+v", afterClear)
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

// TestLoginLimiterLocksAfterRepeatedFailures is a pure unit test of the
// loginLimiter type itself (no HTTP), covering the mechanics the login
// doors rely on: it locks a (ip, username) pair after loginMaxFailures
// failures, leaves other (ip, username) pairs unaffected, and a success
// clears the history.
func TestLoginLimiterLocksAfterRepeatedFailures(t *testing.T) {
	limiter := newLoginLimiter()

	for i := 0; i < loginMaxFailures-1; i++ {
		limiter.recordFailure("1.2.3.4", "admin")
		if locked, _ := limiter.locked("1.2.3.4", "admin"); locked {
			t.Fatalf("expected not locked after %d failure(s)", i+1)
		}
	}

	limiter.recordFailure("1.2.3.4", "admin")
	locked, remaining := limiter.locked("1.2.3.4", "admin")
	if !locked || remaining <= 0 {
		t.Fatalf("expected locked after %d failures, got locked=%v remaining=%v", loginMaxFailures, locked, remaining)
	}

	// Lockout is per (ip, username), not just per username: a different IP
	// trying the same account is unaffected.
	if otherLocked, _ := limiter.locked("5.6.7.8", "admin"); otherLocked {
		t.Fatalf("expected a different IP to not be locked out")
	}

	// A successful login clears the failure history.
	limiter.recordSuccess("1.2.3.4", "admin")
	if stillLocked, _ := limiter.locked("1.2.3.4", "admin"); stillLocked {
		t.Fatalf("expected recordSuccess to clear the lockout")
	}
}

// TestLoginLockoutAppliesAcrossBothAuthEndpoints covers task #27 end to
// end: repeated failed logins against /v1/auth/login lock out the
// (ip, username) pair — even a subsequent CORRECT password is refused — and
// the Jellyfin-compatibility login door (/Users/AuthenticateByName) shares
// that same lockout, since both check the same credentials.
func TestLoginLockoutAppliesAcrossBothAuthEndpoints(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "hub.json"))
	if err != nil {
		t.Fatal(err)
	}
	hub := NewHub(store, "admin", "secret-password", nil)
	server := httptest.NewServer(hub.Routes())
	defer server.Close()

	loginAttempt := func(password string) *http.Response {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"username": "admin", "password": password})
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/auth/login", bytes.NewReader(body))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	for i := 0; i < loginMaxFailures; i++ {
		response := loginAttempt("wrong-password")
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401, got %d", i+1, response.StatusCode)
		}
		response.Body.Close()
	}

	// One more attempt — even with the CORRECT password — should now be
	// locked out.
	response := loginAttempt("secret-password")
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 once locked out, got %d", response.StatusCode)
	}
	if response.Header.Get("Retry-After") == "" {
		t.Fatalf("expected a Retry-After header on a locked-out response")
	}
	response.Body.Close()

	// The Jellyfin-compatibility login door shares the same limiter.
	authBody, _ := json.Marshal(map[string]string{"Username": "admin", "Pw": "secret-password"})
	authRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/Users/AuthenticateByName", bytes.NewReader(authBody))
	authRequest.Header.Set("Content-Type", "application/json")
	authResponse, err := http.DefaultClient.Do(authRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer authResponse.Body.Close()
	if authResponse.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected the Jellyfin auth door to share the lockout, got %d", authResponse.StatusCode)
	}
}
