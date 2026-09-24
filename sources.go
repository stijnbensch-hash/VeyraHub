package main

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	errSourceBadURL           = errors.New("Gebruik een geldige HTTP(S)-URL.")
	errSourceCredentialsInURL = errors.New("Inloggegevens horen niet in de URL; gebruik de aparte velden.")
	errSourceRequiresHTTPS    = errors.New("Gebruik HTTPS; HTTP is alleen toegestaan op je privénetwerk.")
	errWebDAVListFailed       = errors.New("webdav listing failed")
)

const (
	sourceKindLocal  = "local"
	sourceKindWebDAV = "webdav"

	// maxSourceFiles bounds how many files a single source lists, so an
	// enormous folder/WebDAV share can't make catalog requests hang or
	// balloon the response.
	maxSourceFiles = 2000
)

var videoExtensions = map[string]bool{
	".mp4": true, ".mkv": true, ".mov": true, ".m4v": true,
	".avi": true, ".ts": true, ".webm": true, ".wmv": true,
}

// Source is an admin-configured content source outside the addon system:
// a local folder on the hub's own filesystem/Docker volume, or a WebDAV
// share. Unlike addons it has no manifest and no metadata beyond filenames
// — it exists for "my own rips", not for Stremio-catalog-style browsing.
type Source struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`               // "local" or "webdav"
	RootPath  string    `json:"rootPath,omitempty"` // local: absolute folder path inside the hub container
	BaseURL   string    `json:"baseURL,omitempty"`  // webdav: e.g. https://nas.local/dav/movies
	Username  string    `json:"username,omitempty"` // webdav
	Password  string    `json:"-"`                  // webdav; never serialized back to the API
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

func isSourceKind(value string) bool {
	return value == sourceKindLocal || value == sourceKindWebDAV
}

// sourceFile is one playable file found under a Source.
type sourceFile struct {
	// RelativePath is relative to the source's root (local folder or
	// WebDAV base), always using forward slashes.
	RelativePath string
	Size         int64
}

func (s *Store) Sources() []Source {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Source{}, s.state.Sources...)
}

func (s *Store) SourceByID(id string) (Source, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, source := range s.state.Sources {
		if source.ID == id {
			return source, true
		}
	}
	return Source{}, false
}

func (s *Store) CreateSource(source Source) (Source, error) {
	source.ID = randomID()
	source.CreatedAt = time.Now().UTC()
	source.Enabled = true

	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Sources = append(s.state.Sources, source)
	if err := s.persistLocked(); err != nil {
		return Source{}, err
	}
	return source, nil
}

func (s *Store) DeleteSource(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Sources {
		if s.state.Sources[i].ID == id {
			s.state.Sources = append(s.state.Sources[:i], s.state.Sources[i+1:]...)
			return s.persistLocked()
		}
	}
	return os.ErrNotExist
}

func (s *Store) SetSourceEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Sources {
		if s.state.Sources[i].ID == id {
			s.state.Sources[i].Enabled = enabled
			return s.persistLocked()
		}
	}
	return os.ErrNotExist
}

// redactedSource strips the WebDAV password before a Source is ever
// written to an HTTP response (Password already has json:"-", this also
// covers any future field added the same way).
func redactedSource(source Source) Source {
	source.Password = ""
	return source
}

func (h *Hub) registerSourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sources", h.requireAdmin(h.listSources))
	mux.HandleFunc("POST /v1/sources", h.requireAdmin(h.createSource))
	mux.HandleFunc("PATCH /v1/sources/{id}", h.requireAdmin(h.patchSource))
	mux.HandleFunc("DELETE /v1/sources/{id}", h.requireAdmin(h.deleteSource))
	mux.HandleFunc("GET /api/v1/sources/{id}/files", h.requireSession(h.nativeSourceFiles))
	mux.HandleFunc("GET /v1/sources/{id}/file/{path...}", h.jellyfinProtected(h.sourceFile))
}

func (h *Hub) listSources(w http.ResponseWriter, r *http.Request) {
	sources := h.store.Sources()
	redacted := make([]Source, len(sources))
	for i, source := range sources {
		redacted[i] = redactedSource(source)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": redacted})
}

func (h *Hub) createSource(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		RootPath string `json:"rootPath"`
		BaseURL  string `json:"baseURL"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if decodeJSON(r, &input) != nil {
		writeError(w, http.StatusBadRequest, "Ongeldige aanvraag.")
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "Geef de bron een naam.")
		return
	}
	if !isSourceKind(input.Kind) {
		writeError(w, http.StatusBadRequest, "Gebruik kind local of webdav.")
		return
	}

	source := Source{Name: name, Kind: input.Kind}

	switch input.Kind {
	case sourceKindLocal:
		root := strings.TrimSpace(input.RootPath)
		if !filepath.IsAbs(root) {
			writeError(w, http.StatusBadRequest, "Geef een absoluut pad op (bv. /data/films).")
			return
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			writeError(w, http.StatusBadRequest, "De map bestaat niet of is niet leesbaar door de hub.")
			return
		}
		source.RootPath = root

	case sourceKindWebDAV:
		parsed, err := validateWebDAVBase(input.BaseURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		source.BaseURL = strings.TrimSuffix(parsed.String(), "/")
		source.Username = strings.TrimSpace(input.Username)
		source.Password = input.Password
	}

	created, err := h.store.CreateSource(source)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "De bron kon niet worden opgeslagen.")
		return
	}
	writeJSON(w, http.StatusCreated, redactedSource(created))
}

func validateWebDAVBase(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errSourceBadURL
	}
	if parsed.User != nil {
		return nil, errSourceCredentialsInURL
	}
	if parsed.Scheme == "http" && !isPrivateHost(parsed.Hostname()) {
		return nil, errSourceRequiresHTTPS
	}
	return parsed, nil
}

func (h *Hub) patchSource(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if decodeJSON(r, &input) != nil || input.Enabled == nil {
		writeError(w, http.StatusBadRequest, "Geef enabled op.")
		return
	}
	if err := h.store.SetSourceEnabled(r.PathValue("id"), *input.Enabled); err != nil {
		writeError(w, http.StatusNotFound, "Bron niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Hub) deleteSource(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteSource(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "Bron niet gevonden.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listSourceFiles lists the playable files under a source: a bounded
// recursive walk for a local folder, a single-level PROPFIND for WebDAV
// (kept non-recursive to bound the request; nested folders can be added as
// their own source).
func listSourceFiles(ctx context.Context, source Source, client *http.Client) ([]sourceFile, error) {
	switch source.Kind {
	case sourceKindLocal:
		return listLocalFiles(source.RootPath)
	case sourceKindWebDAV:
		return listWebDAVFiles(ctx, source, client)
	default:
		return nil, nil
	}
}

func listLocalFiles(root string) ([]sourceFile, error) {
	var result []sourceFile
	err := filepath.WalkDir(root, func(walkPath string, entry os.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable entries rather than failing the whole listing.
			return nil
		}
		if len(result) >= maxSourceFiles {
			return filepath.SkipAll
		}
		if entry.IsDir() {
			return nil
		}
		if !videoExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			return nil
		}
		relative, err := filepath.Rel(root, walkPath)
		if err != nil {
			return nil
		}
		info, err := entry.Info()
		var size int64
		if err == nil {
			size = info.Size()
		}
		result = append(result, sourceFile{RelativePath: filepath.ToSlash(relative), Size: size})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RelativePath < result[j].RelativePath })
	return result, nil
}

// davMultistatus is the minimal shape needed from a WebDAV PROPFIND
// response to list files: every other property is ignored.
type davMultistatus struct {
	XMLName   xml.Name      `xml:"multistatus"`
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string `xml:"href"`
	Propstat struct {
		Prop struct {
			ContentLength int64 `xml:"getcontentlength"`
			ResourceType  struct {
				Collection *struct{} `xml:"collection"`
			} `xml:"resourcetype"`
		} `xml:"prop"`
	} `xml:"propstat"`
}

func listWebDAVFiles(ctx context.Context, source Source, client *http.Client) ([]sourceFile, error) {
	request, err := http.NewRequestWithContext(ctx, "PROPFIND", source.BaseURL+"/", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Depth", "1")
	request.Header.Set("Content-Type", "application/xml")
	if source.Username != "" {
		request.SetBasicAuth(source.Username, source.Password)
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusMultiStatus && response.StatusCode != http.StatusOK {
		return nil, errWebDAVListFailed
	}

	var payload davMultistatus
	if err := xml.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&payload); err != nil {
		return nil, err
	}

	baseURL, err := url.Parse(source.BaseURL + "/")
	if err != nil {
		return nil, err
	}

	var result []sourceFile
	for _, item := range payload.Responses {
		if item.Propstat.Prop.ResourceType.Collection != nil {
			continue
		}
		hrefURL, err := url.Parse(item.Href)
		if err != nil {
			continue
		}
		resolved := baseURL.ResolveReference(hrefURL)
		relative := strings.TrimPrefix(resolved.Path, baseURL.Path)
		relative = strings.TrimPrefix(relative, "/")
		if relative == "" || !videoExtensions[strings.ToLower(path.Ext(relative))] {
			continue
		}
		if len(result) >= maxSourceFiles {
			break
		}
		result = append(result, sourceFile{RelativePath: relative, Size: item.Propstat.Prop.ContentLength})
	}
	return result, nil
}

// jellyfinSourceItems lists a Source's playable files in Jellyfin item
// shape, for jellyfinItems' "localSource" branch.
func (h *Hub) jellyfinSourceItems(ctx context.Context, sourceID string) []map[string]any {
	source, ok := h.store.SourceByID(sourceID)
	if !ok || !source.Enabled {
		return nil
	}
	files, err := listSourceFiles(ctx, source, h.client)
	if err != nil {
		return nil
	}
	result := make([]map[string]any, 0, len(files))
	for _, file := range files {
		id := hubItemID{Kind: "localItem", AddonID: source.ID, Path: file.RelativePath, Name: path.Base(file.RelativePath)}
		result = append(result, map[string]any{
			"Id": encodeHubID(id), "Name": id.Name, "Type": "Movie", "Overview": "",
		})
	}
	return result
}

// localItemMediaSource builds the single Jellyfin MediaSource for a
// "localItem": a URL back to the hub's own /v1/sources/.../file route,
// carrying the caller's own access token so the player can fetch/seek it
// without any extra auth step. The hub, not the client, holds the WebDAV
// credentials (if any) — proxied server-side by sourceFile/proxyWebDAVFile.
func (h *Hub) localItemMediaSource(r *http.Request, item hubItemID) map[string]any {
	streamURL := h.selfBaseURL(r) + "/v1/sources/" + url.PathEscape(item.AddonID) + "/file/" + item.Path
	if token := jellyfinToken(r); token != "" {
		separator := "?"
		if strings.Contains(streamURL, "?") {
			separator = "&"
		}
		streamURL += separator + "api_key=" + url.QueryEscape(token)
	}
	return map[string]any{
		"Id":   encodeHubID(hubItemID{Kind: "localStream", AddonID: item.AddonID, Path: item.Path}),
		"Name": item.Name, "Path": streamURL, "Protocol": "Http", "Type": "Default",
		"Container": "", "IsRemote": true, "SupportsDirectPlay": true, "SupportsDirectStream": true,
		"SupportsTranscoding": false, "SupportsProbing": false,
		"RequiredHttpHeaders": map[string]string{}, "MediaStreams": []any{}, "MediaAttachments": []any{},
		"Formats": []string{}, "Bitrate": 0, "DefaultAudioStreamIndex": nil, "DefaultSubtitleStreamIndex": nil,
	}
}

// selfBaseURL reconstructs the hub's own externally-reachable base URL from
// the incoming request, for building a self-referencing stream URL. Honors
// X-Forwarded-Proto (the hub sits behind a TLS-terminating reverse proxy in
// every documented deployment) and falls back to the request's own scheme
// detection otherwise.
func (h *Hub) selfBaseURL(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host
}

func (h *Hub) nativeSourceFiles(w http.ResponseWriter, r *http.Request) {
	source, ok := h.store.SourceByID(r.PathValue("id"))
	if !ok || !source.Enabled {
		writeError(w, http.StatusNotFound, "Bron niet gevonden.")
		return
	}
	files, err := listSourceFiles(r.Context(), source, h.client)
	if err != nil {
		writeError(w, http.StatusBadGateway, "De bron kon niet worden gelezen.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "count": len(files)})
}

// sourceFile serves (local) or proxies (WebDAV, with Basic Auth the client
// never sees) one file's bytes, honoring Range requests so seeking works.
func (h *Hub) sourceFile(w http.ResponseWriter, r *http.Request) {
	source, ok := h.store.SourceByID(r.PathValue("id"))
	if !ok || !source.Enabled {
		http.NotFound(w, r)
		return
	}
	relativePath := r.PathValue("path")
	if relativePath == "" || strings.Contains(relativePath, "..") {
		http.NotFound(w, r)
		return
	}

	switch source.Kind {
	case sourceKindLocal:
		fullPath := filepath.Join(source.RootPath, filepath.FromSlash(relativePath))
		if !strings.HasPrefix(fullPath, filepath.Clean(source.RootPath)+string(os.PathSeparator)) {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, fullPath)

	case sourceKindWebDAV:
		h.proxyWebDAVFile(w, r, source, relativePath)

	default:
		http.NotFound(w, r)
	}
}

func (h *Hub) proxyWebDAVFile(w http.ResponseWriter, r *http.Request, source Source, relativePath string) {
	target := strings.TrimSuffix(source.BaseURL, "/") + "/" + relativePath

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "De bron kon niet worden bereikt.")
		return
	}
	if source.Username != "" {
		request.SetBasicAuth(source.Username, source.Password)
	}
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}

	response, err := h.client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, "De bron kon niet worden bereikt.")
		return
	}
	defer response.Body.Close()

	for _, header := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := response.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	io.Copy(w, response.Body)
}
