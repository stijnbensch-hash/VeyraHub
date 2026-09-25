package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// recorderProxy is an optional bridge to VeyraHub's own recorder process.
// The recorder listens on loopback, keeps its own data, and never shares the
// Strand Recorder service or VeyraHub's hub.json.
type recorderProxy struct {
	base   *url.URL
	token  string
	client *http.Client
}

func newRecorderProxy(rawURL, token string) *recorderProxy {
	p := &recorderProxy{token: token, client: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 15 * time.Second}}}
	if rawURL == "" || token == "" {
		return p
	}
	base, err := url.Parse(rawURL)
	if err != nil || base.Scheme != "http" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "" || base.Port() == "" {
		return p
	}
	if ip := net.ParseIP(base.Hostname()); ip == nil || !ip.IsLoopback() {
		return p
	}
	p.base = base
	return p
}

func (h *Hub) registerRecorderRoutes(mux *http.ServeMux) {
	proxy := newRecorderProxy(os.Getenv("VEYRA_HUB_RECORDER_URL"), os.Getenv("VEYRA_HUB_RECORDER_TOKEN"))
	for _, pattern := range []string{
		"GET /v1/recorder/status",
		"GET /v1/recorder/recordings",
		"POST /v1/recorder/recordings",
		"DELETE /v1/recorder/recordings/{id}",
		"POST /v1/recorder/recordings/{id}/stop",
	} {
		mux.HandleFunc(pattern, h.requireSession(proxy.forward))
	}
	// The file route is opened directly by a native media player (AVPlayer),
	// which cannot attach a custom Authorization header — see
	// requireSessionAllowQueryToken.
	mux.HandleFunc("GET /v1/recorder/recordings/{id}/file", h.requireSessionAllowQueryToken(proxy.forward))
}

func (p *recorderProxy) forward(w http.ResponseWriter, r *http.Request) {
	if p.base == nil {
		writeError(w, http.StatusServiceUnavailable, "VeyraHub Recorder is niet geconfigureerd.")
		return
	}
	user, _ := r.Context().Value(ctxUserKey).(User)
	path := strings.TrimPrefix(r.URL.Path, "/v1/recorder")
	target := *p.base
	target.Path = "/v1" + path
	requestContext := r.Context()
	if !strings.HasSuffix(path, "/file") {
		var cancel context.CancelFunc
		requestContext, cancel = context.WithTimeout(requestContext, 30*time.Second)
		defer cancel()
	}
	var body io.Reader = r.Body
	if r.Method == http.MethodPost && path == "/recordings" {
		body = http.MaxBytesReader(w, r.Body, 64<<10)
	}
	upstream, err := http.NewRequestWithContext(requestContext, r.Method, target.String(), body)
	if err != nil {
		writeError(w, 500, "Recorderaanvraag kon niet worden gemaakt.")
		return
	}
	upstream.Header.Set("Authorization", "Bearer "+p.token)
	upstream.Header.Set("X-Veyra-User-ID", user.ID)
	if user.Role == RoleAdmin {
		upstream.Header.Set("X-Veyra-Admin", "true")
	}
	if r.Method == http.MethodPost {
		upstream.Header.Set("Content-Type", "application/json")
	}
	if value := r.Header.Get("Range"); value != "" {
		upstream.Header.Set("Range", value)
	}
	response, err := p.client.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "VeyraHub Recorder is niet bereikbaar.")
		return
	}
	defer response.Body.Close()
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Accept-Ranges", "Content-Range"} {
		if value := response.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}
