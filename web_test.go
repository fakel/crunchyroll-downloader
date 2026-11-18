package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubBinary writes an executable shell script that prints the given lines and
// exits with the given code. It stands in for the download program, which is how
// these tests check the runner and the log parser together without downloading
// anything.
func stubBinary(t *testing.T, exitCode int, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub-downloader")

	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	for _, line := range lines {
		script.WriteString("printf '%s\\n' " + shellQuote(line) + "\n")
	}
	script.WriteString("exit ")
	script.WriteString(itoa(exitCode))
	script.WriteString("\n")

	if err := os.WriteFile(path, []byte(script.String()), 0o755); err != nil {
		t.Fatalf("could not write the stub binary: %v", err)
	}
	return path
}

// shellQuote quotes a string for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// testStore returns a store in a temporary data directory, with settings that
// point at the given stub binary.
func testConfig() webConfig {
	cfg := defaultConfig()
	cfg.BinaryPath = "/bin/true"
	cfg.DownloadDir = os.TempDir()
	return cfg
}

func testStore(t *testing.T, binary string) *jobStore {
	t.Helper()
	dataDir := t.TempDir()
	cfg := defaultConfig()
	cfg.BinaryPath = binary
	cfg.DownloadDir = t.TempDir()

	return testStoreIn(t, dataDir, binary)
}

func testStoreIn(t *testing.T, dataDir, binary string) *jobStore {
	t.Helper()
	cfg := defaultConfig()
	cfg.BinaryPath = binary
	cfg.DownloadDir = t.TempDir()

	store, err := newJobStore(dataDir, cfg)
	if err != nil {
		t.Fatalf("newJobStore: %v", err)
	}
	return store
}

// runQueuedJob runs every queued job to completion, which is what the worker
// does one job at a time.
func runQueuedJob(t *testing.T, store *jobStore) *Job {
	t.Helper()
	stop := make(chan struct{})
	defer close(stop)
	if err := store.runNext(stop); err != nil {
		t.Fatalf("runNext: %v", err)
	}
	_, jobs, _ := store.snapshot()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	return &jobs[0]
}

// TestLogParserProgress feeds the exact lines the program prints and checks the
// progress the page would show.
func TestLogParserProgress(t *testing.T) {
	store := testStore(t, stubBinary(t, 0,
		"Downloading season 1 of Cowboy Bebop (2 episodes)",
		"",
		"Episode 1 is already downloaded as \"Cowboy Bebop S01E01 [720p].mkv\", skipping...",
		"Downloading: Asteroid Blues (S01E02) from Cowboy Bebop",
		"progress: video 42% 22.0 Mbps",
		"progress: audio 日本語 41% 0.2 Mbps",
		"Download finished! Output file: /tmp/x.mkv",
		"Uploading to 192.168.8.2:/boveda/series",
		"Uploaded Cowboy Bebop S01E02.mkv",
		"Removed the local copy.",
		"",
	))

	job, err := store.enqueue("https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop", "", jobOptions{Season: 1}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	finished := runQueuedJob(t, store)
	if finished.ID != job.ID {
		t.Fatalf("unexpected job %q", finished.ID)
	}
	if finished.Status != statusDone {
		t.Fatalf("status = %q, want %q (error: %s)", finished.Status, statusDone, finished.Err)
	}
	if finished.Title != "Cowboy Bebop" {
		t.Errorf("title = %q", finished.Title)
	}
	if finished.Total != 2 {
		t.Errorf("total = %d, want 2", finished.Total)
	}
	// One skipped + one downloaded.
	if finished.Done != 2 {
		t.Errorf("done = %d, want 2", finished.Done)
	}
	if finished.Current != "" {
		t.Errorf("current = %q, want empty after the episode finished", finished.Current)
	}
	if finished.Uploading {
		t.Error("uploading should be false after the upload finished")
	}
	if len(finished.Tracks) != 0 {
		t.Errorf("tracks = %v, want empty after the episode finished", finished.Tracks)
	}
	if finished.Err != "" {
		t.Errorf("err = %q, want empty", finished.Err)
	}
}

// TestLogParserWaiting checks the states shown while nothing is downloading. The
// parser is driven line by line because these states are transient: by the time
// the job finishes, they're gone.
func TestLogParserWaiting(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))
	job, err := store.enqueue("https://www.crunchyroll.com/series/GYEXQKJG6/dr-stone", "", jobOptions{}, "Dr. STONE")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	waiting := func() string {
		got, _ := store.jobByID(job.ID)
		return got.Waiting
	}

	store.applyLine(job.ID, "Downloading season 1 of Dr. STONE (24 episodes)")
	if got := waiting(); got != "" {
		t.Errorf("waiting after the season started = %q, want empty", got)
	}

	store.applyLine(job.ID, "Waiting 1m15s before the next download (rate-limit backoff)...")
	if got := waiting(); got != "waiting 1m15s before the next download (rate-limit backoff)..." {
		t.Errorf("waiting = %q", got)
	}

	store.applyLine(job.ID, "Recovered from error: playback error: 4294: rate limited by Crunchyroll")
	if got := waiting(); got != "recovering from: playback error: 4294: rate limited by Crunchyroll" {
		t.Errorf("waiting = %q", got)
	}

	store.applyLine(job.ID, "Retrying this episode in 5m0s [9:56PM]...")
	if got := waiting(); got != "rate limited by Crunchyroll, retrying in 5m0s" {
		t.Errorf("waiting = %q", got)
	}

	store.applyLine(job.ID, "Download of 5 failed (recovered from error: playback error: 4294), retrying in 15s [attempt 1/3]...")
	if got := waiting(); got != "retrying in 15s: recovered from error: playback error: 4294" {
		t.Errorf("waiting = %q", got)
	}

	store.applyLine(job.ID, "! Upload failed, keeping the local file: dial tcp: connection refused")
	if got := waiting(); got != "upload failed, keeping the local file: dial tcp: connection refused" {
		t.Errorf("waiting = %q", got)
	}

	// The next episode clears whatever the previous one was waiting for.
	store.applyLine(job.ID, "Downloading: Stone World (S01E02) from Dr. STONE")
	if got := waiting(); got != "" {
		t.Errorf("waiting after a new episode started = %q, want empty", got)
	}
}

// TestLogParserFailures checks the list the program prints when episodes could
// not be downloaded, which is what makes a finished run "failed".
func TestLogParserFailures(t *testing.T) {
	store := testStore(t, stubBinary(t, 0,
		"Downloading season 1 of Cowboy Bebop (1 episodes)",
		"",
		"1 episode(s) of Cowboy Bebop could not be downloaded:",
		"  S01E13 - Jupiter Jazz (Part 2) - Get \"https://www.crunchyroll.com/playback/v3/GJWU2JJQV/web/firefox/play\": read: connection reset by peer",
		"Running the same command again retries just these.",
		"",
	))

	store.enqueue("https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop", "", jobOptions{}, "")
	finished := runQueuedJob(t, store)

	if finished.Status != statusFailed {
		t.Fatalf("status = %q, want %q", finished.Status, statusFailed)
	}
	if len(finished.Failures) != 1 {
		t.Fatalf("failures = %v, want 1 entry", finished.Failures)
	}
	if !strings.HasPrefix(finished.Failures[0], "S01E13 - Jupiter Jazz") {
		t.Errorf("failure = %q", finished.Failures[0])
	}
}

// TestFailedRunKeepsTheReason checks that a run that exits non-zero reports the
// reason the program printed instead of "exit status 1".
func TestFailedRunKeepsTheReason(t *testing.T) {
	store := testStore(t, stubBinary(t, 1,
		"Couldn't log in: 401 Unauthorized",
		"Your \"-etp-rt\" cookie is wrong or expired.",
	))

	store.enqueue("https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop", "", jobOptions{}, "")
	finished := runQueuedJob(t, store)

	if finished.Status != statusFailed {
		t.Fatalf("status = %q, want %q", finished.Status, statusFailed)
	}
	if !strings.Contains(finished.Err, "Couldn't log in") {
		t.Errorf("err = %q, want the printed reason", finished.Err)
	}
}

// TestQueueOneAtATime checks that the worker only starts a queued job when asked
// for the next one, and that jobs keep the order they were added in.
func TestQueueOneAtATime(t *testing.T) {
	store := testStore(t, stubBinary(t, 0, "Downloading season 1 of X (1 episodes)"))

	first, _ := store.enqueue("https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop", "", jobOptions{}, "")
	second, _ := store.enqueue("https://www.crunchyroll.com/series/GYEXQKJG6/dr-stone", "", jobOptions{}, "")

	stop := make(chan struct{})
	defer close(stop)

	if err := store.runNext(stop); err != nil {
		t.Fatalf("runNext: %v", err)
	}
	_, jobs, _ := store.snapshot()
	byID := map[string]Job{}
	for _, job := range jobs {
		byID[job.ID] = job
	}
	if byID[first.ID].Status != statusDone {
		t.Errorf("first job status = %q, want done", byID[first.ID].Status)
	}
	if byID[second.ID].Status != statusQueued {
		t.Errorf("second job status = %q, want queued", byID[second.ID].Status)
	}

	if err := store.runNext(stop); err != nil {
		t.Fatalf("runNext: %v", err)
	}
	_, jobs, _ = store.snapshot()
	for _, job := range jobs {
		if job.Status != statusDone {
			t.Errorf("job %s status = %q, want done", job.ID, job.Status)
		}
	}
}

// TestJobArgs checks the flags a job turns into, since that mapping is the only
// thing between the settings and the verified download path.
func TestJobArgs(t *testing.T) {
	job := &Job{
		URL: "https://www.crunchyroll.com/series/GYEXQKJG6/dr-stone",
		Options: jobOptions{
			Season:         3,
			VideoQuality:   "720p",
			AudioQuality:   "128k",
			AudioLang:      "ja-JP",
			SubsLang:       "es-419,en-US",
			CCLang:         "en-US",
			DownloadDelay:  "2m",
			AbsoluteNum:    true,
			RenumberSeason: true,
		},
	}
	cfg := defaultConfig()
	cfg.DownloadDir = "/srv/downloads"

	want := []string{
		"--url", job.URL,
		"--output-dir", "/srv/downloads",
		"--season", "3",
		"--video-quality", "720p",
		"--audio-quality", "128k",
		"--audio-lang", "ja-JP",
		"--subs-lang", "es-419,en-US",
		"--cc-lang", "en-US",
		"--download-delay", "2m",
		"--absolute-numbering",
		"--renumber-seasons",
	}
	got := jobArgs(job, cfg)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("jobArgs =\n  %v\nwant\n  %v", got, want)
	}
}

// TestChildEnv checks that a job's environment carries the settings and not the
// secrets it shouldn't: what's configured is what the child sees.
func TestChildEnv(t *testing.T) {
	cfg := defaultConfig()
	cfg.EtpRt = "cookie-value"
	cfg.Destination = destinationSFTP
	cfg.SFTP = sftpConfig{
		Host:       "192.168.8.2",
		Port:       "22",
		User:       "root",
		Password:   "hunter2",
		RemoteDir:  "/boveda/multimedia/series",
		KnownHosts: "/root/.ssh/known_hosts",
	}

	env := cfg.childEnv()
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"CRDL_PROGRESS=plain",
		"CRDL_ETP_RT=cookie-value",
		"CRDL_SFTP_HOST=192.168.8.2",
		"CRDL_SFTP_PORT=22",
		"CRDL_SFTP_USER=root",
		"CRDL_SFTP_PASSWORD=hunter2",
		"CRDL_SFTP_REMOTE_DIR=/boveda/multimedia/series",
		"CRDL_SFTP_KNOWN_HOSTS=/root/.ssh/known_hosts",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("childEnv is missing %q", want)
		}
	}

	// A local destination must not leak the server settings into the child.
	cfg.Destination = destinationLocal
	if joined := strings.Join(cfg.childEnv(), "\n"); strings.Contains(joined, "CRDL_SFTP_HOST") {
		t.Error("a local job should not get the SFTP variables")
	}
}

// TestConfigRoundTripAndSecrets checks that the settings survive a save/load and
// that the page never receives the secrets.
func TestConfigRoundTripAndSecrets(t *testing.T) {
	dataDir := t.TempDir()
	cfg := defaultConfig()
	cfg.EtpRt = "cookie-value"
	cfg.SFTP.Password = "hunter2"
	cfg.SFTP.Host = "192.168.8.2"
	cfg.Destination = destinationSFTP
	cfg.VideoQuality = "720p"

	if err := cfg.save(dataDir); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(configPath(dataDir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.json mode = %o, want 600", perm)
	}

	loaded, err := loadWebConfig(dataDir)
	if err != nil {
		t.Fatalf("loadWebConfig: %v", err)
	}
	if loaded.EtpRt != "cookie-value" || loaded.SFTP.Password != "hunter2" {
		t.Errorf("secrets did not round-trip: %+v", loaded)
	}
	if loaded.VideoQuality != "720p" || loaded.Destination != destinationSFTP {
		t.Errorf("settings did not round-trip: %+v", loaded)
	}

	body, err := json.Marshal(loaded.public())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "cookie-value") || strings.Contains(string(body), "hunter2") {
		t.Errorf("the public settings leak a secret: %s", body)
	}
	if !strings.Contains(string(body), `"hasEtpRt":true`) || !strings.Contains(string(body), `"hasSecret":true`) {
		t.Errorf("the public settings should say the secrets are set: %s", body)
	}
}

// TestMergeUpdateKeepsWhatIsNotSent checks that a settings PUT with one field
// changes that field and nothing else: the page sends a partial body and never
// sends the secrets back.
func TestMergeUpdateKeepsWhatIsNotSent(t *testing.T) {
	cfg := defaultConfig()
	cfg.EtpRt = "cookie-value"
	cfg.SFTP.Password = "hunter2"
	cfg.SFTP.Host = "192.168.8.2"
	cfg.SubsLang = "es-419,en-US"

	next, err := cfg.mergeUpdate([]byte(`{"downloadDelay":"2m","sftp":{"user":"crdl"}}`))
	if err != nil {
		t.Fatalf("mergeUpdate: %v", err)
	}
	if next.DownloadDelay != "2m" {
		t.Errorf("downloadDelay = %q, want 2m", next.DownloadDelay)
	}
	if next.SFTP.User != "crdl" {
		t.Errorf("sftp.user = %q, want crdl", next.SFTP.User)
	}
	if next.SFTP.Host != "192.168.8.2" || next.SFTP.Password != "hunter2" {
		t.Errorf("the SFTP settings were clobbered: %+v", next.SFTP)
	}
	if next.EtpRt != "cookie-value" {
		t.Errorf("the cookie was clobbered: %q", next.EtpRt)
	}
	if next.SubsLang != "es-419,en-US" {
		t.Errorf("subsLang was clobbered: %q", next.SubsLang)
	}

	// A new secret in the body replaces the stored one.
	next, err = cfg.mergeUpdate([]byte(`{"etpRt":"new-cookie","sftpPassword":"new-password"}`))
	if err != nil {
		t.Fatalf("mergeUpdate: %v", err)
	}
	if next.EtpRt != "new-cookie" || next.SFTP.Password != "new-password" {
		t.Errorf("the new secrets were not applied: %+v", next)
	}
}

// TestInterruptedOnRestart checks that a job left running when the server stopped
// comes back as interrupted, which is what makes the page offer a retry.
func TestInterruptedOnRestart(t *testing.T) {
	dataDir := t.TempDir()
	store, err := newJobStore(dataDir, defaultConfig())
	if err != nil {
		t.Fatalf("newJobStore: %v", err)
	}
	job, err := store.enqueue("https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop", "", jobOptions{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	store.update(job.ID, func(j *Job) { j.Status = statusRunning })
	// The worker saves after starting a job, not after every state change.
	if err := store.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	reloaded, err := newJobStore(dataDir, defaultConfig())
	if err != nil {
		t.Fatalf("newJobStore: %v", err)
	}
	got, ok := reloaded.jobByID(job.ID)
	if !ok {
		t.Fatal("the job was not persisted")
	}
	if got.Status != statusInterrupted {
		t.Errorf("status = %q, want %q", got.Status, statusInterrupted)
	}
}

// TestHandlers covers the API's happy path and the errors the page has to show:
// a bad URL, a bad delay and an unknown job.
func TestHandlers(t *testing.T) {
	store := testStore(t, stubBinary(t, 0, "Downloading season 1 of X (1 episodes)"))
	web := &webServer{dataDir: store.dataDir, store: store, cfg: store.cfg}
	server := httptest.NewServer(web.routes())
	defer server.Close()

	get := func(path string) (int, string) {
		res, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer res.Body.Close()
		var body strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			body.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return res.StatusCode, body.String()
	}
	post := func(path, body string) (int, string) {
		res, err := server.Client().Post(server.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		defer res.Body.Close()
		var out strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return res.StatusCode, out.String()
	}

	if code, body := get("/api/state"); code != http.StatusOK || !strings.Contains(body, `"series"`) {
		t.Errorf("GET /api/state = %d %s", code, body)
	}
	if code, _ := get("/"); code != http.StatusOK {
		t.Errorf("GET / = %d", code)
	}
	if code, body := post("/api/jobs", `{"url":"https://example.com/not-crunchyroll"}`); code != http.StatusBadRequest || !strings.Contains(body, "Crunchyroll") {
		t.Errorf("POST /api/jobs with a bad URL = %d %s", code, body)
	}
	if code, body := post("/api/jobs", `{"url":"https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop","season":7}`); code != http.StatusCreated {
		t.Errorf("POST /api/jobs = %d %s", code, body)
	}
	if code, _ := get("/api/jobs/nope/log"); code != http.StatusNotFound {
		t.Errorf("GET an unknown log = %d", code)
	}
	if code, body := post("/api/pause", `{"paused":true}`); code != http.StatusOK || !strings.Contains(body, "true") {
		t.Errorf("POST /api/pause = %d %s", code, body)
	}
	if code, _ := get("/api/jobs"); code != http.StatusOK {
		t.Errorf("GET /api/jobs = %d", code)
	}
}

// TestUnknownMethodIsRejected checks that the mux refuses a method it doesn't
// serve instead of silently accepting it.
func TestUnknownMethodIsRejected(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))
	web := &webServer{dataDir: store.dataDir, store: store, cfg: store.cfg}
	server := httptest.NewServer(web.routes())
	defer server.Close()

	res, err := server.Client().Post(server.URL+"/api/config", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /api/config: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/config = %d, want 405", res.StatusCode)
	}
}

// TestLogEndpointOffsets checks that the log endpoint returns only what the
// caller hasn't read, which is what makes polling cheap.
func TestLogEndpointOffsets(t *testing.T) {
	store := testStore(t, stubBinary(t, 0,
		"Downloading season 1 of X (1 episodes)",
		"Downloading: Pilot (S01E01) from X",
	))
	store.enqueue("https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop", "", jobOptions{}, "")
	job := runQueuedJob(t, store)
	if job.LogPath == "" {
		t.Fatal("the job has no log file")
	}

	web := &webServer{dataDir: store.dataDir, store: store, cfg: store.cfg}
	server := httptest.NewServer(web.routes())
	defer server.Close()

	type logResponse struct {
		Text   string `json:"text"`
		Offset int64  `json:"offset"`
		Done   bool   `json:"done"`
	}

	var first logResponse
	res, err := server.Client().Get(server.URL + "/api/jobs/" + job.ID + "/log?offset=0")
	if err != nil {
		t.Fatalf("GET log: %v", err)
	}
	if err := json.NewDecoder(res.Body).Decode(&first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	res.Body.Close()
	if !strings.Contains(first.Text, "Downloading: Pilot") {
		t.Errorf("log text = %q", first.Text)
	}
	if !first.Done {
		t.Error("a finished job should report done")
	}

	res, err = server.Client().Get(server.URL + "/api/jobs/" + job.ID + "/log?offset=" + itoa64(first.Offset))
	if err != nil {
		t.Fatalf("GET log: %v", err)
	}
	var second logResponse
	if err := json.NewDecoder(res.Body).Decode(&second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	res.Body.Close()
	if second.Text != "" {
		t.Errorf("the second read should be empty, got %q", second.Text)
	}
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestStaleLogIsPruned checks that the job history doesn't grow forever.
func TestStaleLogIsPruned(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))
	store.maxJobs = 2

	for i := 0; i < 4; i++ {
		job, err := store.enqueue("https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop", "", jobOptions{}, "")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		store.update(job.ID, func(j *Job) {
			j.Status = statusDone
			j.FinishedAt = time.Now()
		})
		store.mu.Lock()
		store.pruneLocked()
		store.mu.Unlock()
	}

	_, jobs, _ := store.snapshot()
	if len(jobs) > 2 {
		t.Errorf("history = %d jobs, want at most 2", len(jobs))
	}
}

// TestSeriesListOneEntryPerSeries checks that the page gets one row per series
// with its latest run, no matter how many times it was downloaded.
func TestSeriesListOneEntryPerSeries(t *testing.T) {
	store := testStore(t, stubBinary(t, 0, "Downloading season 1 of X (1 episodes)"))

	entry, err := store.addToWishlist("https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop", "Cowboy Bebop", 0, 0)
	if err != nil {
		t.Fatalf("addToWishlist: %v", err)
	}

	// Three runs of the same series, plus one ad-hoc download of another.
	var latest string
	for i := 0; i < 3; i++ {
		job, err := store.enqueue(entry.URL, entry.ID, jobOptions{}, "Cowboy Bebop")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		latest = job.ID
		store.update(job.ID, func(j *Job) { j.Status = statusDone })
	}
	if _, err := store.enqueue("https://www.crunchyroll.com/series/GYEXQKJG6/dr-stone", "", jobOptions{}, "Dr. STONE"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// An ad-hoc run of the series already in the list is the same series, not a
	// second one (the trailing slash a copy-paste adds doesn't make it different).
	adhoc, err := store.enqueue(entry.URL+"/", "", jobOptions{}, "Cowboy Bebop")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	latest = adhoc.ID

	series := store.seriesSnapshot()
	if len(series) != 2 {
		t.Fatalf("series = %d entries, want 2 (one per series): %+v", len(series), series)
	}

	var cowboy *seriesEntry
	for i := range series {
		if series[i].ID == entry.ID {
			cowboy = &series[i]
		}
	}
	if cowboy == nil {
		t.Fatal("the wishlist series is missing")
	}
	if !cowboy.InWishlist {
		t.Error("the wishlist series should be marked as such")
	}
	if cowboy.Runs != 4 {
		t.Errorf("runs = %d, want 4", cowboy.Runs)
	}
	if cowboy.Job == nil || cowboy.Job.ID != latest {
		t.Errorf("the entry should carry the latest run, got %+v", cowboy.Job)
	}
}

// TestProgressSurvivesARestart checks that re-queueing a series (retrying a
// stuck run, or searching for new episodes) does not blank its progress bar:
// a fresh run starts at 0/0, so the list has to keep showing how far earlier
// runs got until the new one passes them.
func TestProgressSurvivesARestart(t *testing.T) {
	store := testStore(t, stubBinary(t, 0,
		"Downloading season 1 of Cowboy Bebop (24 episodes)",
		"",
		"Episode 1 is already downloaded as \"x\", skipping...",
		"Download finished! Output file: /tmp/x.mkv",
	))
	url := "https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop"

	first, err := store.enqueue(url, "", jobOptions{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	runQueuedJob(t, store) // 2 of 24 done, then the run ends
	store.update(first.ID, func(j *Job) { j.Status = statusInterrupted })

	// Searching for new episodes queues a fresh run that has not announced
	// anything yet.
	if _, err := store.enqueue(url, "", jobOptions{}, ""); err != nil {
		t.Fatalf("enqueue again: %v", err)
	}
	series := store.seriesSnapshot()
	if len(series) != 1 || series[0].Job == nil {
		t.Fatalf("series = %+v", series)
	}
	if series[0].Job.Done != 2 || series[0].Job.Total != 24 {
		t.Errorf("progress after re-queue = %d/%d, want 2/24", series[0].Job.Done, series[0].Job.Total)
	}
	// The stored runs keep their own numbers; only the list adds them up.
	if got, _ := store.jobByID(first.ID); got.Done != 2 || got.Total != 24 {
		t.Errorf("stored first run = %d/%d, want its own 2/24", got.Done, got.Total)
	}
}

// TestQueueIsIdempotent checks that asking for a series twice doesn't queue two
// runs of it: "search new" covers every series at once and the button is easy to
// press again.
func TestQueueIsIdempotent(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))

	entry, err := store.addToWishlist("https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop", "", 0, 0)
	if err != nil {
		t.Fatalf("addToWishlist: %v", err)
	}

	first, _ := store.enqueue(entry.URL, entry.ID, jobOptions{}, "")
	// A trailing slash is what a copy-paste adds; it's the same series.
	second, _ := store.enqueue(entry.URL+"/", entry.ID, jobOptions{}, "")
	if first.ID != second.ID {
		t.Errorf("a second run was queued for the same series: %s and %s", first.ID, second.ID)
	}

	// Once the run is finished, asking again is a new run.
	store.update(first.ID, func(j *Job) { j.Status = statusDone })
	third, _ := store.enqueue(entry.URL, entry.ID, jobOptions{}, "")
	if third.ID == first.ID {
		t.Error("a finished run should not be reused")
	}

	if _, jobs, _ := store.snapshot(); len(jobs) != 2 {
		t.Errorf("jobs = %d, want 2", len(jobs))
	}
}

// TestLearnedTitleReachesTheWishlist checks that a series added by URL alone is
// named after the first run, which is what the page shows.
func TestLearnedTitleReachesTheWishlist(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))

	entry, err := store.addToWishlist("https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop", "", 0, 0)
	if err != nil {
		t.Fatalf("addToWishlist: %v", err)
	}
	job, err := store.enqueue(entry.URL, entry.ID, jobOptions{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	store.applyLine(job.ID, "Downloading season 1 of Cowboy Bebop (26 episodes)")

	wishlist, _, _ := store.snapshot()
	if wishlist[0].Title != "Cowboy Bebop" {
		t.Errorf("wishlist title = %q, want Cowboy Bebop", wishlist[0].Title)
	}

	series := store.seriesSnapshot()
	if len(series) != 1 || series[0].Title != "Cowboy Bebop" {
		t.Errorf("series = %+v, want one entry named Cowboy Bebop", series)
	}
}

// TestDuplicateQueuedJobsAreDroppedOnLoad checks the cleanup of the duplicates an
// older version left behind: one queued job per series, the rest discarded.
func TestDuplicateQueuedJobsAreDroppedOnLoad(t *testing.T) {
	dir := t.TempDir()
	store := testStoreIn(t, dir, stubBinary(t, 0))

	url := "https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop"
	done, err := store.enqueue(url+"/season-2", "", jobOptions{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	store.update(done.ID, func(j *Job) { j.Status = statusDone })

	// The state the old handler produced: one queued job per press.
	for i := 0; i < 3; i++ {
		store.mu.Lock()
		store.state.Jobs = append([]*Job{{
			ID: newID(), URL: url, Status: statusQueued, CreatedAt: time.Now(),
		}}, store.state.Jobs...)
		if err := store.persistLocked(); err != nil {
			t.Fatalf("persist: %v", err)
		}
		store.mu.Unlock()
	}

	reloaded, err := newJobStore(dir, testConfig())
	if err != nil {
		t.Fatalf("newJobStore: %v", err)
	}
	_, jobs, _ := reloaded.snapshot()
	queued := 0
	for _, job := range jobs {
		if job.Status == statusQueued {
			queued++
		}
	}
	if queued != 1 {
		t.Errorf("queued jobs = %d, want 1", queued)
	}
	if len(jobs) != 2 {
		t.Errorf("jobs = %d, want 2 (one queued, one finished)", len(jobs))
	}
}

func TestTitleFromURL(t *testing.T) {
	cases := map[string]string{
		"https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop":                                     "Cowboy Bebop",
		"https://www.crunchyroll.com/series/GYE5K3GQR/konosuba---gods-blessing-on-this-wonderful-world": "Konosuba Gods Blessing On This Wonderful World",
		"https://www.crunchyroll.com/series/GYEXQKJG6/dr-stone/":                                        "Dr Stone",
	}
	for url, want := range cases {
		if got := titleFromURL(url); got != want {
			t.Errorf("titleFromURL(%q) = %q, want %q", url, got, want)
		}
	}
}

// TestPriorityOrder is the whole point of the feature: the queue starts the
// series with the highest priority, whatever order they were added in.
func TestPriorityOrder(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))

	low, err := store.enqueue("https://www.crunchyroll.com/series/A/low", "", jobOptions{Priority: 0}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := store.enqueue("https://www.crunchyroll.com/series/B/medium", "", jobOptions{Priority: 2}, ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	high, err := store.enqueue("https://www.crunchyroll.com/series/C/high", "", jobOptions{Priority: 3}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if next := store.nextQueuedLocked(); next == nil || next.ID != high.ID {
		t.Fatalf("next job = %v, want the priority 3 one", next)
	}
	// Dropping the front of the queue must not resurrect the oldest job: the
	// priority 2 series goes next, and the oldest is last.
	store.findLocked(high.ID).Status = statusRunning
	if next := store.nextQueuedLocked(); next == nil || next.Options.Priority != 2 {
		t.Errorf("second job = %v, want the priority 2 one", next)
	}
	store.findLocked(store.nextQueuedLocked().ID).Status = statusRunning
	if next := store.nextQueuedLocked(); next == nil || next.ID != low.ID {
		t.Errorf("third job = %v, want the oldest one", next)
	}
}

// TestEqualPriorityKeepsTheQueueOrder locks in the promise in nextQueuedLocked's
// comment: with no priority set, the queue is still first in, first out.
func TestEqualPriorityKeepsTheQueueOrder(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))

	first, err := store.enqueue("https://www.crunchyroll.com/series/A/one", "", jobOptions{}, "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := store.enqueue("https://www.crunchyroll.com/series/B/two", "", jobOptions{}, ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if next := store.nextQueuedLocked(); next == nil || next.ID != first.ID {
		t.Errorf("next job = %v, want the first one queued", next)
	}
}

// TestSetPriorityReordersTheQueue checks that promoting a series reorders the
// job it already has waiting, so a priority read off the list is a priority the
// queue obeys.
func TestSetPriorityReordersTheQueue(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))

	url := "https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop"
	entry, err := store.addToWishlist(url, "Cowboy Bebop", 0, 0)
	if err != nil {
		t.Fatalf("addToWishlist: %v", err)
	}
	job, err := store.enqueue(entry.URL, entry.ID, jobOptions{}, entry.Title)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := store.enqueue("https://www.crunchyroll.com/series/B/two", "", jobOptions{Priority: 2}, ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if err := store.setPriority(entry.ID, 3); err != nil {
		t.Fatalf("setPriority: %v", err)
	}
	store.mu.Lock()
	next := store.nextQueuedLocked()
	store.mu.Unlock()
	if next == nil || next.ID != job.ID {
		t.Fatalf("next job = %v, want the promoted one", next)
	}

	// Out-of-range and unknown entries are refused rather than silently clamped.
	if err := store.setPriority(entry.ID, 9); !errors.Is(err, ErrPriorityOutOfRange) {
		t.Errorf("setPriority(9) = %v, want ErrPriorityOutOfRange", err)
	}
	if err := store.setPriority("nope", 1); !errors.Is(err, errNotFound) {
		t.Errorf("setPriority(unknown) = %v, want errNotFound", err)
	}
}

// TestReaddingASeriesKeepsItsPriority: the form sends priority 0 unless one was
// picked, and re-adding a series must not throw away the order already set.
func TestReaddingASeriesKeepsItsPriority(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))
	url := "https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop"

	if _, err := store.addToWishlist(url, "", 0, 3); err != nil {
		t.Fatalf("addToWishlist: %v", err)
	}
	entry, err := store.addToWishlist(url, "", 1, 0)
	if err != nil {
		t.Fatalf("addToWishlist: %v", err)
	}
	if entry.Priority != 3 {
		t.Errorf("priority = %d after re-adding, want 3", entry.Priority)
	}
	if entry.Season != 1 {
		t.Errorf("season = %d, want the new one (1)", entry.Season)
	}
}

// TestPrioritySurvivesARestart: the priority lives in the persisted state, so a
// restart while a series waits doesn't lose its place in line.
func TestPrioritySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	store := testStoreIn(t, dir, stubBinary(t, 0))
	url := "https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop"
	entry, err := store.addToWishlist(url, "", 0, 2)
	if err != nil {
		t.Fatalf("addToWishlist: %v", err)
	}
	if _, err := store.enqueue(entry.URL, entry.ID, jobOptions{Priority: entry.Priority}, ""); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	reloaded, err := newJobStore(dir, testConfig())
	if err != nil {
		t.Fatalf("newJobStore: %v", err)
	}
	series := reloaded.seriesSnapshot()
	if len(series) != 1 || series[0].Priority != 2 {
		t.Fatalf("series = %+v, want one entry at priority 2", series)
	}
	reloaded.mu.Lock()
	defer reloaded.mu.Unlock()
	if next := reloaded.nextQueuedLocked(); next == nil || next.Options.Priority != 2 {
		t.Errorf("queued job lost its priority across the restart: %v", next)
	}
}

// TestPriorityEndpoint checks the HTTP face of a priority change: the form's
// shape (URL, season, priority) and the PUT that reorders a series afterwards.
func TestPriorityEndpoint(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))
	web := &webServer{dataDir: store.dataDir, store: store, cfg: store.cfg}
	server := httptest.NewServer(web.routes())
	defer server.Close()

	do := func(method, path, body string) (int, string) {
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer res.Body.Close()
		var out strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				break
			}
			_ = n
		}
		return res.StatusCode, out.String()
	}

	url := "https://www.crunchyroll.com/series/GYVNXMVP6/cowboy-bebop"
	code, body := do("POST", "/api/wishlist", `{"url":"`+url+`","priority":3}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/wishlist with a priority = %d %s", code, body)
	}
	entry, err := store.addToWishlist(url, "", 0, 0)
	if err != nil {
		t.Fatalf("addToWishlist: %v", err)
	}
	if entry.Priority != 3 {
		t.Errorf("priority = %d after adding, want 3", entry.Priority)
	}

	if code, body := do("PUT", "/api/wishlist/"+entry.ID+"/priority", `{"priority":1}`); code != http.StatusOK {
		t.Errorf("PUT priority = %d %s", code, body)
	}
	if code, body := do("PUT", "/api/wishlist/"+entry.ID+"/priority", `{"priority":7}`); code != http.StatusBadRequest {
		t.Errorf("PUT an out-of-range priority = %d %s, want 400", code, body)
	}
	if code, _ := do("PUT", "/api/wishlist/nope/priority", `{"priority":1}`); code != http.StatusNotFound {
		t.Errorf("PUT a priority on an unknown series = %d, want 404", code)
	}
	if code, _ := do("POST", "/api/series", `{"url":"`+url+`","priority":9}`); code != http.StatusBadRequest {
		t.Errorf("POST /api/series with a bad priority = %d, want 400", code)
	}
}

// TestSeriesListOrder is the order the operator asked for: what hasn't downloaded
// yet on top, then what is downloading, then what already ran — each group by
// priority, so the top of the list is what happens next.
func TestSeriesListOrder(t *testing.T) {
	store := testStore(t, stubBinary(t, 0))

	add := func(slug string, priority int) wishlistEntry {
		t.Helper()
		entry, err := store.addToWishlist("https://www.crunchyroll.com/series/"+slug, "", 0, priority)
		if err != nil {
			t.Fatalf("addToWishlist(%s): %v", slug, err)
		}
		return entry
	}
	queue := func(entry wishlistEntry, status JobStatus) *Job {
		t.Helper()
		job, err := store.enqueue(entry.URL, entry.ID, jobOptions{Priority: entry.Priority}, entry.Title)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		store.update(job.ID, func(j *Job) { j.Status = status })
		return job
	}

	// One of each group, deliberately added out of order.
	doneLow := add("A/done-low", 0)
	queue(doneLow, statusDone)
	runningLow := add("B/running-low", 0)
	queue(runningLow, statusRunning)
	waitingHigh := add("C/waiting-high", 3)
	queue(waitingHigh, statusQueued)
	add("D/never", 1) // in the list, no run yet
	doneHigh := add("E/done-high", 3)
	queue(doneHigh, statusDone)
	runningHigh := add("F/running-high", 2)
	queue(runningHigh, statusRunning)
	waitingLow := add("G/waiting-low", 0)
	queue(waitingLow, statusQueued)
	failed := add("H/failed", 2)
	queue(failed, statusFailed)

	got := []string{}
	for _, entry := range store.seriesSnapshot() {
		got = append(got, entry.Title)
	}
	// Title is learned from the slug until a run names the series.
	want := []string{
		"Waiting High", "Never", "Waiting Low", // not downloaded yet, by priority
		"Running High", "Running Low", // downloading, by priority
		"Done High", "Failed", "Done Low", // already ran, by priority
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v\nwant    %v", got, want)
	}
}
