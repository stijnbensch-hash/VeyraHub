package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// This service has its own process, data file and recording directory. It does
// not read or write VeyraHub's hub.json or Strand Recorder's volumes.
type recording struct {
	ID         string    `json:"id"`
	OwnerID    string    `json:"ownerID"`
	Title      string    `json:"title"`
	Channel    string    `json:"channel,omitempty"`
	SourceURL  string    `json:"sourceURL"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Status     string    `json:"status"`
	Bytes      int64     `json:"bytes,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	ScanStatus string    `json:"scanStatus,omitempty"`
	AdBreaks   []adBreak `json:"adBreaks,omitempty"`
}

type adBreak struct {
	StartSeconds float64 `json:"startSeconds"`
	EndSeconds   float64 `json:"endSeconds"`
}

type publicRecording struct {
	ID         string    `json:"id"`
	OwnerID    string    `json:"ownerID"`
	Title      string    `json:"title"`
	Channel    string    `json:"channel,omitempty"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Status     string    `json:"status"`
	Bytes      int64     `json:"bytes,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	ScanStatus string    `json:"scanStatus,omitempty"`
	AdBreaks   []adBreak `json:"adBreaks,omitempty"`
}

func (r recording) public() publicRecording {
	return publicRecording{r.ID, r.OwnerID, r.Title, r.Channel, r.Start, r.End, r.Status, r.Bytes, r.Error, r.CreatedAt, r.ScanStatus, r.AdBreaks}
}

type recorder struct {
	mu            sync.Mutex
	jobs          map[string]*recording
	cancel        map[string]context.CancelFunc
	stopReason    map[string]string
	dataDir       string
	recordingsDir string
	token         string
	ffmpeg        string
	comskip       string
	comskipINI    string
	scanning      bool
	// retention is how long a completed recording is kept before it's
	// automatically deleted (file and entry), measured from its scheduled
	// end time. Zero disables automatic cleanup entirely.
	retention time.Duration
}

func newRecorder(dataDir, recordingsDir, token, ffmpeg string, retention time.Duration) (*recorder, error) {
	if token == "" {
		return nil, errors.New("internal token is required")
	}
	for _, dir := range []string{dataDir, recordingsDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	comskip, _ := exec.LookPath("comskip")
	comskipINI := env("VEYRA_RECORDER_COMSKIP_INI", "/etc/veyrahub-recorder/comskip.ini")
	if _, err := os.Stat(comskipINI); err != nil {
		comskip = ""
	}
	r := &recorder{jobs: map[string]*recording{}, cancel: map[string]context.CancelFunc{}, stopReason: map[string]string{}, dataDir: dataDir, recordingsDir: recordingsDir, token: token, ffmpeg: ffmpeg, comskip: comskip, comskipINI: comskipINI, retention: retention}
	data, err := os.ReadFile(filepath.Join(dataDir, "recordings.json"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(data) != 0 {
		var jobs []recording
		if err := json.Unmarshal(data, &jobs); err != nil {
			return nil, fmt.Errorf("invalid recorder data: %w", err)
		}
		for i := range jobs {
			job := jobs[i]
			if job.Status == "recording" {
				job.Status, job.Error = "failed", "Recording interrupted by service restart"
			}
			if job.ScanStatus == "running" {
				job.ScanStatus = "pending"
			}
			r.jobs[job.ID] = &job
		}
		if err := r.saveLocked(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *recorder) saveLocked() error {
	jobs := make([]recording, 0, len(r.jobs))
	for _, job := range r.jobs {
		jobs = append(jobs, *job)
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.dataDir, ".recordings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(r.dataDir, "recordings.json"))
}

func (r *recorder) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		value := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(value), []byte(r.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "Unauthorised")
			return
		}
		if req.Header.Get("X-Veyra-User-ID") == "" {
			writeError(w, http.StatusForbidden, "User identity required")
			return
		}
		next(w, req)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (r *recorder) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /v1/status", r.authorize(r.status))
	mux.HandleFunc("GET /v1/recordings", r.authorize(r.list))
	mux.HandleFunc("POST /v1/recordings", r.authorize(r.create))
	mux.HandleFunc("DELETE /v1/recordings/{id}", r.authorize(r.delete))
	mux.HandleFunc("POST /v1/recordings/{id}/stop", r.authorize(r.stop))
	mux.HandleFunc("GET /v1/recordings/{id}/file", r.authorize(r.file))
	return mux
}

func (r *recorder) status(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := map[string]int{}
	for _, job := range r.jobs {
		counts[job.Status]++
	}
	writeJSON(w, 200, map[string]any{"app": "veyrahub-recorder", "version": 1, "counts": counts, "ffmpegAvailable": r.ffmpeg != "", "comskipAvailable": r.comskip != ""})
}

func allowed(req *http.Request, job *recording) bool {
	return req.Header.Get("X-Veyra-Admin") == "true" || job.OwnerID == req.Header.Get("X-Veyra-User-ID")
}

func (r *recorder) list(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]publicRecording, 0, len(r.jobs))
	for _, job := range r.jobs {
		if allowed(req, job) {
			items = append(items, job.public())
		}
	}
	writeJSON(w, 200, map[string]any{"recordings": items})
}

type createRequest struct {
	Title   string    `json:"title"`
	Channel string    `json:"channel"`
	URL     string    `json:"url"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
}

func (r *recorder) create(w http.ResponseWriter, req *http.Request) {
	var input createRequest
	if err := json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&input); err != nil {
		writeError(w, 400, "Invalid recording request")
		return
	}
	input.Title, input.Channel = strings.TrimSpace(input.Title), strings.TrimSpace(input.Channel)
	u, err := url.Parse(input.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		writeError(w, 400, "An HTTP(S) stream URL is required")
		return
	}
	now := time.Now().UTC()
	if input.Title == "" || len(input.Title) > 200 || len(input.Channel) > 120 || input.End.Before(now) || !input.End.After(input.Start) || input.End.Sub(input.Start) > 12*time.Hour || input.Start.After(now.Add(30*24*time.Hour)) {
		writeError(w, 400, "Invalid title or recording time")
		return
	}
	if freeBytes(r.recordingsDir) < 2_000_000_000 {
		writeError(w, 507, "Not enough free space for recording")
		return
	}
	id := newID()
	job := &recording{ID: id, OwnerID: req.Header.Get("X-Veyra-User-ID"), Title: input.Title, Channel: input.Channel, SourceURL: input.URL, Start: input.Start.UTC(), End: input.End.UTC(), Status: "scheduled", CreatedAt: now}
	r.mu.Lock()
	for _, existing := range r.jobs {
		if existing.OwnerID == job.OwnerID && existing.SourceURL == job.SourceURL && existing.Start.Equal(job.Start) && existing.End.Equal(job.End) && (existing.Status == "scheduled" || existing.Status == "recording") {
			result := existing.public()
			r.mu.Unlock()
			writeJSON(w, 200, map[string]any{"recording": result})
			return
		}
	}
	r.jobs[id] = job
	err = r.saveLocked()
	if err != nil {
		delete(r.jobs, id)
	}
	r.mu.Unlock()
	if err != nil {
		writeError(w, 500, "Could not save recording")
		return
	}
	writeJSON(w, 201, map[string]any{"recording": job.public()})
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (r *recorder) delete(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	r.mu.Lock()
	job := r.jobs[id]
	if job == nil {
		r.mu.Unlock()
		writeError(w, 404, "Recording not found")
		return
	}
	if !allowed(req, job) {
		r.mu.Unlock()
		writeError(w, 403, "Forbidden")
		return
	}
	if cancel := r.cancel[id]; cancel != nil {
		r.stopReason[id] = "Recording deleted"
		cancel()
	}
	delete(r.jobs, id)
	err := r.saveLocked()
	r.mu.Unlock()
	if err != nil {
		writeError(w, 500, "Could not delete recording")
		return
	}
	r.removeFiles(id)
	w.WriteHeader(204)
}

func (r *recorder) stop(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	job := r.jobs[req.PathValue("id")]
	if job == nil {
		r.mu.Unlock()
		writeError(w, 404, "Recording not found")
		return
	}
	if !allowed(req, job) {
		r.mu.Unlock()
		writeError(w, 403, "Forbidden")
		return
	}
	if job.Status != "recording" {
		r.mu.Unlock()
		writeError(w, 409, "Recording is not active")
		return
	}
	if cancel := r.cancel[job.ID]; cancel != nil {
		r.stopReason[job.ID] = "Stopped by user"
		cancel()
	}
	r.mu.Unlock()
	writeJSON(w, 202, map[string]string{"status": "stopping"})
}

func (r *recorder) filePath(id string) string { return filepath.Join(r.recordingsDir, id+".ts") }

func (r *recorder) removeFiles(id string) {
	base := filepath.Join(r.recordingsDir, id)
	for _, suffix := range []string{".ts", ".edl", ".txt", ".log", ".logo.txt"} {
		_ = os.Remove(base + suffix)
	}
}

func (r *recorder) file(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	job := r.jobs[req.PathValue("id")]
	if job == nil {
		r.mu.Unlock()
		writeError(w, 404, "Recording not found")
		return
	}
	if !allowed(req, job) {
		r.mu.Unlock()
		writeError(w, 403, "Forbidden")
		return
	}
	name := job.Title + ".ts"
	path := r.filePath(job.ID)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeFilename(name)))
	http.ServeFile(w, req, path)
}

func safeFilename(value string) string {
	return strings.Map(func(c rune) rune {
		if c < 32 || c == '/' || c == '\\' || c == '"' {
			return '_'
		}
		return c
	}, value)
}

func (r *recorder) run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		for _, id := range r.tick() {
			r.removeFiles(id)
		}
		select {
		case <-ctx.Done():
			r.mu.Lock()
			for _, cancel := range r.cancel {
				cancel()
			}
			r.mu.Unlock()
			return
		case <-ticker.C:
		}
	}
}

// tick advances scheduled/running recordings and, if retention is set,
// removes completed recordings whose scheduled end time is older than the
// retention window. It returns the ids of recordings it just removed, whose
// files the caller should delete once the lock is released.
func (r *recorder) tick() []string {
	r.mu.Lock()
	now := time.Now()
	changed := false
	if freeBytes(r.recordingsDir) < 2_000_000_000 {
		for id, cancel := range r.cancel {
			r.stopReason[id] = "Stopped because storage is nearly full"
			cancel()
		}
	}
	for _, job := range r.jobs {
		if job.Status != "scheduled" {
			continue
		}
		if !job.End.After(now) {
			job.Status, job.Error = "failed", "Scheduled time has passed"
			changed = true
			continue
		}
		if job.Start.After(now) {
			continue
		}
		if r.ffmpeg == "" {
			job.Status, job.Error = "failed", "FFmpeg is not installed"
			changed = true
			continue
		}
		if len(r.cancel) >= 2 {
			continue
		}
		ctx, cancel := context.WithDeadline(context.Background(), job.End)
		r.cancel[job.ID] = cancel
		job.Status = "recording"
		changed = true
		go r.capture(ctx, job.ID, job.SourceURL, job.End.Sub(now))
	}
	if !r.scanning && r.comskip != "" {
		for _, job := range r.jobs {
			if job.Status == "completed" && job.ScanStatus == "pending" {
				r.scanning = true
				job.ScanStatus = "running"
				changed = true
				go r.scan(job.ID)
				break
			}
		}
	}
	var expired []string
	if r.retention > 0 {
		cutoff := now.Add(-r.retention)
		for id, job := range r.jobs {
			if job.Status == "completed" && job.End.Before(cutoff) {
				expired = append(expired, id)
			}
		}
		for _, id := range expired {
			delete(r.jobs, id)
			changed = true
		}
	}
	if changed {
		if err := r.saveLocked(); err != nil {
			slog.Error("recorder state save failed", "error", err)
		}
	}
	r.mu.Unlock()
	if len(expired) > 0 {
		slog.Info("recorder retention: removed expired recordings", "count", len(expired), "retentionDays", int(r.retention/(24*time.Hour)))
	}
	return expired
}

func (r *recorder) capture(ctx context.Context, id, source string, remaining time.Duration) {
	path := r.filePath(id)
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", source, "-t", fmt.Sprintf("%.0f", remaining.Seconds()), "-map", "0:v?", "-map", "0:a?", "-c", "copy", "-f", "mpegts", path}
	cmd := exec.CommandContext(ctx, r.ffmpeg, args...)
	// Source URLs can contain provider credentials; never log command arguments or stderr.
	err := cmd.Run()
	var size int64
	if info, statErr := os.Stat(path); statErr == nil {
		size = info.Size()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancel, id)
	reason := r.stopReason[id]
	delete(r.stopReason, id)
	job := r.jobs[id]
	if job == nil {
		r.removeFiles(id)
		return
	}
	job.Bytes = size
	if reason == "Stopped because storage is nearly full" {
		job.Status, job.Error = "failed", reason
	} else if size > 0 {
		job.Status, job.Error = "completed", ""
		if r.comskip != "" {
			job.ScanStatus = "pending"
		}
	} else if err != nil {
		job.Status, job.Error = "failed", "The stream could not be recorded"
	} else {
		job.Status, job.Error = "failed", "The stream produced no recording"
	}
	if saveErr := r.saveLocked(); saveErr != nil {
		slog.Error("recorder state save failed", "error", saveErr)
	}
}

func (r *recorder) scan(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	path := r.filePath(id)
	cmd := exec.CommandContext(ctx, r.comskip, "--ini="+r.comskipINI, path)
	cmd.Dir = r.recordingsDir
	// Comskip can echo input filenames and metadata; do not log stdout/stderr.
	err := cmd.Run()
	breaks, parseErr := parseEDL(strings.TrimSuffix(path, ".ts") + ".edl")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scanning = false
	if job := r.jobs[id]; job != nil {
		if err == nil && (parseErr == nil || errors.Is(parseErr, os.ErrNotExist)) {
			job.ScanStatus = "done"
			job.AdBreaks = breaks
		} else {
			job.ScanStatus = "failed"
		}
		if saveErr := r.saveLocked(); saveErr != nil {
			slog.Error("recorder state save failed", "error", saveErr)
		}
	} else {
		r.removeFiles(id)
	}
}

func parseEDL(path string) ([]adBreak, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var breaks []adBreak
	scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		start, err1 := strconv.ParseFloat(fields[0], 64)
		end, err2 := strconv.ParseFloat(fields[1], 64)
		if err1 == nil && err2 == nil && start >= 0 && end > start {
			breaks = append(breaks, adBreak{start, end})
		}
	}
	return breaks, scanner.Err()
}

func freeBytes(dir string) uint64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0
	}
	return stat.Bavail * uint64(stat.Bsize)
}

func main() {
	token := os.Getenv("VEYRA_RECORDER_TOKEN")
	dataDir := env("VEYRA_RECORDER_DATA", "/var/lib/veyrahub-recorder")
	recordingsDir := env("VEYRA_RECORDER_RECORDINGS", "/var/lib/veyrahub-recorder/files")
	retention := envDays("VEYRA_RECORDER_RETENTION_DAYS", 30)
	ffmpeg, _ := exec.LookPath("ffmpeg")
	r, err := newRecorder(dataDir, recordingsDir, token, ffmpeg, retention)
	if err != nil {
		slog.Error("recorder setup failed", "error", err)
		os.Exit(1)
	}
	if retention > 0 {
		slog.Info("recorder retention enabled", "days", int(retention/(24*time.Hour)))
	} else {
		slog.Info("recorder retention disabled (VEYRA_RECORDER_RETENTION_DAYS=0)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go r.run(ctx)
	server := &http.Server{Addr: env("VEYRA_RECORDER_ADDRESS", "127.0.0.1:8480"), Handler: r.routes(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	slog.Info("VeyraHub recorder listening", "address", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("recorder stopped", "error", err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// envDays reads a day count from the environment and returns it as a
// Duration; 0 (or a negative/invalid value) disables whatever feature it
// configures.
func envDays(key string, fallbackDays int) time.Duration {
	days := fallbackDays
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			days = parsed
		}
	}
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}
