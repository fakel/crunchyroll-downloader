package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The page is a handful of files with no build step, embedded so the program is
// still a single binary to copy to a server.
//
//go:embed web/static
var staticFiles embed.FS

// webServer holds what the handlers need: the job store, the data directory and
// the current settings.
type webServer struct {
	dataDir string
	store   *jobStore

	mu  sync.Mutex // serialises settings writes
	cfg webConfig
}

// logChunk bounds how much of a job's log one request returns, so a huge log
// can't stall the browser; the page asks again from the returned offset.
const logChunk = 64 * 1024

// serveWeb runs the web interface until the process is stopped. It's the whole
// of the --serve branch of main().
func serveWeb(listen, dataDir string) error {
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}

	cfg, err := loadWebConfig(dataDir)
	if err != nil {
		return err
	}
	if cfg.Listen == "" {
		cfg.Listen = listen
	}

	store, err := newJobStore(dataDir, cfg)
	if err != nil {
		return err
	}

	web := &webServer{dataDir: dataDir, store: store, cfg: cfg}
	stop := make(chan struct{})
	defer close(stop)
	go store.run(stop)
	// An empty notify only kicks the worker if a job was left queued (e.g. the
	// server restarted with work pending).
	store.wake()

	server := &http.Server{
		Addr:              listen,
		Handler:           web.routes(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("could not listen on %s: %w", listen, err)
	}
	log.Printf("web: listening on http://%s (data in %s)", listen, dataDir)

	err = server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// routes wires the API and the static page. Patterns use the method-aware
// mux from Go 1.22 and later, so an unsupported method is a 405 rather than a
// handler that has to check it.
func (w *webServer) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/config", w.handleGetConfig)
	mux.HandleFunc("PUT /api/config", w.handlePutConfig)

	// /api/state is the whole picture in one request, which is what the page
	// polls; the per-collection routes below are the same data, per section.
	mux.HandleFunc("GET /api/state", w.handleGetState)
	mux.HandleFunc("GET /api/jobs", w.handleListJobs)
	mux.HandleFunc("POST /api/jobs", w.handleCreateJob)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", w.handleCancelJob)
	mux.HandleFunc("POST /api/jobs/{id}/retry", w.handleRetryJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", w.handleDeleteJob)
	mux.HandleFunc("GET /api/jobs/{id}/log", w.handleJobLog)
	mux.HandleFunc("GET /api/jobs/{id}/log/download", w.handleJobLogDownload)

	// /api/series is the same wishlist under the name the plan used.
	mux.HandleFunc("POST /api/series", w.handleAddWishlist)
	mux.HandleFunc("DELETE /api/series/{id}", w.handleDeleteWishlist)
	mux.HandleFunc("POST /api/series/{id}/download", w.handleDownloadWishlistEntry)

	mux.HandleFunc("POST /api/wishlist", w.handleAddWishlist)
	mux.HandleFunc("POST /api/wishlist/download", w.handleDownloadWishlist)
	mux.HandleFunc("POST /api/wishlist/{id}/download", w.handleDownloadWishlistEntry)
	mux.HandleFunc("DELETE /api/wishlist/{id}", w.handleDeleteWishlist)

	mux.HandleFunc("POST /api/pause", w.handlePause)

	static, err := fs.Sub(staticFiles, "web/static")
	if err != nil {
		panic(err) // embedded at build time; a failure here is a build bug
	}
	mux.Handle("GET /", http.FileServerFS(static))

	return logRequests(mux)
}

// logRequests prints one line per request, which is what the service log shows
// while the page itself keeps no state.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: rw, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("web: %s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

// statusRecorder remembers the status code so it can be logged.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// writeJSON sends v as JSON. It's the only way responses are composed, so no
// handler can forget the content type.
func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(status)
	enc := json.NewEncoder(rw)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("web: could not write the response: %v", err)
	}
}

// writeError reports a failure the page can show. The message is meant to be
// read by a person, so handlers pass plain English rather than error chains.
func writeError(rw http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(rw, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// readJSON decodes a request body into v, rejecting bodies that aren't JSON.
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("the request body is empty")
	}
	return json.Unmarshal(body, v)
}

func (w *webServer) handleGetConfig(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	cfg := w.cfg
	w.mu.Unlock()
	writeJSON(rw, http.StatusOK, cfg.public())
}

// handlePutConfig saves the settings. Only fields present in the body change;
// the secrets are kept unless the body carries new ones, because the page never
// receives them in the first place.
func (w *webServer) handlePutConfig(rw http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(rw, http.StatusBadRequest, "could not read the settings: %v", err)
		return
	}
	if len(body) == 0 {
		writeError(rw, http.StatusBadRequest, "the request body is empty")
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	next, err := w.cfg.mergeUpdate(body)
	if err != nil {
		writeError(rw, http.StatusBadRequest, "could not read the settings: %v", err)
		return
	}
	if err := next.validate(); err != nil {
		writeError(rw, http.StatusBadRequest, "%v", err)
		return
	}
	if err := next.save(w.dataDir); err != nil {
		writeError(rw, http.StatusInternalServerError, "could not save the settings: %v", err)
		return
	}
	w.cfg = next
	w.store.setConfig(next)
	writeJSON(rw, http.StatusOK, next.public())
}

// jobOptionsFrom turns the current settings into the options a new job keeps.
func (w *webServer) jobOptionsFrom(season int) jobOptions {
	w.mu.Lock()
	cfg := w.cfg
	w.mu.Unlock()
	return jobOptions{
		Season:         season,
		VideoQuality:   cfg.VideoQuality,
		AudioQuality:   cfg.AudioQuality,
		AudioLang:      cfg.AudioLang,
		SubsLang:       cfg.SubsLang,
		CCLang:         cfg.CCLang,
		DownloadDelay:  cfg.DownloadDelay,
		AbsoluteNum:    cfg.AbsoluteNum,
		RenumberSeason: cfg.RenumberSeason,
	}
}

// validateURL accepts the two kinds of link the program understands, so a typo
// is refused when it's submitted rather than at the end of a failed run.
func validateURL(url string) error {
	contentType, contentID := parseUrl(url)
	if contentType == "" || contentID == "" {
		return errors.New("that doesn't look like a Crunchyroll link; paste a /series/… or /watch/… URL")
	}
	return nil
}

// handleGetState returns everything the page needs to draw itself: the wishlist,
// the jobs and the settings with their secrets masked.
func (w *webServer) handleGetState(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	cfg := w.cfg
	w.mu.Unlock()
	writeJSON(rw, http.StatusOK, map[string]any{
		"series": w.store.seriesSnapshot(),
		"paused": w.store.isPaused(),
		"config": cfg.public(),
	})
}

func (w *webServer) handleListJobs(rw http.ResponseWriter, r *http.Request) {
	writeJSON(rw, http.StatusOK, map[string]any{
		"series": w.store.seriesSnapshot(),
		"paused": w.store.isPaused(),
	})
}

// handleCreateJob queues an ad-hoc download (a URL pasted into the form, not a
// wishlist entry).
func (w *webServer) handleCreateJob(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		URL    string `json:"url"`
		Season int    `json:"season"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(rw, http.StatusBadRequest, "could not read the request: %v", err)
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	if err := validateURL(body.URL); err != nil {
		writeError(rw, http.StatusBadRequest, "%v", err)
		return
	}
	if body.Season < 0 {
		writeError(rw, http.StatusBadRequest, "the season has to be 0 (every season) or a positive number")
		return
	}

	job, err := w.store.enqueue(body.URL, "", w.jobOptionsFrom(body.Season), "")
	if err != nil {
		writeError(rw, http.StatusInternalServerError, "could not queue the download: %v", err)
		return
	}
	writeJSON(rw, http.StatusCreated, job)
}

func (w *webServer) handleCancelJob(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := w.store.cancelJob(id); err != nil {
		writeStoreError(rw, err)
		return
	}
	job, _ := w.store.jobByID(id)
	writeJSON(rw, http.StatusOK, job)
}

// handleRetryJob queues a fresh job with the same URL and options as an old one,
// which is how the page recovers from a failed or interrupted run.
func (w *webServer) handleRetryJob(rw http.ResponseWriter, r *http.Request) {
	old, ok := w.store.jobByID(r.PathValue("id"))
	if !ok {
		writeStoreError(rw, errNotFound)
		return
	}
	job, err := w.store.enqueue(old.URL, old.SeriesID, old.Options, old.Title)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, "could not queue the download: %v", err)
		return
	}
	writeJSON(rw, http.StatusCreated, job)
}

func (w *webServer) handleDeleteJob(rw http.ResponseWriter, r *http.Request) {
	if err := w.store.removeJob(r.PathValue("id")); err != nil {
		writeStoreError(rw, err)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}

// handleJobLog returns the tail of a job's log from the caller's offset, so the
// page can poll for new lines without re-reading everything. "done" tells it the
// job won't produce more.
func (w *webServer) handleJobLog(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := w.store.jobByID(id)
	if !ok {
		writeStoreError(rw, errNotFound)
		return
	}

	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if offset < 0 {
		offset = 0
	}

	var text string
	var next int64 = offset
	if job.LogPath != "" {
		file, err := os.Open(job.LogPath)
		if err == nil {
			defer file.Close()
			if _, err := file.Seek(offset, io.SeekStart); err == nil {
				chunk, err := io.ReadAll(io.LimitReader(file, logChunk))
				if err == nil {
					text = stripANSI(string(chunk))
					next = offset + int64(len(chunk))
				}
			}
		}
	}

	writeJSON(rw, http.StatusOK, map[string]any{
		"text":   text,
		"offset": next,
		"done":   job.Status.terminal() || (job.Status != statusRunning && job.Status != statusQueued && next <= offset),
	})
}

// handleJobLogDownload returns the whole log as a file download, for when the
// operator wants it outside the browser.
func (w *webServer) handleJobLogDownload(rw http.ResponseWriter, r *http.Request) {
	job, ok := w.store.jobByID(r.PathValue("id"))
	if !ok {
		writeStoreError(rw, errNotFound)
		return
	}
	if job.LogPath == "" {
		writeError(rw, http.StatusNotFound, "this job hasn't produced a log yet")
		return
	}
	body, err := os.ReadFile(job.LogPath)
	if err != nil {
		writeError(rw, http.StatusNotFound, "could not read the log: %v", err)
		return
	}
	rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
	rw.Header().Set("Content-Disposition", `attachment; filename="`+job.ID+`.log"`)
	rw.Write(body)
}

// stripANSI removes terminal escape sequences, which the program doesn't emit in
// plain mode but an older log file might still contain.
func stripANSI(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			out.WriteByte(s[i])
			continue
		}
		// Skip "ESC [ … m" style sequences.
		j := i + 1
		if j < len(s) && (s[j] == '[' || s[j] == ']') {
			j++
		}
		for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == ';') {
			j++
		}
		if j < len(s) {
			j++
		}
		i = j - 1
	}
	return out.String()
}

func (w *webServer) handleAddWishlist(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		URL    string `json:"url"`
		Title  string `json:"title"`
		Season int    `json:"season"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(rw, http.StatusBadRequest, "could not read the request: %v", err)
		return
	}
	body.URL = strings.TrimSpace(body.URL)
	if err := validateURL(body.URL); err != nil {
		writeError(rw, http.StatusBadRequest, "%v", err)
		return
	}
	if body.Season < 0 {
		writeError(rw, http.StatusBadRequest, "the season has to be 0 (every season) or a positive number")
		return
	}

	entry, err := w.store.addToWishlist(body.URL, strings.TrimSpace(body.Title), body.Season)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, "could not save the series: %v", err)
		return
	}
	// Adding to the wishlist means "I want this", so it starts downloading
	// straight away; the entry stays for the next "download everything" pass.
	job, err := w.store.enqueue(entry.URL, entry.ID, w.jobOptionsFrom(entry.Season), entry.Title)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, "could not queue the download: %v", err)
		return
	}
	writeJSON(rw, http.StatusCreated, map[string]any{"entry": entry, "job": job})
}

// handleDownloadWishlistEntry queues one series again. Episodes already on disk
// or on the server are skipped, so this is the "look for new episodes" button.
func (w *webServer) handleDownloadWishlistEntry(rw http.ResponseWriter, r *http.Request) {
	entry, ok := w.store.wishlistEntryByID(r.PathValue("id"))
	if !ok {
		writeStoreError(rw, errNotFound)
		return
	}
	job, err := w.store.enqueue(entry.URL, entry.ID, w.jobOptionsFrom(entry.Season), entry.Title)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, "could not queue the download: %v", err)
		return
	}
	writeJSON(rw, http.StatusCreated, job)
}

// handleDownloadWishlist queues every series in the list, in order. The queue is
// processed one job at a time, so this just fills it.
func (w *webServer) handleDownloadWishlist(rw http.ResponseWriter, r *http.Request) {
	wishlist, _, _ := w.store.snapshot()
	queued := make([]*Job, 0, len(wishlist))
	skipped := 0
	for _, entry := range wishlist {
		before := w.store.hasActiveJob(entry.URL)
		if before {
			skipped++
		}
		job, err := w.store.enqueue(entry.URL, entry.ID, w.jobOptionsFrom(entry.Season), entry.Title)
		if err != nil {
			writeError(rw, http.StatusInternalServerError, "could not queue %s: %v", entry.URL, err)
			return
		}
		if !before {
			queued = append(queued, job)
		}
	}
	writeJSON(rw, http.StatusCreated, map[string]any{"queued": len(queued), "alreadyRunning": skipped, "jobs": queued})
}

func (w *webServer) handleDeleteWishlist(rw http.ResponseWriter, r *http.Request) {
	if err := w.store.removeFromWishlist(r.PathValue("id")); err != nil {
		writeStoreError(rw, err)
		return
	}
	rw.WriteHeader(http.StatusNoContent)
}

func (w *webServer) handlePause(rw http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(rw, http.StatusBadRequest, "could not read the request: %v", err)
		return
	}
	if err := w.store.setPaused(body.Paused); err != nil {
		writeError(rw, http.StatusInternalServerError, "could not save the state: %v", err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]bool{"paused": body.Paused})
}

// writeStoreError maps a store error to the response the page expects.
func writeStoreError(rw http.ResponseWriter, err error) {
	if errors.Is(err, errNotFound) {
		writeError(rw, http.StatusNotFound, "not found")
		return
	}
	writeError(rw, http.StatusConflict, "%v", err)
}
