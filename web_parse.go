package main

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// The download program was written for a human watching a terminal, so it reports
// progress by printing lines. Rather than duplicate the download logic behind an
// API, the web interface runs the same program and reads those lines back: this
// file is the whole contract between the two, and the patterns below are the
// strings download.go, output.go and remote.go actually print.

var (
	// "Downloading season 1 of Dr. STONE (24 episodes)"
	reSeasonStarted = regexp.MustCompile(`^Downloading season (\d+) of (.+) \((\d+) episodes\)$`)
	// "Downloading: Asteroid Blues (S01E01) from Cowboy Bebop"
	reEpisodeStarted = regexp.MustCompile(`^Downloading: (.*) \(S(\d+)E(\d+)\) from (.*)$`)
	// "progress: video 62% 3.4 Mbps" and "progress: video 100% done"
	rePlainProgress = regexp.MustCompile(`^progress: (.+?) (\d+)% ?(?:([0-9.]+) Mbps|done)?$`)
	// "Episode 5 is already downloaded as "…", skipping..."
	reEpisodeSkipped = regexp.MustCompile(`^Episode \d+ is already (?:downloaded|on the server) as "`)
	// "3 episode(s) of Dr. STONE could not be downloaded:"
	reSeasonFailures = regexp.MustCompile(` episode\(s\) of .+ could not be downloaded:$`)
	// "Download of 5 failed (recovered from error: …), retrying in 15s [attempt 1/3]..."
	reEpisodeRetry = regexp.MustCompile(`^Download of \d+ failed \((.+)\), retrying in (.+) \[attempt \d+/\d+\]\.\.\.$`)
	// "Retrying this episode in 5m0s [3:15AM]..."
	reRateLimitRetry = regexp.MustCompile(`^Retrying this episode in (.+) \[.+\]\.\.\.$`)
)

// consume reads the child's output line by line, appends it to the job's log and
// folds the interesting lines into the job's progress.
func (s *jobStore) consume(id string, r io.Reader, log *os.File) {
	scanner := bufio.NewScanner(r)
	// The program prints its own JSON-ish output and long error stacks; the
	// default 64 KiB line limit is generous but a stack trace can beat it.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var inFailures bool
	var offset int64
	for scanner.Scan() {
		line := scanner.Text()

		if _, err := log.WriteString(line + "\n"); err == nil {
			offset += int64(len(line)) + 1
			s.advanceLogOffset(id, offset)
		}

		// A blank line closes the "could not be downloaded" list.
		if strings.TrimSpace(line) == "" {
			inFailures = false
			continue
		}
		if reSeasonFailures.MatchString(line) {
			inFailures = true
		}
		if inFailures && strings.HasPrefix(line, "  ") {
			failure := strings.TrimSpace(line)
			s.update(id, func(job *Job) {
				job.Failures = append(job.Failures, failure)
			})
			continue
		}
		s.applyLine(id, line)
	}
}

// applyLine updates a job from one line of output. Anything unrecognised is only
// logged, which keeps the parser tolerant of the program gaining new messages.
func (s *jobStore) applyLine(id, line string) {
	if m := reSeasonStarted.FindStringSubmatch(line); m != nil {
		count, _ := strconv.Atoi(m[3])
		s.update(id, func(job *Job) {
			if job.Title == "" {
				job.Title = m[2]
			}
			job.Total += count
			job.Waiting = ""
		})
		s.learnTitle(id, m[2])
		return
	}

	if m := reEpisodeStarted.FindStringSubmatch(line); m != nil {
		title, season, episode := m[1], m[2], m[3]
		s.update(id, func(job *Job) {
			job.Current = "S" + season + "E" + episode + " - " + title
			if m[4] != "" {
				job.Title = m[4]
			}
			job.Tracks = map[string]float64{}
			job.Waiting = ""
		})
		// The page names a series by its title in the wishlist, and the entry is
		// only there to be re-queued later: without this, a series added by its
		// URL only would show its URL forever.
		s.learnTitle(id, m[4])
		return
	}

	if m := rePlainProgress.FindStringSubmatch(line); m != nil {
		track, pct := m[1], m[2]
		value, _ := strconv.ParseFloat(pct, 64)
		s.update(id, func(job *Job) {
			if job.Tracks == nil {
				job.Tracks = map[string]float64{}
			}
			job.Tracks[track] = value
		})
		return
	}

	if reEpisodeSkipped.MatchString(line) {
		// Already on disk or on the server: nothing left to do for it.
		s.update(id, func(job *Job) { job.Done++ })
		return
	}

	if strings.HasPrefix(line, "Download finished! Output file:") {
		s.update(id, func(job *Job) {
			job.Done++
			job.Current = ""
			job.Tracks = map[string]float64{}
			job.Uploading = false
			job.Waiting = ""
		})
		return
	}

	if strings.HasPrefix(line, "Uploading to ") {
		s.update(id, func(job *Job) { job.Uploading = true })
		return
	}
	if strings.HasPrefix(line, "Uploaded ") || strings.HasPrefix(line, "Removed the local copy.") {
		s.update(id, func(job *Job) { job.Uploading = false })
		return
	}

	if reSeasonFailures.MatchString(line) {
		// Header of a list whose entries follow, indented by two spaces.
		s.update(id, func(job *Job) { job.Waiting = "some episodes could not be downloaded" })
		return
	}

	if m := reEpisodeRetry.FindStringSubmatch(line); m != nil {
		s.update(id, func(job *Job) { job.Waiting = "retrying in " + m[2] + ": " + m[1] })
		return
	}
	if m := reRateLimitRetry.FindStringSubmatch(line); m != nil {
		s.update(id, func(job *Job) { job.Waiting = "rate limited by Crunchyroll, retrying in " + m[1] })
		return
	}

	if wait, ok := strings.CutPrefix(line, "Waiting "); ok && strings.Contains(line, "before the next download") {
		s.update(id, func(job *Job) { job.Waiting = "waiting " + wait })
		return
	}

	if reason, ok := strings.CutPrefix(line, "! Upload failed, keeping the local file: "); ok {
		s.update(id, func(job *Job) {
			job.Uploading = false
			job.Waiting = "upload failed, keeping the local file: " + reason
		})
		return
	}

	if reason, ok := strings.CutPrefix(line, "Recovered from error: "); ok {
		s.update(id, func(job *Job) { job.Waiting = "recovering from: " + reason })
		return
	}

	if isFatalLine(line) {
		s.update(id, func(job *Job) { job.Err = line })
	}
}

// isFatalLine reports whether a line describes a reason the run will fail, so
// the page can show the reason instead of "exit status 1". The program prints
// these and stops (or skips a URL) without a distinguishable exit code.
func isFatalLine(line string) bool {
	for _, prefix := range []string{
		"Couldn't log in:",
		"Couldn't read that episode:",
		"Invalid URL (must be",
		"This anime has no season",
		"Invalid SFTP configuration:",
		"Failed to open URLs file:",
		"You must specify the \"-etp-rt\" option!",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}
