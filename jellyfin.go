package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
)

type hubItemID struct {
	Kind       string `json:"k"`
	AddonID    string `json:"a,omitempty"`
	CatalogID  string `json:"c,omitempty"`
	MediaType  string `json:"t,omitempty"`
	MediaID    string `json:"i,omitempty"`
	Name       string `json:"n,omitempty"`
	Overview   string `json:"o,omitempty"`
	Poster     string `json:"p,omitempty"`
	Backdrop   string `json:"b,omitempty"`
	Year       int    `json:"y,omitempty"`
	SeriesName string `json:"sn,omitempty"`
	SeriesID   string `json:"si,omitempty"`
	Season     int    `json:"s,omitempty"`
	Episode    int    `json:"e,omitempty"`

	// Path is a file's path relative to its Source's root/base, used by
	// Kind == "localItem" ("localSource" library, AddonID field reused to
	// carry the Source id — same neutral hubItemID shape, no new library
	// concept needed for it).
	Path string `json:"pa,omitempty"`

	// StreamRef identifies one specific stream among the (possibly several)
	// streams a single addon returns for the same title — e.g. a 1080p and
	// a 4K release from the same addon. Kind == "stream" MediaSource ids
	// carry this (see jellyfinPlaybackInfo/streamRef) so jellyfinStream can
	// redirect to the exact source a client picked instead of always the
	// first stream any addon happened to return.
	StreamRef string `json:"r,omitempty"`
}

// streamRef derives a short, stable identifier for one HubStream, used to
// tell apart multiple streams from the same addon within a MediaSource id.
// It hashes the stream's URL rather than embedding it so the id stays
// short, even though it still round-trips through client requests.
func streamRef(stream HubStream) string {
	hash := fnv.New32a()
	hash.Write([]byte(stream.URL))
	return strconv.FormatUint(uint64(hash.Sum32()), 36)
}

func (h *Hub) registerJellyfinRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /System/Info/Public", h.jellyfinSystemInfo)
	mux.HandleFunc("GET /Branding/Configuration", h.jellyfinBranding)
	mux.HandleFunc("GET /Users/Public", h.jellyfinPublicUsers)
	mux.HandleFunc("POST /Users/AuthenticateByName", h.jellyfinAuthenticate)
	mux.HandleFunc("GET /System/Info", h.jellyfinProtected(h.jellyfinSystemInfoFull))
	mux.HandleFunc("GET /Users/Me", h.jellyfinProtected(h.jellyfinUser))
	mux.HandleFunc("GET /Users/{userID}", h.jellyfinProtected(h.jellyfinUser))
	mux.HandleFunc("GET /Users/{userID}/Views", h.jellyfinProtected(h.jellyfinViews))
	mux.HandleFunc("GET /Users/{userID}/Items", h.jellyfinProtected(h.jellyfinItems))
	mux.HandleFunc("GET /Users/{userID}/Items/{itemID}", h.jellyfinProtected(h.jellyfinItemDetail))
	mux.HandleFunc("GET /Users/{userID}/Items/Latest", h.jellyfinProtected(h.jellyfinLatest))
	mux.HandleFunc("GET /Items/{itemID}", h.jellyfinProtected(h.jellyfinItemDetail))
	mux.HandleFunc("GET /Shows/{seriesID}/Episodes", h.jellyfinProtected(h.jellyfinShowEpisodes))
	mux.HandleFunc("GET /Items/{itemID}/Images/{kind}", h.jellyfinProtected(h.jellyfinImage))
	mux.HandleFunc("GET /Items/{itemID}/Images/Backdrop/{index}", h.jellyfinProtected(h.jellyfinImage))
	mux.HandleFunc("GET /Items/{itemID}/PlaybackInfo", h.jellyfinProtected(h.jellyfinPlaybackInfo))
	mux.HandleFunc("POST /Items/{itemID}/PlaybackInfo", h.jellyfinProtected(h.jellyfinPlaybackInfo))
	mux.HandleFunc("GET /Videos/{itemID}/stream", h.jellyfinProtected(h.jellyfinStream))
	mux.HandleFunc("POST /Sessions/Capabilities", h.jellyfinProtected(h.jellyfinAcceptNoContent))
	mux.HandleFunc("POST /Sessions/Capabilities/Full", h.jellyfinProtected(h.jellyfinAcceptNoContent))
	mux.HandleFunc("POST /Sessions/Playing", h.jellyfinProtected(h.jellyfinReportProgress))
	mux.HandleFunc("POST /Sessions/Playing/Progress", h.jellyfinProtected(h.jellyfinReportProgress))
	mux.HandleFunc("POST /Sessions/Playing/Stopped", h.jellyfinProtected(h.jellyfinReportProgress))
	mux.HandleFunc("POST /Users/{userID}/PlayedItems/{itemID}", h.jellyfinProtected(h.jellyfinMarkPlayed))
	mux.HandleFunc("DELETE /Users/{userID}/PlayedItems/{itemID}", h.jellyfinProtected(h.jellyfinMarkUnplayed))
	mux.HandleFunc("GET /DisplayPreferences/{id}", h.jellyfinProtected(h.jellyfinDisplayPreferences))
	mux.HandleFunc("POST /DisplayPreferences/{id}", h.jellyfinProtected(h.jellyfinAcceptNoContent))
}

func (h *Hub) jellyfinSystemInfo(w http.ResponseWriter, r *http.Request) {
	state := h.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"ServerName": "Veyra Hub", "ProductName": "Veyra Hub",
		"Version": "10.11.8", "Id": state.NodeID,
		"OperatingSystem": "linux", "StartupWizardCompleted": true,
		"VeyraHubVersion": version,
	})
}

// jellyfinBranding and jellyfinPublicUsers answer two unauthenticated calls
// most Jellyfin/Emby clients (not just Veyra's own) make before login: a
// branding check and a user-picker list. Returning defaults/an empty list
// makes a generic client fall through to its normal manual-login form
// instead of hanging or erroring on an unimplemented route.
func (h *Hub) jellyfinBranding(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"LoginDisclaimer": "", "CustomCss": "", "SplashscreenEnabled": false})
}

func (h *Hub) jellyfinPublicUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []map[string]any{})
}

// jellyfinSystemInfoFull is the authenticated counterpart of
// jellyfinSystemInfo (GET /System/Info/Public): the same identity, plus a
// couple of fields generic clients check post-login (e.g. transcoding
// support, which the hub never does — every stream is passed straight
// through from its addon).
func (h *Hub) jellyfinSystemInfoFull(w http.ResponseWriter, r *http.Request) {
	state := h.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"ServerName": "Veyra Hub", "ProductName": "Veyra Hub",
		"Version": "10.11.8", "Id": state.NodeID,
		"OperatingSystem": "linux", "StartupWizardCompleted": true,
		"VeyraHubVersion": version,
		"SupportsTranscoding": false, "SupportsHttps": false,
		"LocalAddress": "", "WanAddress": "",
	})
}

// jellyfinUser answers both GET /Users/Me and GET /Users/{userID}: the
// signed-in user's profile, as a generic client fetches it right after
// authenticating.
func (h *Hub) jellyfinUser(w http.ResponseWriter, r *http.Request) {
	user, _ := userFromContext(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"Id": user.ID, "Name": user.Username,
		"HasPassword": true, "HasConfiguredPassword": true, "HasConfiguredEasyPassword": false,
		"EnableAutoLogin": false,
		"Policy": map[string]any{
			"IsAdministrator": user.Role == RoleAdmin, "IsDisabled": !user.Enabled,
			"EnableLiveTvManagement": false, "EnableContentDeletion": false,
		},
		"Configuration": map[string]any{"PlayDefaultAudioTrack": true},
	})
}

// jellyfinAcceptNoContent answers write-only calls a generic client makes
// as part of its normal startup/playback flow but whose content the hub has
// no use for (reported player capabilities, display-preference writes):
// accepting them with 204 lets the client continue instead of treating an
// unimplemented route as a hard failure.
func (h *Hub) jellyfinAcceptNoContent(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// jellyfinDisplayPreferences answers a generic client's startup read of its
// UI settings with a minimal, valid-shaped default so it doesn't treat a
// 404 as corrupt state; the hub doesn't otherwise store per-client display
// preferences.
func (h *Hub) jellyfinDisplayPreferences(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"Id": r.PathValue("id"), "ViewType": "movies", "SortBy": "SortName",
		"RememberIndexing": false, "RememberSorting": false, "CustomPrefs": map[string]string{},
	})
}

// jellyfinItemDetail answers GET /Items/{itemID} and GET
// /Users/{userID}/Items/{itemID}: metadata for one item or one browsable
// folder (library/smart collection/local source), decoded straight from the
// hub's self-describing item id rather than a fresh catalog lookup, since
// jellyfinItemsFromMetas already embedded everything a client needs.
func (h *Hub) jellyfinItemDetail(w http.ResponseWriter, r *http.Request) {
	item, err := decodeHubID(r.PathValue("itemID"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch item.Kind {
	case "item":
		writeJSON(w, http.StatusOK, jellyfinItem(item))
	case "library", "smartCollection", "localSource":
		writeJSON(w, http.StatusOK, map[string]any{
			"Id": r.PathValue("itemID"), "Name": item.Name, "Type": "CollectionFolder",
		})
	default:
		http.NotFound(w, r)
	}
}

// jellyfinShowEpisodes answers GET /Shows/{seriesID}/Episodes, the route
// many generic clients use for a series' episode list instead of paging
// GET /Users/{userID}/Items with ParentId=<series>.
func (h *Hub) jellyfinShowEpisodes(w http.ResponseWriter, r *http.Request) {
	series, err := decodeHubID(r.PathValue("seriesID"))
	values := []map[string]any{}
	if err == nil && series.Kind == "item" && series.MediaType == "series" {
		meta, _ := h.fetchMeta(r.Context(), series.AddonID, "series", series.MediaID)
		if episodes := h.jellyfinEpisodes(meta, series); episodes != nil {
			values = episodes
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"Items": values, "TotalRecordCount": len(values)})
}

// jellyfinReportProgress answers the three playback-reporting calls a
// generic client makes while and after playing (Sessions/Playing[/Progress
// or /Stopped]): it decodes the same hub item id PlaybackInfo/stream handed
// out and feeds the reported position into the same progress store the
// native API's /progress routes use, so resume position stays in sync
// however a client watches through the hub. Ticks are Jellyfin's playback
// position unit: 100 nanoseconds each.
func (h *Hub) jellyfinReportProgress(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ItemId        string  `json:"ItemId"`
		PositionTicks float64 `json:"PositionTicks"`
	}
	_ = decodeJSON(r, &input)
	item, err := decodeHubID(input.ItemId)
	if err == nil && item.MediaType != "" && item.MediaID != "" {
		user, _ := userFromContext(r)
		const ticksPerSecond = 10_000_000
		_, _ = h.store.SetProgressForUser(user.ID, item.MediaType, item.MediaID, input.PositionTicks/ticksPerSecond, 0)
	}
	w.WriteHeader(http.StatusNoContent)
}

// jellyfinMarkPlayed/jellyfinMarkUnplayed answer the "mark watched"/"mark
// unwatched" toggle (POST/DELETE /Users/{userID}/PlayedItems/{itemID}) a
// generic client's UI offers outside of just playing something through.
// Marking played stores a finished progress record — the same state
// finishing a title through normal playback reporting would leave — rather
// than adding a separate watched-flag concept to the store.
func (h *Hub) jellyfinMarkPlayed(w http.ResponseWriter, r *http.Request) {
	item, err := decodeHubID(r.PathValue("itemID"))
	if err == nil && item.MediaType != "" && item.MediaID != "" {
		user, _ := userFromContext(r)
		_, _ = h.store.SetProgressForUser(user.ID, item.MediaType, item.MediaID, 1, 1)
	}
	writeJSON(w, http.StatusOK, map[string]any{"Played": true})
}

func (h *Hub) jellyfinMarkUnplayed(w http.ResponseWriter, r *http.Request) {
	item, err := decodeHubID(r.PathValue("itemID"))
	if err == nil && item.MediaType != "" && item.MediaID != "" {
		user, _ := userFromContext(r)
		_ = h.store.ClearProgressForUser(user.ID, item.MediaType, item.MediaID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"Played": false})
}

func (h *Hub) jellyfinAuthenticate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"Username"`
		Password string `json:"Pw"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusUnauthorized, "Gebruikersnaam of wachtwoord onjuist.")
		return
	}

	username := strings.TrimSpace(input.Username)
	ip := clientIP(r)

	if writeIfLoginLocked(w, h.loginLimiter, ip, username) {
		return
	}

	user, ok := h.store.Authenticate(username, input.Password)
	if !ok {
		h.loginLimiter.recordFailure(ip, username)
		writeError(w, http.StatusUnauthorized, "Gebruikersnaam of wachtwoord onjuist.")
		return
	}
	h.loginLimiter.recordSuccess(ip, username)
	fields := embyAuthFields(r.Header.Get("X-Emby-Authorization"))
	_, accessToken, err := h.store.CreateMediaSession(user.ID, fields["DeviceId"], fields["Device"])
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Aanmelden is mislukt.")
		return
	}
	state := h.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"User":        map[string]any{"Id": user.ID, "Name": user.Username},
		"AccessToken": accessToken, "ServerId": state.NodeID,
	})
}

func (h *Hub) jellyfinProtected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := jellyfinToken(r)
		session, user, ok := h.store.SessionByAccessToken(token)
		if !ok || (r.PathValue("userID") != "" && r.PathValue("userID") != user.ID) {
			writeError(w, http.StatusUnauthorized, "Een geldige mediaserversessie is vereist.")
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserKey, user)
		ctx = context.WithValue(ctx, ctxSessionKey, session)
		next(w, r.WithContext(ctx))
	}
}

// embyAuthFields parses the "X-Emby-Authorization" header Jellyfin/Emby
// clients send, e.g. `MediaBrowser Client="Veyra", Device="iPhone",
// DeviceId="abc", Version="1.0"`, into a key/value map.
func embyAuthFields(header string) map[string]string {
	fields := map[string]string{}
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(part[:eq])
		value := strings.Trim(strings.TrimSpace(part[eq+1:]), "\"")
		fields[key] = value
	}
	return fields
}

// jellyfinViews lists every browsable library as a Jellyfin "View" —
// one per addon catalog, smart collection and local/WebDAV source. Each
// entry also carries "GroupId"/"GroupName", a Veyra-Hub-specific extension
// (real Jellyfin clients simply ignore unknown fields) so a client can
// group libraries by where they come from — e.g. show an addon picker
// before a catalog picker, the way the native API already exposes addons
// separately. Smart collections and local sources aren't addons, so they
// get a fixed, synthetic group instead of an addon id/name.
func (h *Hub) jellyfinViews(w http.ResponseWriter, r *http.Request) {
	items := h.jellyfinViewItems()
	writeJSON(w, http.StatusOK, map[string]any{"Items": items, "TotalRecordCount": len(items)})
}

// jellyfinViewItems builds the same "View" list jellyfinViews answers with,
// factored out so jellyfinItems can hand it back for an unscoped browse
// (GET /Users/{userID}/Items with no ParentId) too — the same fallback
// real Jellyfin servers give a client that hasn't picked a library yet,
// rather than an empty list that reads as "nothing here".
func (h *Hub) jellyfinViewItems() []map[string]any {
	const smartCollectionsGroupID = "smart-collections"
	const smartCollectionsGroupName = "Slimme collecties"
	const localSourcesGroupID = "local-sources"
	const localSourcesGroupName = "Lokaal & WebDAV"

	var items []map[string]any
	for _, addon := range h.addonCatalog() {
		if !addon.Enabled {
			continue
		}
		for _, catalog := range addon.Catalogs {
			id := encodeHubID(hubItemID{Kind: "library", AddonID: addon.ID, CatalogID: catalog.ID, MediaType: catalog.Type, Name: catalog.Name})
			collectionType := "movies"
			switch catalog.Type {
			case "series":
				collectionType = "tvshows"
			case "sports", "tv":
				collectionType = "livetv"
			}
			items = append(items, map[string]any{
				"Id": id, "Name": catalog.Name, "Type": "CollectionFolder", "CollectionType": collectionType,
				"GroupId": addon.ID, "GroupName": addon.Name,
			})
		}
	}
	for _, collection := range h.store.SmartCollections() {
		id := encodeHubID(hubItemID{Kind: "smartCollection", CatalogID: collection.ID, MediaType: collection.MediaType, Name: collection.Name})
		collectionType := "movies"
		switch collection.MediaType {
		case "series":
			collectionType = "tvshows"
		case "sports", "tv":
			collectionType = "livetv"
		}
		items = append(items, map[string]any{
			"Id": id, "Name": collection.Name, "Type": "CollectionFolder", "CollectionType": collectionType,
			"GroupId": smartCollectionsGroupID, "GroupName": smartCollectionsGroupName,
		})
	}
	for _, source := range h.store.Sources() {
		if !source.Enabled {
			continue
		}
		id := encodeHubID(hubItemID{Kind: "localSource", AddonID: source.ID, Name: source.Name})
		items = append(items, map[string]any{
			"Id": id, "Name": source.Name, "Type": "CollectionFolder", "CollectionType": "movies",
			"GroupId": localSourcesGroupID, "GroupName": localSourcesGroupName,
		})
	}
	if items == nil {
		items = []map[string]any{}
	}
	return items
}

func (h *Hub) jellyfinItems(w http.ResponseWriter, r *http.Request) {
	parentID := r.URL.Query().Get("ParentId")
	search := strings.TrimSpace(r.URL.Query().Get("SearchTerm"))
	limit := queryInt(r, "Limit", 100)
	var values []map[string]any
	user, _ := userFromContext(r)
	filter := h.store.ContentFilterForUser(user.ID)

	if parentID != "" {
		parent, err := decodeHubID(parentID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Ongeldige bibliotheek-id.")
			return
		}
		switch parent.Kind {
		case "library":
			addon, catalog, ok := h.catalogByID(parent.AddonID, parent.CatalogID, parent.MediaType)
			if ok {
				metas, _ := h.fetchCatalog(r.Context(), addon, catalog, search, queryInt(r, "StartIndex", 0))
				values = h.jellyfinItemsFromMetas(addon.ID, filterMetas(metas, filter))
			}
		case "smartCollection":
			values = h.jellyfinSmartCollectionItems(r.Context(), parent.CatalogID, filter)
		case "localSource":
			values = h.jellyfinSourceItems(r.Context(), parent.AddonID)
		case "item":
			if parent.MediaType == "series" {
				meta, _ := h.fetchMeta(r.Context(), parent.AddonID, "series", parent.MediaID)
				values = h.jellyfinEpisodes(meta, parent)
			}
		}
	} else if search != "" {
		values = h.searchCatalogs(r.Context(), search, filter)
	} else if includeTypes := r.URL.Query().Get("IncludeItemTypes"); includeTypes == "" {
		// No ParentId, no SearchTerm and no type filter: a client browsing
		// without having picked a library yet. Real Jellyfin servers answer
		// this with the user's top-level items — here, the same library
		// list Views returns — rather than an empty page that reads as "no
		// content".
		values = h.jellyfinViewItems()
	} else {
		// No ParentId or SearchTerm, but a type filter (e.g.
		// IncludeItemTypes=Movie,Series): the client wants actual playable
		// items of those types, not folders. Returning the CollectionFolder
		// list from jellyfinViewItems here would always come back empty
		// once filterJellyfinTypes strips out anything that isn't a
		// Movie/Series/Episode, which is exactly what made an unscoped,
		// type-filtered browse (as Strand does) look like an empty library.
		values = h.jellyfinAllItems(r.Context(), filter, limit)
	}

	values = filterJellyfinTypes(values, r.URL.Query().Get("IncludeItemTypes"))
	if len(values) > limit {
		values = values[:limit]
	}
	if values == nil {
		values = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"Items": values, "TotalRecordCount": len(values)})
}

// jellyfinAllItems collects up to limit playable items (movies/series) across
// every enabled addon's catalogs, unfiltered by type — the caller applies
// filterJellyfinTypes with whatever IncludeItemTypes the request specified.
// Shared by jellyfinLatest and jellyfinItems' unscoped, type-filtered browse
// fallback (no ParentId, no SearchTerm, but an IncludeItemTypes filter).
func (h *Hub) jellyfinAllItems(ctx context.Context, filter ContentFilter, limit int) []map[string]any {
	var values []map[string]any
	for _, addon := range h.addonCatalog() {
		if !addon.Enabled {
			continue
		}
		for _, catalog := range addon.Catalogs {
			metas, err := h.fetchCatalog(ctx, addon, catalog, "", 0)
			if err == nil {
				values = append(values, h.jellyfinItemsFromMetas(addon.ID, filterMetas(metas, filter))...)
			}
			if len(values) >= limit {
				break
			}
		}
		if len(values) >= limit {
			break
		}
	}
	return values
}

func (h *Hub) jellyfinLatest(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "Limit", 20)
	user, _ := userFromContext(r)
	filter := h.store.ContentFilterForUser(user.ID)
	values := h.jellyfinAllItems(r.Context(), filter, limit)
	values = filterJellyfinTypes(values, r.URL.Query().Get("IncludeItemTypes"))
	if len(values) > limit {
		values = values[:limit]
	}
	if values == nil {
		values = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, values)
}

func (h *Hub) jellyfinImage(w http.ResponseWriter, r *http.Request) {
	item, err := decodeHubID(r.PathValue("itemID"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target := item.Poster
	if strings.EqualFold(r.PathValue("kind"), "Backdrop") {
		target = item.Backdrop
	}
	if target == "" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, target, http.StatusTemporaryRedirect)
}

func (h *Hub) jellyfinPlaybackInfo(w http.ResponseWriter, r *http.Request) {
	item, err := decodeHubID(r.PathValue("itemID"))
	if err != nil || (item.MediaID == "" && item.Kind != "localItem") {
		http.NotFound(w, r)
		return
	}
	if h.profileLimitExceeded(r) {
		writeError(w, http.StatusForbidden, "De dagelijkse kijklimiet van dit profiel is bereikt.")
		return
	}

	if item.Kind == "localItem" {
		writeJSON(w, http.StatusOK, map[string]any{
			"MediaSources":  []map[string]any{h.localItemMediaSource(r, item)},
			"PlaySessionId": "",
			"ErrorCode":     nil,
		})
		return
	}

	streams := h.aggregateStreams(r.Context(), item.MediaType, item.MediaID)

	mediaSources := make([]map[string]any, 0, len(streams))
	for index, stream := range streams {
		name := strings.TrimSpace(stream.Name)
		if name == "" {
			name = strings.TrimSpace(stream.Title)
		}
		if name == "" {
			name = "Bron " + strconv.Itoa(index+1)
		}

		sourceID := encodeHubID(hubItemID{
			Kind:      "stream",
			AddonID:   stream.AddonID,
			MediaType: item.MediaType,
			MediaID:   item.MediaID,
			Name:      name,
			StreamRef: streamRef(stream),
		})

		source := map[string]any{
			"Id":                         sourceID,
			"Name":                       name,
			"Path":                       stream.URL,
			"Protocol":                   "Http",
			"Type":                       "Default",
			"Container":                  "",
			"IsRemote":                   true,
			"SupportsDirectPlay":         true,
			"SupportsDirectStream":       true,
			"SupportsTranscoding":        false,
			"SupportsProbing":            false,
			"RequiredHttpHeaders":        map[string]string{},
			"MediaStreams":               []any{},
			"MediaAttachments":           []any{},
			"Formats":                    []string{},
			"Bitrate":                    0,
			"DefaultAudioStreamIndex":    nil,
			"DefaultSubtitleStreamIndex": nil,
			// Veyra-Hub-specifieke uitbreiding op de Jellyfin-MediaSource-vorm
			// (zoals GroupId/GroupName al bij Views bestaat): laat een
			// Veyra-client deze bron aan de addon toeschrijven die hem
			// leverde, i.p.v. alles onder de hub-servernaam te tonen. Een
			// echte Jellyfin/Emby-server stuurt deze velden niet mee.
			"AddonId":   stream.AddonID,
			"AddonName": stream.AddonName,
		}

		if stream.Filename != "" {
			source["Path"] = stream.URL
		}

		if stream.VideoSize > 0 {
			source["Size"] = stream.VideoSize
		}

		if stream.Description != "" {
			source["Name"] = name
		}

		mediaSources = append(mediaSources, source)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"MediaSources":  mediaSources,
		"PlaySessionId": "",
		"ErrorCode":     nil,
	})
}

func (h *Hub) jellyfinStream(w http.ResponseWriter, r *http.Request) {
	item, err := decodeHubID(r.PathValue("itemID"))
	if err != nil || (item.MediaID == "" && item.Kind != "localItem") {
		http.NotFound(w, r)
		return
	}
	if h.profileLimitExceeded(r) {
		writeError(w, http.StatusForbidden, "De dagelijkse kijklimiet van dit profiel is bereikt.")
		return
	}
	if item.Kind == "localItem" {
		source := h.localItemMediaSource(r, item)
		http.Redirect(w, r, source["Path"].(string), http.StatusTemporaryRedirect)
		return
	}
	streams := h.aggregateStreams(r.Context(), item.MediaType, item.MediaID)
	if len(streams) == 0 {
		writeError(w, http.StatusNotFound, "Geen directe bron gevonden.")
		return
	}
	http.Redirect(w, r, chosenStreamURL(streams, requestedMediaSourceID(r)), http.StatusTemporaryRedirect)
}

// requestedMediaSourceID reads the MediaSource id a client picked in
// PlaybackInfo and now wants to play, from wherever a Jellyfin/Emby client
// may send it: the standard "MediaSourceId" query parameter (Jellyfin's own
// clients vary its casing) or, for a POST-style PlaySessionId-only client,
// left blank so the caller falls back to the first stream.
func requestedMediaSourceID(r *http.Request) string {
	query := r.URL.Query()
	for _, key := range []string{"MediaSourceId", "mediaSourceId", "MediaSourceID"} {
		if value := strings.TrimSpace(query.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

// chosenStreamURL picks the URL to redirect to for a /stream request: the
// specific stream the client asked for via mediaSourceId (matched by its
// StreamRef, the same hash jellyfinPlaybackInfo used to build that source's
// id) if one was given and still present among the current streams,
// otherwise falls back to the first stream — the previous, only, behavior —
// so clients that call /stream directly without a prior PlaybackInfo still
// work.
func chosenStreamURL(streams []HubStream, mediaSourceID string) string {
	if mediaSourceID != "" {
		if chosen, err := decodeHubID(mediaSourceID); err == nil && chosen.Kind == "stream" {
			for _, stream := range streams {
				if streamRef(stream) == chosen.StreamRef {
					return stream.URL
				}
			}
		}
	}
	return streams[0].URL
}

func (h *Hub) searchCatalogs(ctx context.Context, query string, filter ContentFilter) []map[string]any {
	var result []map[string]any
	for _, addon := range h.addonCatalog() {
		if !addon.Enabled {
			continue
		}
		for _, catalog := range addon.Catalogs {
			if !contains(catalog.Extra, "search") {
				continue
			}
			metas, err := h.fetchCatalog(ctx, addon, catalog, query, 0)
			if err == nil {
				result = append(result, h.jellyfinItemsFromMetas(addon.ID, filterMetas(metas, filter))...)
			}
		}
	}
	return deduplicateJellyfinItems(result)
}

func (h *Hub) jellyfinItemsFromMetas(addonID string, metas []stremioMeta) []map[string]any {
	result := make([]map[string]any, 0, len(metas))
	for _, meta := range metas {
		if meta.ID == "" || !isMediaType(meta.Type) {
			continue
		}
		payload := hubItemID{Kind: "item", AddonID: addonID, MediaType: meta.Type, MediaID: meta.ID, Name: meta.Name, Overview: meta.Description, Poster: meta.Poster, Backdrop: meta.Background, Year: metaYear(meta)}
		result = append(result, jellyfinItem(payload))
	}
	return result
}

func (h *Hub) jellyfinEpisodes(meta stremioMeta, series hubItemID) []map[string]any {
	var result []map[string]any
	for _, video := range meta.Videos {
		if video.Season <= 0 || video.Episode <= 0 || video.ID == "" {
			continue
		}
		name := video.Title
		if name == "" {
			name = video.Name
		}
		payload := hubItemID{Kind: "item", AddonID: series.AddonID, MediaType: "series", MediaID: video.ID, Name: name, Poster: video.Thumbnail, Backdrop: series.Backdrop, SeriesName: series.Name, SeriesID: encodeHubID(series), Season: video.Season, Episode: video.Episode}
		result = append(result, jellyfinItem(payload))
	}
	return result
}

func jellyfinItem(item hubItemID) map[string]any {
	typeName := "Movie"
	if item.MediaType == "series" {
		typeName = "Series"
	}
	if item.Season > 0 {
		typeName = "Episode"
	}
	value := map[string]any{"Id": encodeHubID(item), "Name": item.Name, "Type": typeName, "Overview": item.Overview}
	if item.Year > 0 {
		value["ProductionYear"] = item.Year
	}
	if item.Poster != "" {
		value["ImageTags"] = map[string]string{"Primary": "remote"}
	}
	if item.Backdrop != "" {
		value["BackdropImageTags"] = []string{"remote"}
	}
	if typeName == "Episode" {
		value["SeriesName"] = item.SeriesName
		value["SeriesId"] = item.SeriesID
		value["ParentIndexNumber"] = item.Season
		value["IndexNumber"] = item.Episode
	}
	return value
}

func (h *Hub) catalogByID(addonID, catalogID, mediaType string) (Addon, AddonCatalog, bool) {
	for _, addon := range h.addonCatalog() {
		if addon.ID != addonID || !addon.Enabled {
			continue
		}
		for _, catalog := range addon.Catalogs {
			if catalog.ID == catalogID && catalog.Type == mediaType {
				return addon, catalog, true
			}
		}
	}
	return Addon{}, AddonCatalog{}, false
}

func encodeHubID(value hubItemID) string {
	data, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeHubID(value string) (hubItemID, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return hubItemID{}, err
	}
	var result hubItemID
	err = json.Unmarshal(data, &result)
	return result, err
}

func jellyfinToken(r *http.Request) string {
	if token := r.URL.Query().Get("api_key"); token != "" {
		return token
	}
	if token := r.Header.Get("X-Emby-Token"); token != "" {
		return token
	}
	header := r.Header.Get("X-Emby-Authorization")
	for _, key := range []string{"Token=\"", "token=\""} {
		if start := strings.Index(header, key); start >= 0 {
			value := header[start+len(key):]
			if end := strings.Index(value, "\""); end >= 0 {
				return value[:end]
			}
		}
	}
	return ""
}

func queryInt(r *http.Request, key string, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func metaYear(meta stremioMeta) int {
	var number int
	if json.Unmarshal(meta.Year, &number) == nil && number > 0 {
		return number
	}
	var text string
	if json.Unmarshal(meta.Year, &text) == nil {
		if value, _ := strconv.Atoi(firstFour(text)); value > 0 {
			return value
		}
	}
	if value, _ := strconv.Atoi(firstFour(meta.ReleaseInfo)); value > 0 {
		return value
	}
	return 0
}

func firstFour(value string) string {
	if len(value) >= 4 {
		return value[:4]
	}
	return value
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func filterJellyfinTypes(values []map[string]any, raw string) []map[string]any {
	if raw == "" {
		return values
	}
	wanted := map[string]bool{}
	for _, value := range strings.Split(raw, ",") {
		wanted[value] = true
	}
	var result []map[string]any
	for _, value := range values {
		if kind, _ := value["Type"].(string); wanted[kind] {
			result = append(result, value)
		}
	}
	return result
}

func deduplicateJellyfinItems(values []map[string]any) []map[string]any {
	seen := map[string]bool{}
	var result []map[string]any
	for _, value := range values {
		id, _ := value["Id"].(string)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, value)
	}
	return result
}
