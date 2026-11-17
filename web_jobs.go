package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// JobStatus is where a job is in its life. The names are what the API and the
// page use.
type JobStatus string

const (
	statusQueued      JobStatus = "queued"
	statusRunning     JobStatus = "running"
	statusDone        JobStatus = "done"
	statusFailed      JobStatus = "failed"
	statusCancelled   JobStatus = "cancelled"
	statusInterrupted JobStatus = "interrupted"
)

// terminal reports whether a job is finished for good, so the worker skips it
// and the API treats it as read-only history. Interrupted is not terminal: the
// page offers to retry it.
func (s JobStatus) terminal() bool {
	switch s {
	case statusDone, statusFailed, statusCancelled:
		return true
	}
	return false
}

// jobOptions are the per-job download settings, snapshotted from the config when
// the job is created so changing the settings later doesn't alter queued work.
type jobOptions struct {
	Season         int    `json:"season"` // 0 = every season
	VideoQuality   string `json:"videoQuality"`
	AudioQuality   string `json:"audioQuality"`
	AudioLang      string `json:"audioLang"`
	SubsLang       string `json:"subsLang"`
	CCLang         string `json:"ccLang"`
	DownloadDelay  string `json:"downloadDelay"`
	AbsoluteNum    bool   `json:"absoluteNumbering"`
	RenumberSeason bool   `json:"renumberSeasons"`

	// Priority orders the queue, taken from the wishlist when the job is made:
	// the worker always starts the waiting series with the highest number.
	Priority int `json:"priority"`
}

// Job is one download run: a URL plus the options it was queued with, and the
// progress parsed out of its output.
type Job struct {
	ID       string     `json:"id"`
	URL      string     `json:"url"`
	SeriesID string     `json:"seriesId"` // wishlist entry it came from, "" if ad-hoc
	Title    string     `json:"title"`    // series title, learned while running
	Options  jobOptions `json:"options"`

	Status JobStatus `json:"status"`

	// Progress, all of it derived from the child's output (see web_parse.go).
	Total     int                `json:"total"`     // episodes announced so far
	Done      int                `json:"done"`      // episodes finished
	Current   string             `json:"current"`   // "S01E05 - Title", empty when between episodes
	Tracks    map[string]float64 `json:"tracks"`    // "video" -> 62
	Uploading bool               `json:"uploading"` // file is on its way to the server
	Waiting   string             `json:"waiting"`   // human text for a pause, e.g. a retry or the inter-episode gap
	Failures  []string           `json:"failures"`  // episodes the run reported as undownloadable

	LogPath    string    `json:"logPath"`
	Err        string    `json:"error"`
	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`

	// offset is how much of the log the page has already read, kept here so a
	// reloading browser resumes where it left off instead of re-reading it all.
	offset int64

	// inFailures is set while the parser is inside the trailing list of
	// episodes a run reported as undownloadable.
	inFailures bool
}

// wishlistEntry is a series the operator wants downloaded. Adding one queues a
// job; the entry itself stays so "search new" can re-queue it later, and the
// SxxExx skip means only what's missing gets fetched.
type wishlistEntry struct {
	ID       string    `json:"id"`
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	Season   int       `json:"season"` // 0 = every season
	Priority int       `json:"priority"`
	AddedAt  time.Time `json:"addedAt"`
	LastJob  string    `json:"lastJob"` // most recent job ID, for "what happened last time"
}

// storeState is the whole persisted state. Jobs and the wishlist share one file
// so a single atomic write keeps them consistent.
type storeState struct {
	Wishlist []wishlistEntry `json:"wishlist"`
	Jobs     []*Job          `json:"jobs"`
	Paused   bool            `json:"paused"`
}

// jobStore owns the state and the single worker that runs one job at a time.
// Crunchyroll's rate limit is per client, so parallel downloads would only race
// each other into it.
type jobStore struct {
	mu      sync.Mutex
	dataDir string
	cfg     webConfig
	state   storeState

	running  *jobProcess
	notify   chan struct{}
	maxJobs  int // history cap, keeps the JSON file from growing without bound
	runningT *Job
}

// jobProcess is a job whose child process is alive.
type jobProcess struct {
	job  *Job
	pid  int
	kill func() error
	done chan struct{}
}

// newJobStore loads the persisted state (marking anything that was running as
// interrupted, since it can't have survived the restart) and returns a store
// ready to start work.
func newJobStore(dataDir string, cfg webConfig) (*jobStore, error) {
	s := &jobStore{
		dataDir: dataDir,
		cfg:     cfg,
		notify:  make(chan struct{}, 1),
		maxJobs: 200,
	}
	s.state = storeState{}
	body, err := os.ReadFile(s.statePath())
	if err == nil {
		if err := json.Unmarshal(body, &s.state); err != nil {
			return nil, fmt.Errorf("%s: %w", s.statePath(), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	for _, job := range s.state.Jobs {
		if job.Status == statusRunning {
			// The process died with the previous server; nothing is running now.
			job.Status = statusInterrupted
			job.Waiting = ""
			job.FinishedAt = time.Now()
			if job.Err == "" {
				job.Err = "the server stopped while this job was running"
			}
		}
		if job.Tracks == nil {
			job.Tracks = map[string]float64{}
		}
	}
	s.dropDuplicateQueued()
	s.pruneLocked()
	return s, nil
}

// dropDuplicateQueued removes queued jobs for a series that is already queued or
// running. Older versions queued one job per press of "search new", and a queued
// duplicate would otherwise re-scan a whole series for nothing.
func (s *jobStore) dropDuplicateQueued() {
	active := map[string]bool{}
	kept := s.state.Jobs[:0]
	for _, job := range s.state.Jobs {
		key := seriesKey(job.URL)
		switch job.Status {
		case statusRunning, statusQueued:
			if active[key] {
				continue
			}
			active[key] = true
		}
		kept = append(kept, job)
	}
	s.state.Jobs = kept
}

// setConfig replaces the settings used for the next job. Jobs already queued
// keep the options they were created with.
func (s *jobStore) setConfig(cfg webConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

func (s *jobStore) statePath() string {
	return filepath.Join(s.dataDir, "jobs.json")
}

func (s *jobStore) logsDir() string {
	return filepath.Join(s.dataDir, "logs")
}

// persistLocked writes the state atomically. Must be called with s.mu held.
func (s *jobStore) persistLocked() error {
	if err := os.MkdirAll(s.dataDir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.statePath(), append(body, '\n'), 0o600)
}

// pruneLocked drops the oldest finished jobs past the history cap. Must be
// called with s.mu held.
func (s *jobStore) pruneLocked() {
	if len(s.state.Jobs) <= s.maxJobs {
		return
	}
	kept := make([]*Job, 0, s.maxJobs)
	removed := len(s.state.Jobs) - s.maxJobs
	for _, job := range s.state.Jobs {
		if removed > 0 && job.Status.terminal() {
			removed--
			continue
		}
		kept = append(kept, job)
	}
	s.state.Jobs = kept
}

// enqueue adds a job for url and wakes the worker.
func (s *jobStore) enqueue(url, seriesID string, opts jobOptions, title string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Asking for a series that is already waiting or downloading is the same
	// request again (the button is easy to press twice, and "search new" covers
	// every series at once), so it returns the run that is already there instead
	// of queuing a second one that would re-skip every episode.
	if active := s.activeJobLocked(url); active != nil {
		// A queued run can still be reordered; one that is already downloading
		// can't, so its priority is what it started with.
		if active.Status == statusQueued && active.Options.Priority != opts.Priority {
			active.Options.Priority = opts.Priority
			if err := s.persistLocked(); err != nil {
				return nil, err
			}
		}
		return active, nil
	}

	job := &Job{
		ID:        newID(),
		URL:       url,
		SeriesID:  seriesID,
		Title:     title,
		Options:   opts,
		Status:    statusQueued,
		Tracks:    map[string]float64{},
		CreatedAt: time.Now(),
	}
	s.state.Jobs = append(s.state.Jobs, job)
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	s.wake()
	return job, nil
}

// activeJobLocked returns the queued or running job for a series, if there is
// one. Callers hold the lock.
func (s *jobStore) activeJobLocked(url string) *Job {
	key := seriesKey(url)
	for _, job := range s.state.Jobs {
		if seriesKey(job.URL) != key {
			continue
		}
		if job.Status == statusQueued || job.Status == statusRunning {
			return job
		}
	}
	return nil
}

// addToWishlist stores a series (replacing an existing entry for the same URL,
// so adding twice updates the season instead of duplicating it). A priority of
// 0 leaves whatever the entry already had, so re-adding a series doesn't quietly
// reset the order the operator set.
func (s *jobStore) addToWishlist(url, title string, season, priority int) (wishlistEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, entry := range s.state.Wishlist {
		if entry.URL == url {
			s.state.Wishlist[i].Season = season
			if priority > 0 {
				s.state.Wishlist[i].Priority = priority
			}
			if title != "" {
				s.state.Wishlist[i].Title = title
			}
			if err := s.persistLocked(); err != nil {
				return wishlistEntry{}, err
			}
			return s.state.Wishlist[i], nil
		}
	}

	entry := wishlistEntry{
		ID: newID(), URL: url, Title: title, Season: season,
		Priority: priority, AddedAt: time.Now(),
	}
	s.state.Wishlist = append(s.state.Wishlist, entry)
	if err := s.persistLocked(); err != nil {
		return wishlistEntry{}, err
	}
	return entry, nil
}

// setPriority changes the priority of a series and of the runs of it that are
// still waiting, so ordering a series also reorders the queue it sits in.
// A run already downloading is left alone: cancelling it to start it again is
// the only way to change what it does, and that's the operator's call.
func (s *jobStore) setPriority(seriesID string, priority int) error {
	if !validPriority(priority) {
		return ErrPriorityOutOfRange
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	index := -1
	for i, entry := range s.state.Wishlist {
		if entry.ID == seriesID {
			index = i
			break
		}
	}
	if index < 0 {
		return errNotFound
	}

	url := seriesKey(s.state.Wishlist[index].URL)
	s.state.Wishlist[index].Priority = priority
	for _, job := range s.state.Jobs {
		if job.Status == statusQueued && seriesKey(job.URL) == url {
			job.Options.Priority = priority
		}
	}
	if err := s.persistLocked(); err != nil {
		return err
	}
	s.wake()
	return nil
}

// learnTitle stores the series title a run reported on the job's wishlist
// entry, so the page can name a series that was added by its URL alone.
func (s *jobStore) learnTitle(jobID, title string) {
	if title == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var seriesID string
	if job := s.findLocked(jobID); job != nil {
		seriesID = job.SeriesID
	}
	if seriesID == "" {
		return
	}
	for i, entry := range s.state.Wishlist {
		if entry.ID != seriesID || entry.Title == title {
			continue
		}
		s.state.Wishlist[i].Title = title
		_ = s.persistLocked()
		return
	}
}

// hasActiveJob reports whether the series is already queued or running.
func (s *jobStore) hasActiveJob(url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeJobLocked(url) != nil
}

// removeFromWishlist deletes an entry but keeps its job history.
func (s *jobStore) removeFromWishlist(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, entry := range s.state.Wishlist {
		if entry.ID == id {
			s.state.Wishlist = append(s.state.Wishlist[:i], s.state.Wishlist[i+1:]...)
			return s.persistLocked()
		}
	}
	return errNotFound
}

// wishlistEntry returns one entry by ID.
func (s *jobStore) wishlistEntryByID(id string) (wishlistEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.state.Wishlist {
		if entry.ID == id {
			return entry, true
		}
	}
	return wishlistEntry{}, false
}

// seriesEntry is one row of the page: a series the operator wants, with the
// most recent run for it. The page shows one entry per series rather than one
// per run, because a series is downloaded many times over its life (new
// episodes, retries) and only its latest run is interesting.
type seriesEntry struct {
	ID         string    `json:"id"` // wishlist entry ID, "" for an ad-hoc download
	URL        string    `json:"url"`
	Title      string    `json:"title"`
	Season     int       `json:"season"` // 0 = every season
	Priority   int       `json:"priority"`
	AddedAt    time.Time `json:"addedAt"`
	InWishlist bool      `json:"inWishlist"`
	Runs       int       `json:"runs"` // how many runs this series has had
	Job        *Job      `json:"job"`  // most recent run, nil if never run
}

// seriesSnapshot groups the jobs by series (by URL: a wishlist entry and an
// ad-hoc download of the same series are the same series) and keeps the most
// recent run, so the page can draw a single entry per series.
func (s *jobStore) seriesSnapshot() []seriesEntry {
	wishlist, jobs, _ := s.snapshot()

	entries := make([]seriesEntry, 0, len(wishlist))
	byKey := map[string]int{} // series key -> index in entries

	for _, entry := range wishlist {
		key := seriesKey(entry.URL)
		byKey[key] = len(entries)
		entries = append(entries, seriesEntry{
			ID: entry.ID, URL: entry.URL, Title: entry.Title, Season: entry.Season,
			Priority: entry.Priority, AddedAt: entry.AddedAt, InWishlist: true,
		})
	}

	// jobs arrive newest first, so the first one seen for a series is its latest.
	for i := range jobs {
		job := jobs[i]
		key := seriesKey(job.URL)
		index, ok := byKey[key]
		if !ok {
			index = len(entries)
			byKey[key] = index
			entries = append(entries, seriesEntry{
				URL: job.URL, Title: job.Title, Season: job.Options.Season,
				Priority: job.Options.Priority, AddedAt: job.CreatedAt,
			})
		}
		entries[index].Runs++
		if job.Title != "" && entries[index].Title == "" {
			entries[index].Title = job.Title
		}
		if entries[index].Job == nil {
			entries[index].Job = &job
		}
	}

	for i := range entries {
		if entries[i].Title == "" {
			entries[i].Title = titleFromURL(entries[i].URL)
		}
	}

	// What is still to come first, then what is being downloaded, then what is
	// finished. Within a group the highest priority comes first, and within a
	// priority the most recently added, which is what a plain queue did before
	// priorities existed.
	sort.SliceStable(entries, func(i, j int) bool {
		bi, bj := seriesBucket(entries[i]), seriesBucket(entries[j])
		if bi != bj {
			return bi < bj
		}
		if entries[i].Priority != entries[j].Priority {
			return entries[i].Priority > entries[j].Priority
		}
		return entries[i].AddedAt.After(entries[j].AddedAt)
	})
	return entries
}

// seriesBucket groups the list the way the page shows it: 0 for a series that
// hasn't been downloaded (or is waiting in the queue), 1 for one downloading
// right now, 2 for one that already ran. A series that failed or was cancelled
// counts as done: it had its turn and whatever is left of it is a retry.
func seriesBucket(entry seriesEntry) int {
	switch {
	case entry.Job == nil, entry.Job.Status == statusQueued:
		return 0
	case entry.Job.Status == statusRunning:
		return 1
	default:
		return 2
	}
}

// titleFromURL names a series by its slug until a run reports the real title,
// so a series just added by its link isn't a blank row (and isn't a URL either).
func titleFromURL(url string) string {
	slug := seriesKey(url)
	if i := strings.LastIndex(slug, "/"); i >= 0 {
		slug = slug[i+1:]
	}
	words := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// seriesKey is what makes two runs the same series: the URL, without the slack
// a copy-paste adds.
func seriesKey(url string) string {
	return strings.TrimRight(strings.TrimSpace(url), "/")
}

// snapshot is a consistent copy of the state for the API. The Job values are
// copied so the handler can serialise them without holding the lock.
func (s *jobStore) snapshot() (wishlist []wishlistEntry, jobs []Job, paused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	wishlist = append([]wishlistEntry{}, s.state.Wishlist...)
	jobs = make([]Job, 0, len(s.state.Jobs))
	for _, job := range s.state.Jobs {
		copied := *job
		copied.Tracks = make(map[string]float64, len(job.Tracks))
		for k, v := range job.Tracks {
			copied.Tracks[k] = v
		}
		copied.Failures = append([]string(nil), job.Failures...)
		jobs = append(jobs, copied)
	}
	// Newest first: the page wants the interesting end of the list on top.
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].CreatedAt.After(jobs[j].CreatedAt) })
	return wishlist, jobs, s.state.Paused
}

// jobByID returns a copy of one job.
func (s *jobStore) jobByID(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.state.Jobs {
		if job.ID == id {
			copied := *job
			return copied, true
		}
	}
	return Job{}, false
}

// removeJob deletes a finished job from the history (and its log file).
func (s *jobStore) removeJob(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, job := range s.state.Jobs {
		if job.ID != id {
			continue
		}
		if job.Status == statusRunning {
			return errors.New("the job is running; cancel it first")
		}
		s.state.Jobs = append(s.state.Jobs[:i], s.state.Jobs[i+1:]...)
		if job.LogPath != "" {
			os.Remove(job.LogPath)
		}
		return s.persistLocked()
	}
	return errNotFound
}

// setPaused stops the worker from picking up queued jobs. A running job is left
// alone; pausing is about not starting the next one.
func (s *jobStore) setPaused(paused bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Paused = paused
	if !paused {
		s.wake()
	}
	return s.persistLocked()
}

// isPaused reports whether the worker is holding queued jobs back.
func (s *jobStore) isPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Paused
}

// wake nudges the worker without blocking.
func (s *jobStore) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// nextQueuedLocked returns the queued job with the highest priority, or nil.
// Equal priorities keep their order (oldest first), so without a priority set
// the queue behaves exactly as before. Must be called with s.mu held.
func (s *jobStore) nextQueuedLocked() *Job {
	var next *Job
	for _, job := range s.state.Jobs {
		if job.Status != statusQueued {
			continue
		}
		if next == nil || job.Options.Priority > next.Options.Priority {
			next = job
		}
	}
	return next
}

// ErrPriorityOutOfRange is what a priority outside the accepted range returns.
var ErrPriorityOutOfRange = errors.New("the priority has to be between 0 and 3")

// validPriority reports whether p is a priority the page offers.
func validPriority(p int) bool {
	return p >= 0 && p <= 3
}

// run is the worker loop: one job at a time, forever. It returns only when stop
// is closed.
func (s *jobStore) run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-s.notify:
		}
		if err := s.runNext(stop); err != nil {
			fmt.Printf("web: %v\n", err)
		}
	}
}

// runNext picks up and finishes a single job. It's separated from run so a test
// can drive exactly one job synchronously.
func (s *jobStore) runNext(stop <-chan struct{}) error {
	s.mu.Lock()
	if s.state.Paused {
		s.mu.Unlock()
		return nil
	}
	if s.runningT != nil {
		s.mu.Unlock()
		return nil
	}
	job := s.nextQueuedLocked()
	if job == nil {
		s.mu.Unlock()
		return nil
	}
	job.Status = statusRunning
	job.StartedAt = time.Now()
	job.Tracks = map[string]float64{}
	if err := os.MkdirAll(s.logsDir(), 0o700); err != nil {
		s.mu.Unlock()
		return err
	}
	job.LogPath = filepath.Join(s.logsDir(), sanitizeFilename(job.ID)+".log")
	s.runningT = job
	cfg := s.cfg
	s.mu.Unlock()

	if err := s.persist(); err != nil {
		fmt.Printf("web: could not save the job state: %v\n", err)
	}

	err := s.startProcess(job, cfg, stop)
	s.finish(job.ID, err)
	return nil
}

// finish records the outcome of a job.
func (s *jobStore) finish(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job := s.findLocked(id)
	if job == nil {
		return
	}
	s.runningT = nil
	job.Waiting = ""
	job.Uploading = false
	job.FinishedAt = time.Now()

	switch {
	case job.Status == statusCancelled:
		// Cancelled by the operator; keep that status and the partial log.
	case err != nil:
		job.Status = statusFailed
		job.Err = err.Error()
	case len(job.Failures) > 0:
		job.Status = statusFailed
		if job.Err == "" {
			job.Err = fmt.Sprintf("%d episode(s) could not be downloaded", len(job.Failures))
		}
	default:
		job.Status = statusDone
		// Errors seen along the way were transient if the run finished anyway.
		job.Err = ""
	}

	if job.SeriesID != "" {
		for i := range s.state.Wishlist {
			if s.state.Wishlist[i].ID == job.SeriesID {
				s.state.Wishlist[i].LastJob = job.ID
			}
		}
	}
	s.pruneLocked()
	if persistErr := s.persistLocked(); persistErr != nil {
		fmt.Printf("web: could not save the job state: %v\n", persistErr)
	}
	s.wake()
}

// findLocked returns a job by ID. Must be called with s.mu held.
func (s *jobStore) findLocked(id string) *Job {
	for _, job := range s.state.Jobs {
		if job.ID == id {
			return job
		}
	}
	return nil
}

// persist saves the state with the lock taken.
func (s *jobStore) persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}

// update applies fn to a job under the lock and ignores slots that vanished.
func (s *jobStore) update(id string, fn func(job *Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job := s.findLocked(id); job != nil {
		fn(job)
	}
}

// logOffset returns how much of the log the caller's cursor has consumed.
func (s *jobStore) logOffset(id string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if job := s.findLocked(id); job != nil {
		return job.offset
	}
	return 0
}

// advanceLogOffset records the new cursor for a job's log.
func (s *jobStore) advanceLogOffset(id string, offset int64) {
	s.update(id, func(job *Job) { job.offset = offset })
}

var errNotFound = errors.New("not found")

// newID returns a random identifier for jobs and wishlist entries.
func newID() string {
	return uuid.NewString()
}

// jobArgs translates a job into the CLI's flags. Everything the CLI already
// supports is passed through as a flag, so the download path is the one that was
// verified against real episodes rather than a second implementation.
func jobArgs(job *Job, cfg webConfig) []string {
	args := []string{"--url", job.URL}

	if cfg.DownloadDir != "" {
		args = append(args, "--output-dir", cfg.DownloadDir)
	}
	if job.Options.Season > 0 {
		args = append(args, "--season", fmt.Sprint(job.Options.Season))
	}
	if job.Options.VideoQuality != "" {
		args = append(args, "--video-quality", job.Options.VideoQuality)
	}
	if job.Options.AudioQuality != "" {
		args = append(args, "--audio-quality", job.Options.AudioQuality)
	}
	if job.Options.AudioLang != "" {
		args = append(args, "--audio-lang", job.Options.AudioLang)
	}
	if job.Options.SubsLang != "" {
		args = append(args, "--subs-lang", job.Options.SubsLang)
	}
	if job.Options.CCLang != "" {
		args = append(args, "--cc-lang", job.Options.CCLang)
	}
	if job.Options.DownloadDelay != "" {
		args = append(args, "--download-delay", job.Options.DownloadDelay)
	}
	if job.Options.AbsoluteNum {
		args = append(args, "--absolute-numbering")
	}
	if job.Options.RenumberSeason {
		args = append(args, "--renumber-seasons")
	}
	return args
}

// startProcess runs one download as a child process and folds its output into
// the job. Running the same binary keeps a crashing download from taking the
// server with it, and means cancel can simply kill the process group (ffmpeg
// included).
func (s *jobStore) startProcess(job *Job, cfg webConfig, stop <-chan struct{}) error {
	args := jobArgs(job, cfg)

	logFile, err := os.OpenFile(job.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("could not open the log file: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(cfg.BinaryPath, args...)
	cmd.Env = cfg.childEnv()
	// device.wvd and a bundled ffmpeg are looked up next to the program and in
	// the working directory, so the child runs where the binary lives.
	cmd.Dir = filepath.Dir(cfg.BinaryPath)
	setProcessGroup(cmd)

	// One pipe for stdout and stderr, read line by line: the parser needs whole
	// lines and the log file needs everything, in the order it happened. The pipe
	// is made here instead of with StdoutPipe because Wait closes that one, and a
	// fast child can exit before the last lines were read: the end of the season
	// is exactly where the failed episodes are listed, so losing them would report
	// a broken run as done.
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW

	if err := cmd.Start(); err != nil {
		stdout.Close()
		stdoutW.Close()
		return fmt.Errorf("could not start %s: %w", cfg.BinaryPath, err)
	}
	// Only the child holds the write end now, so the reader sees EOF when it exits.
	stdoutW.Close()

	running := &jobProcess{job: job, pid: cmd.Process.Pid}
	running.kill = func() error { return killProcessGroup(cmd.Process.Pid) }
	s.mu.Lock()
	s.running = running
	s.mu.Unlock()

	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		defer stdout.Close()
		s.consume(job.ID, stdout, logFile)
	}()

	// A server shutdown shouldn't leave an orphaned download behind.
	killOnStop := make(chan struct{})
	go func() {
		select {
		case <-stop:
			running.kill()
		case <-killOnStop:
		}
	}()

	waitErr := cmd.Wait()
	close(killOnStop)
	<-consumed

	s.mu.Lock()
	s.running = nil
	cancelled := false
	if j := s.findLocked(job.ID); j != nil {
		cancelled = j.Status == statusCancelled
	}
	s.mu.Unlock()

	if cancelled {
		return nil
	}
	if waitErr != nil {
		if msg := s.failureMessage(job.ID); msg != "" {
			return errors.New(msg)
		}
		return fmt.Errorf("the download stopped: %w", waitErr)
	}
	return nil
}

// failureMessage returns the most specific error the child printed, so the page
// shows "Couldn't log in: …" instead of "exit status 1".
func (s *jobStore) failureMessage(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.findLocked(id)
	if job == nil {
		return ""
	}
	return job.Err
}

// cancelJob asks a running job's process group to stop.
func (s *jobStore) cancelJob(id string) error {
	s.mu.Lock()
	job := s.findLocked(id)
	if job == nil {
		s.mu.Unlock()
		return errNotFound
	}
	if job.Status != statusRunning && job.Status != statusQueued {
		s.mu.Unlock()
		return errors.New("the job is already finished")
	}
	job.Status = statusCancelled
	job.Err = "cancelled"
	running := s.running
	s.mu.Unlock()

	if running != nil && running.job.ID == id && running.kill != nil {
		if err := running.kill(); err != nil {
			return fmt.Errorf("could not stop the download: %w", err)
		}
	}
	s.persist()
	return nil
}
