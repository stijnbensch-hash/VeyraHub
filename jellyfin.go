package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
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
}

type stremioMeta struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Poster      string          `json:"poster"`
	Background  string          `json:"background"`
	ReleaseInfo string          `json:"releaseInfo"`
	Year        json.RawMessage `json:"year"`
	Videos      []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Name      string `json:"name"`
		Season    int    `json:"season"`
		Episode   int    `json:"episode"`
		Thumbnail string `json:"thumbnail"`
	} `json:"videos"`
}

func (h *Hub) registerJellyfinRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /System/Info/Public", h.jellyfinSystemInfo)
	mux.HandleFunc("POST /Users/AuthenticateByName", h.jellyfinAuthenticate)
	mux.HandleFunc("GET /Users/{userID}/Views", h.jellyfinProtected(h.jellyfinViews))
	mux.HandleFunc("GET /Users/{userID}/Items", h.jellyfinProtected(h.jellyfinItems))
	mux.HandleFunc("GET /Users/{userID}/Items/Latest", h.jellyfinProtected(h.jellyfinLatest))
	mux.HandleFunc("GET /Items/{itemID}/Images/{kind}", h.jellyfinProtected(h.jellyfinImage))
	mux.HandleFunc("GET /Items/{itemID}/Images/Backdrop/{index}", h.jellyfinProtected(h.jellyfinImage))
	mux.HandleFunc("GET /Videos/{itemID}/stream", h.jellyfinProtected(h.jellyfinStream))
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

func (h *Hub) jellyfinAuthenticate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username string `json:"Username"`
		Password string `json:"Pw"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusUnauthorized, "Gebruikersnaam of wachtwoord onjuist.")
		return
	}
	user, ok := h.store.Authenticate(strings.TrimSpace(input.Username), input.Password)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Gebruikersnaam of wachtwoord onjuist.")
		return
	}
	fields := embyAuthFields(r.Header.Get("X-Emby-Authorization"))
	session, accessToken, _, err := h.store.CreateSession(user.ID, fields["DeviceId"], fields["Device"])
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Aanmelden is mislukt.")
		return
	}
	state := h.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"User":        map[string]any{"Id": user.ID, "Name": user.Username},
		"AccessToken": accessToken, "ServerId": state.NodeID,
	})
	_ = session
}

func (h *Hub) jellyfinProtected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := jellyfinToken(r)
		_, user, ok := h.store.SessionByAccessToken(token)
		if !ok || (r.PathValue("userID") != "" && r.PathValue("userID") != user.ID) {
			writeError(w, http.StatusUnauthorized, "Een geldige mediaserversessie is vereist.")
			return
		}
		next(w, r)
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

func (h *Hub) jellyfinViews(w http.ResponseWriter, r *http.Request) {
	var items []map[string]any
	for _, addon := range h.store.Snapshot().Addons {
		if !addon.Enabled {
			continue
		}
		for _, catalog := range addon.Catalogs {
			id := encodeHubID(hubItemID{Kind: "library", AddonID: addon.ID, CatalogID: catalog.ID, MediaType: catalog.Type, Name: catalog.Name})
			collectionType := "movies"
			if catalog.Type == "series" {
				collectionType = "tvshows"
			}
			items = append(items, map[string]any{
				"Id": id, "Name": catalog.Name, "Type": "CollectionFolder", "CollectionType": collectionType,
			})
		}
	}
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"Items": items, "TotalRecordCount": len(items)})
}

func (h *Hub) jellyfinItems(w http.ResponseWriter, r *http.Request) {
	parentID := r.URL.Query().Get("ParentId")
	search := strings.TrimSpace(r.URL.Query().Get("SearchTerm"))
	limit := queryInt(r, "Limit", 100)
	var values []map[string]any

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
				values = h.jellyfinItemsFromMetas(addon.ID, metas)
			}
		case "item":
			if parent.MediaType == "series" {
				meta, _ := h.fetchMeta(r.Context(), parent.AddonID, "series", parent.MediaID)
				values = h.jellyfinEpisodes(meta, parent)
			}
		}
	} else if search != "" {
		values = h.searchCatalogs(r.Context(), search)
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

func (h *Hub) jellyfinLatest(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "Limit", 20)
	var values []map[string]any
	for _, addon := range h.store.Snapshot().Addons {
		if !addon.Enabled {
			continue
		}
		for _, catalog := range addon.Catalogs {
			metas, err := h.fetchCatalog(r.Context(), addon, catalog, "", 0)
			if err == nil {
				values = append(values, h.jellyfinItemsFromMetas(addon.ID, metas)...)
			}
			if len(values) >= limit {
				break
			}
		}
		if len(values) >= limit {
			break
		}
	}
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

func (h *Hub) jellyfinStream(w http.ResponseWriter, r *http.Request) {
	item, err := decodeHubID(r.PathValue("itemID"))
	if err != nil || item.MediaID == "" {
		http.NotFound(w, r)
		return
	}
	streams := h.aggregateStreams(r.Context(), item.MediaType, item.MediaID)
	if len(streams) == 0 {
		writeError(w, http.StatusNotFound, "Geen directe bron gevonden.")
		return
	}
	http.Redirect(w, r, streams[0].URL, http.StatusTemporaryRedirect)
}

func (h *Hub) fetchCatalog(ctx context.Context, addon Addon, catalog AddonCatalog, search string, skip int) ([]stremioMeta, error) {
	base, err := url.Parse(addon.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = path.Join(base.Path, "catalog", catalog.Type, catalog.ID)
	var extras []string
	if skip > 0 {
		extras = append(extras, "skip="+strconv.Itoa(skip))
	}
	if search != "" && contains(catalog.Extra, "search") {
		extras = append(extras, "search="+url.QueryEscape(search))
	}
	if len(extras) > 0 {
		base.Path += "/" + strings.Join(extras, "&") + ".json"
	} else {
		base.Path += ".json"
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	response, err := h.client.Do(request)
	if err != nil {
		h.store.RecordAddonHealth(addon.ID, false, sanitizeAddonError(err))
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		err := fmt.Errorf("catalog HTTP %d", response.StatusCode)
		h.store.RecordAddonHealth(addon.ID, false, sanitizeAddonError(err))
		return nil, err
	}
	var payload struct {
		Metas []stremioMeta `json:"metas"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload)
	if err != nil {
		h.store.RecordAddonHealth(addon.ID, false, sanitizeAddonError(err))
	} else {
		h.store.RecordAddonHealth(addon.ID, true, "")
	}
	return payload.Metas, err
}

func (h *Hub) fetchMeta(ctx context.Context, preferredAddonID, mediaType, id string) (stremioMeta, error) {
	meta, _, err := h.fetchMetaWithSource(ctx, preferredAddonID, mediaType, id)
	return meta, err
}

func (h *Hub) fetchMetaWithSource(ctx context.Context, preferredAddonID, mediaType, id string) (stremioMeta, string, error) {
	addons := h.store.Snapshot().Addons

	for pass := 0; pass < 2; pass++ {
		for _, addon := range addons {
			if !addon.Enabled || !contains(addon.Resources, "meta") || (pass == 0) != (addon.ID == preferredAddonID) {
				continue
			}

			base, err := url.Parse(addon.BaseURL)
			if err != nil {
				continue
			}

			base.Path = path.Join(base.Path, "meta", mediaType, id+".json")

			request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
			if err != nil {
				continue
			}

			response, err := h.client.Do(request)
			if err != nil {
				h.store.RecordAddonHealth(addon.ID, false, sanitizeAddonError(err))
				continue
			}

			if response.StatusCode < 200 || response.StatusCode >= 300 {
				response.Body.Close()
				h.store.RecordAddonHealth(
					addon.ID,
					false,
					fmt.Sprintf("metadata request returned HTTP %d", response.StatusCode),
				)
				continue
			}

			var payload struct {
				Meta stremioMeta `json:"meta"`
			}

			err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload)
			response.Body.Close()

			if err != nil {
				h.store.RecordAddonHealth(addon.ID, false, sanitizeAddonError(err))
				continue
			}

			if payload.Meta.ID == "" {
				h.store.RecordAddonHealth(addon.ID, false, "metadata response contained no media item")
				continue
			}

			h.store.RecordAddonHealth(addon.ID, true, "")
			return payload.Meta, addon.ID, nil
		}
	}

	return stremioMeta{}, "", fmt.Errorf("metadata not found")
}

func (h *Hub) searchCatalogs(ctx context.Context, query string) []map[string]any {
	var result []map[string]any
	for _, addon := range h.store.Snapshot().Addons {
		if !addon.Enabled {
			continue
		}
		for _, catalog := range addon.Catalogs {
			if !contains(catalog.Extra, "search") {
				continue
			}
			metas, err := h.fetchCatalog(ctx, addon, catalog, query, 0)
			if err == nil {
				result = append(result, h.jellyfinItemsFromMetas(addon.ID, metas)...)
			}
		}
	}
	return deduplicateJellyfinItems(result)
}

func (h *Hub) jellyfinItemsFromMetas(addonID string, metas []stremioMeta) []map[string]any {
	result := make([]map[string]any, 0, len(metas))
	for _, meta := range metas {
		if meta.ID == "" || (meta.Type != "movie" && meta.Type != "series") {
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
	for _, addon := range h.store.Snapshot().Addons {
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
