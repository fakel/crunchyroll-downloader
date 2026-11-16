package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	runtimedebug "runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/iyear/gowidevine"
	"github.com/unki2aut/go-mpd"
)

const maxWorkers = 10

func buildUrl(base, representationId, file string, partNum *int64) string {
	if partNum != nil {
		file = strings.ReplaceAll(file, "$Number$", fmt.Sprintf("%05d", *partNum))
		file = strings.ReplaceAll(file, "$Number%05d$", fmt.Sprintf("%05d", *partNum))
	}
	return base + strings.ReplaceAll(file, "$RepresentationID$", representationId)
}

func downloadPart(url string) ([]byte, error) {
	maxRetries := 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}

		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Origin", "https://static.crunchyroll.com")
		req.Header.Set("Referer", "https://static.crunchyroll.com/")
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if attempt < maxRetries-1 {
				continue
			}
			return nil, fmt.Errorf("failed after %d retries: %w", maxRetries, err)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			if attempt < maxRetries-1 {
				continue
			}
			return nil, fmt.Errorf("failed after %d retries, status: %d", maxRetries, resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			if attempt < maxRetries-1 {
				continue
			}
			return nil, fmt.Errorf("failed reading body after %d retries: %w", maxRetries, err)
		}
		return body, nil
	}
	return nil, fmt.Errorf("failed after %d retries", maxRetries)
}

func getFilename(set *mpd.AdaptationSet, subExt string) string {
	if set == nil {
		if subExt == "" {
			subExt = "ass"
		}
		f, _ := os.CreateTemp("", "crdl-subs-*."+subExt)
		return f.Name()
	}
	for _, representation := range set.Representations {
		if representation.Height != nil {
			f, _ := os.CreateTemp("", "crdl-video-*.mp4")
			return f.Name()
		} else if representation.Bandwidth != nil {
			f, _ := os.CreateTemp("", "crdl-audio-*.mp3")
			return f.Name()
		}
	}
	return ""
}

// maxBufferedSegments bounds how many segments may be resident in memory at
// once, counting both in-flight downloads and payloads still waiting to be
// written. Without it the workers race arbitrarily far ahead of the sequential
// writer whenever an early segment is slow, which is what exhausted memory on
// movie-length titles.
const maxBufferedSegments = maxWorkers * 2

// streamSegments fetches every url concurrently but writes the payloads to w
// strictly in index order, releasing each one as soon as it has been written.
// Peak memory is therefore bounded by maxBufferedSegments rather than growing
// with the length of the media.
//
// onProgress, if non-nil, is called with the running count of fetched segments.
func streamSegments(w io.Writer, urls []string, fetch func(string) ([]byte, error), onProgress func(fetched int64)) error {
	total := len(urls)
	if total == 0 {
		return nil
	}

	var (
		mu      sync.Mutex
		cond    = sync.NewCond(&mu)
		payload = make([][]byte, total)
		ready   = make([]bool, total)
		failure error
	)

	abort := make(chan struct{})
	var abortOnce sync.Once
	fail := func(err error) {
		mu.Lock()
		if failure == nil {
			failure = err
		}
		mu.Unlock()
		cond.Broadcast()
		abortOnce.Do(func() { close(abort) })
	}

	// A worker claims a slot before claiming an index, so the indices held at
	// any moment are always the lowest outstanding ones. That ordering is what
	// guarantees the writer's next index is always held by a live worker and
	// can never be starved by later segments hogging every slot.
	slots := make(chan struct{}, maxBufferedSegments)
	var next atomic.Int64
	var fetched atomic.Int64

	var wg sync.WaitGroup
	for n := 0; n < maxWorkers; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case slots <- struct{}{}:
				case <-abort:
					return
				}

				i := int(next.Add(1) - 1)
				if i >= total {
					<-slots
					return
				}

				data, err := fetch(urls[i])
				if err != nil {
					fail(err)
					return
				}

				mu.Lock()
				payload[i] = data
				ready[i] = true
				mu.Unlock()
				cond.Broadcast()

				if onProgress != nil {
					onProgress(fetched.Add(1))
				}
			}
		}()
	}

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; i < total; i++ {
			mu.Lock()
			for !ready[i] && failure == nil {
				cond.Wait()
			}
			if failure != nil {
				mu.Unlock()
				return
			}
			data := payload[i]
			payload[i] = nil
			mu.Unlock()

			_, err := w.Write(data)
			<-slots
			if err != nil {
				fail(fmt.Errorf("writing segment %d: %w", i, err))
				return
			}
		}
	}()

	wg.Wait()
	<-writerDone

	mu.Lock()
	defer mu.Unlock()
	return failure
}

func downloadParts(title string, baseUrl, representationId *string, set *mpd.AdaptationSet, keys []*widevine.Key) (string, error) {
	initUrl := buildUrl(*baseUrl, *representationId, *set.SegmentTemplate.Initialization, nil)
	initData, err := downloadPart(initUrl)
	if err != nil {
		return "", err
	}

	timeline := expandTimeline(set.SegmentTemplate.SegmentTimeline.S, 1)
	total := len(timeline)
	urls := make([]string, total)
	for i, item := range timeline {
		urls[i] = buildUrl(*baseUrl, *representationId, *set.SegmentTemplate.Media, &item)
	}

	filename := getFilename(set, "")
	encPath := filename + ".enc"
	encFile, err := os.Create(encPath)
	if err != nil {
		return "", err
	}
	defer os.Remove(encPath)
	defer encFile.Close()

	if _, err = encFile.Write(initData); err != nil {
		return "", fmt.Errorf("writing init segment: %w", err)
	}

	bar := newProgressBar(title, int64(total), "segments")
	var totalBytes atomic.Int64
	fetch := func(url string) ([]byte, error) {
		data, err := downloadPart(url)
		if err == nil {
			totalBytes.Add(int64(len(data)))
		}
		return data, err
	}
	err = streamSegments(encFile, urls, fetch, func(fetched int64) {
		bar.update(fetched, totalBytes.Load())
	})
	bar.finish()
	if err != nil {
		return "", err
	}

	if _, err = encFile.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewinding %s: %w", encPath, err)
	}

	file, err := os.Create(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()

	if err = decryptMP4(initData, encFile, keys, file); err != nil {
		return "", fmt.Errorf("decryptMP4: %w", err)
	}

	return filename, nil
}

// downloadAudioTrack downloads a version's audio representation into a temporary
// file. sets is non-nil only for on-demand manifests.
func downloadAudioTrack(manifest *mpd.MPD, sets []onDemandAdaptationSet, locale, quality string, keys []*widevine.Key) (string, error) {
	title := "Downloading " + trackTitle(locale) + " audio"
	if isOnDemand(manifest) {
		return downloadOnDemandAdaptation(title, sets, false, quality, keys)
	}
	audioSet := manifest.Period[0].AdaptationSets[1]
	audioBaseUrl, audioRepresentationId := getBaseUrl(audioSet, false, quality)
	if audioBaseUrl == nil {
		return "", fmt.Errorf("failed to get the audio base URL for %s, maybe the audio quality you entered is wrong?", locale)
	}
	return downloadParts(title, audioBaseUrl, audioRepresentationId, audioSet, keys)
}

// downloadVideoTrack downloads the video representation into a temporary file.
// The video track is identical across dubs, so it is downloaded only once.
func downloadVideoTrack(manifest *mpd.MPD, sets []onDemandAdaptationSet, quality string, keys []*widevine.Key) (string, error) {
	if isOnDemand(manifest) {
		return downloadOnDemandAdaptation("Downloading video", sets, true, quality, keys)
	}
	videoSet := manifest.Period[0].AdaptationSets[0]
	baseUrl, representationId := getBaseUrl(videoSet, true, quality)
	if baseUrl == nil {
		return "", fmt.Errorf("failed to get the video base URL, maybe the video quality you entered is wrong?")
	}
	return downloadParts("Downloading video", baseUrl, representationId, videoSet, keys)
}

func downloadSubs(url, format string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Origin", "https://static.crunchyroll.com")
	req.Header.Set("Referer", "https://static.crunchyroll.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:147.0) Gecko/20100101 Firefox/147.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	filename := getFilename(nil, format)
	file, err := os.Create(filename)
	if err != nil {
		return "", err
	}
	if _, err = file.Write(body); err != nil {
		file.Close()
		return "", err
	}
	file.Close()

	return filename, nil
}

// buildGuidByLocale maps each available audio locale to its playback GUID. The
// episode's "versions" list is authoritative; the episode's own content ID is
// only a fallback for single-audio content where the API lists no versions.
// Crunchyroll sets "audio_locale" to the preferred language (which may be a dub)
// while the episode ID still points at the original, so mapping audio_locale to
// the content ID directly would resolve the dub to the wrong (original) stream.
func buildGuidByLocale(info EpisodeInfo, baseContentId string) map[string]string {
	guidByLocale := map[string]string{}
	for _, v := range info.EpisodeMetadata.Versions {
		guidByLocale[v.AudioLocale] = v.GUID
	}
	if len(guidByLocale) == 0 && info.EpisodeMetadata.AudioLocale != "" {
		guidByLocale[info.EpisodeMetadata.AudioLocale] = baseContentId
	}
	return guidByLocale
}

// mergeSubtitleAndCaptions combines the subtitles and captions offered by every
// audio version of an episode. Subtitles (translation scripts) are usually
// identical across versions, but captions (closed captions) transcribe a
// specific dub and only appear on that version's playback. Taking the first
// downloadable entry per locale preserves the union without duplication.
func mergeSubtitleAndCaptions(episodes []Episode) (subtitles, captions map[string]*Subtitle) {
	subtitles = map[string]*Subtitle{}
	captions = map[string]*Subtitle{}
	for _, ep := range episodes {
		for locale, sub := range ep.Subtitles {
			if sub != nil && (subtitles[locale] == nil || subtitles[locale].URL == "") {
				subtitles[locale] = sub
			}
		}
		for locale, cc := range ep.Captions {
			if cc != nil && (captions[locale] == nil || captions[locale].URL == "") {
				captions[locale] = cc
			}
		}
	}
	return subtitles, captions
}

// filterAvailableLangs drops subtitle/caption locales that the episode does not
// offer, warning about each one instead of aborting the whole download. Subtitles
// are optional, so a missing locale (e.g. the default "en-US" on a movie) should
// not prevent the video and audio from being saved.
func filterAvailableLangs(langs []string, available map[string]*Subtitle, kind string, episode int) []string {
	var filtered []string
	for _, locale := range langs {
		if available[locale] == nil {
			fmt.Printf("! %s locale %s is not available for episode %v, skipping it.\n", kind, locale, episode)
			continue
		}
		filtered = append(filtered, locale)
	}
	return filtered
}

func requestsAll(langs []string) bool {
	for _, locale := range langs {
		if strings.EqualFold(locale, "all") {
			return true
		}
	}
	return false
}

// expandLangs keeps explicitly requested locales first, then adds available
// locales in stable order. With ALL alone, the usual default locale leads when
// offered. Lists without ALL are returned unchanged, including duplicates and
// missing locales, so their existing handling remains intact.
func expandLangs(requested, available []string, preferred string) []string {
	if !requestsAll(requested) {
		return requested
	}

	selected := make([]string, 0, len(requested)+len(available))
	seen := make(map[string]bool, len(requested)+len(available))
	for _, locale := range requested {
		if strings.EqualFold(locale, "all") || seen[locale] {
			continue
		}
		selected = append(selected, locale)
		seen[locale] = true
	}
	if len(selected) == 0 {
		for _, locale := range available {
			if locale == preferred {
				selected = append(selected, locale)
				seen[locale] = true
				break
			}
		}
	}
	sort.Strings(available)
	for _, locale := range available {
		if !seen[locale] {
			selected = append(selected, locale)
			seen[locale] = true
		}
	}
	return selected
}

// additionalVersionGUIDs finds unselected dubs whose playback may offer other
// subtitles or captions. Sorting by locale keeps the first merged track for a
// repeated language stable across runs.
func additionalVersionGUIDs(guidByLocale map[string]string, selected []string) []string {
	seen := make(map[string]bool, len(selected))
	for _, guid := range selected {
		seen[guid] = true
	}
	locales := make([]string, 0, len(guidByLocale))
	for locale := range guidByLocale {
		if locale != "" {
			locales = append(locales, locale)
		}
	}
	sort.Strings(locales)
	var guids []string
	for _, locale := range locales {
		guid := guidByLocale[locale]
		if guid != "" && !seen[guid] {
			guids = append(guids, guid)
			seen[guid] = true
		}
	}
	return guids
}

// availableSubtitleLangs excludes entries with no downloadable file from ALL.
// Explicit requests still use filterAvailableLangs and retain their old rules.
func availableSubtitleLangs(tracks map[string]*Subtitle) []string {
	available := make([]string, 0, len(tracks))
	for locale, track := range tracks {
		if locale != "" && track != nil && track.URL != "" {
			available = append(available, locale)
		}
	}
	return available
}

// seasonEpisodeNumbers returns the season and episode numbers used both in the
// output path and in the container metadata, so the two never disagree.
//
// With -absolute-numbering every episode lands in Season 01 and keeps its
// episode number, which is how media servers index long-running shows that
// Crunchyroll splits into artificial seasons: One Piece's "Season 4" (Alabasta)
// is E62-143 of a single season everywhere else.
func seasonEpisodeNumbers(info EpisodeInfo) (season, episode int) {
	// -absolute-numbering puts every episode in Season 01 and keeps its own
	// number, which is how media servers index long-running shows that
	// Crunchyroll splits into artificial seasons.
	if absoluteNum != nil && *absoluteNum {
		return 1, info.EpisodeMetadata.EpisodeNumber
	}
	return info.EpisodeMetadata.SeasonNumber, info.EpisodeMetadata.EpisodeNumber
}

// jellyfinEpisodePath lays an episode out the way Jellyfin matches best:
//
//	<base>/<Series> (<Year>)/Season NN/<Series> (<Year>) SxxExx - <Title>.mkv
//
// The year is what lets Jellyfin tell remakes and reboots sharing a title
// apart, and the season folder keeps every season of a series together.
func jellyfinEpisodePath(baseDirectory string, info EpisodeInfo, videoQuality string) string {
	seriesName := sanitizeFilename(info.EpisodeMetadata.SeriesTitle)
	if info.SeriesLaunchYear > 0 {
		seriesName = fmt.Sprintf("%s (%d)", seriesName, info.SeriesLaunchYear)
	}
	season, episode := seasonEpisodeNumbers(info)
	return filepath.Join(
		baseDirectory,
		seriesName,
		seasonFolder(season),
		fmt.Sprintf("%s S%02dE%02d - %s [%s].mkv", seriesName, season, episode, sanitizeFilename(info.Title), videoQuality),
	)
}

// seasonFolder is the directory name holding a season's episodes.
func seasonFolder(season int) string {
	return fmt.Sprintf("Season %02d", season)
}

// episodeMarker is the part of a file name that identifies an episode no matter
// whose title or quality tag it was written with: "S01E05 - ". Matching on it
// keeps a re-run from downloading an episode twice when the title language or
// the requested quality changed.
func episodeMarker(info EpisodeInfo) string {
	season, episode := seasonEpisodeNumbers(info)
	return fmt.Sprintf("S%02dE%02d - ", season, episode)
}

// findEpisodeInDir reports the name of an existing file in dir that matches
// marker, so an episode already on disk is skipped even if its file name differs.
func findEpisodeInDir(dir, marker string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.Contains(entry.Name(), marker) {
			return entry.Name(), true
		}
	}
	return "", false
}

func downloadEpisode(baseContentId string, info EpisodeInfo, audioLangs, subsLangs, ccLangs []string, videoQuality, audioQuality *string, baseDirectory string) (err error) {
	outputFile := jellyfinEpisodePath(baseDirectory, info, *videoQuality)

	// The episode number is the identity here, not the file name: changing
	// --subs-lang renames the file (a Spanish run titles it differently from an
	// English one), and re-downloading a whole episode for a rename would put two
	// copies of it on the server.
	seasonDir, marker := filepath.Dir(outputFile), episodeMarker(info)
	if name, ok := findEpisodeInDir(seasonDir, marker); ok {
		fmt.Printf("Episode %v is already downloaded as \"%s\", skipping...\n", info.EpisodeMetadata.EpisodeNumber, name)
		return
	}

	// Created only once an episode is really going to be downloaded: doing it
	// before the skips above leaves an empty season folder behind after every
	// run that found nothing to do.
	if _, err := os.Stat(seasonDir); err != nil {
		_ = os.MkdirAll(seasonDir, 0777)
	}

	// Resolve each requested audio locale to its version GUID. Each dub is a
	// separate playback stream with its own manifest, token and Widevine keys.
	guidByLocale := buildGuidByLocale(info, baseContentId)

	availableAudio := make([]string, 0, len(guidByLocale))
	for locale, guid := range guidByLocale {
		if locale != "" && guid != "" {
			availableAudio = append(availableAudio, locale)
		}
	}
	audioLangs = expandLangs(audioLangs, availableAudio, "ja-JP")

	type audioVersion struct {
		locale    string
		contentId string
	}
	var versions []audioVersion
	for _, locale := range audioLangs {
		guid, ok := guidByLocale[locale]
		if !ok {
			fmt.Printf("! Audio locale %s is not available for episode %v, skipping it.\n", locale, info.EpisodeMetadata.EpisodeNumber)
			continue
		}
		versions = append(versions, audioVersion{locale: locale, contentId: guid})
	}
	if len(versions) == 0 {
		fmt.Printf("! None of the requested audio locales are available for episode %v, aborting this episode.\n", info.EpisodeMetadata.EpisodeNumber)
		return
	}

	season, episodeNumber := seasonEpisodeNumbers(info)
	fmt.Printf("Downloading: %s (S%02vE%02v) from %s\n", info.Title, season, episodeNumber, info.EpisodeMetadata.SeriesTitle)

	// Space this download out from the previous one, if a delay is configured.
	// Applied here rather than around the whole function so that episodes
	// already on disk (returned above) don't burn the wait for nothing.
	backoff.wait()

	// activeStreams tracks every playback token we open so we can release them
	// all if anything fails partway through.
	var (
		streamsMu     sync.Mutex
		activeStreams = map[string]string{}
	)
	defer func() {
		// Start the backoff clock as soon as this download is over, success or
		// not, so a failed/rate-limited attempt doesn't get retried immediately.
		backoff.done()

		print("Cleaning up...\n")

		streamsMu.Lock()
		streams := activeStreams
		activeStreams = map[string]string{}
		streamsMu.Unlock()

		for id, sToken := range streams {
			if err := deleteStream(id, sToken); err != nil {
				fmt.Printf("Failed to remove the player stream for %s: %v\n", id, err)
			}
		}
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
			if *debug {
				fmt.Printf("Recovered from error: %v\n%s\n", r, runtimedebug.Stack())
			} else {
				fmt.Printf("Recovered from error: %v\n", r)
			}
		}
	}()

	// Fetch selected dubs' playback up front. Captions are per-audio-locale,
	// and the first version alone may not contain the requested captions.
	episodes := make([]Episode, len(versions))
	selectedGUIDs := make([]string, 0, len(versions))
	for i, version := range versions {
		ep, err := getEpisode(version.contentId)
		if err != nil {
			panic(err)
		}
		episodes[i] = ep
		selectedGUIDs = append(selectedGUIDs, version.contentId)
		streamsMu.Lock()
		activeStreams[version.contentId] = ep.Token
		streamsMu.Unlock()
	}
	if requestsAll(subsLangs) || requestsAll(ccLangs) {
		// ALL covers the episode, including captions attached to dubs whose
		// audio was not selected. Keep these playback tokens until the tracks
		// have downloaded, but do not download their audio streams.
		for _, guid := range additionalVersionGUIDs(guidByLocale, selectedGUIDs) {
			ep, fetchErr := getEpisode(guid)
			if fetchErr != nil {
				if errors.Is(fetchErr, ErrRateLimited) {
					panic(fetchErr)
				}
				fmt.Printf("! Playback %s is unavailable; skipping its subtitles and captions: %v\n", guid, fetchErr)
				continue
			}
			episodes = append(episodes, ep)
			streamsMu.Lock()
			activeStreams[guid] = ep.Token
			streamsMu.Unlock()
		}
	}

	// Merge subtitles and captions across versions. Subtitles (translation
	// scripts) are usually identical across versions, while captions are the
	// per-dub transcriptions that only exist on their own version.
	subtitles, captions := mergeSubtitleAndCaptions(episodes)

	subsLangs = expandLangs(subsLangs, availableSubtitleLangs(subtitles), "en-US")
	ccLangs = expandLangs(ccLangs, availableSubtitleLangs(captions), "")

	fmt.Printf("Audio locales: %s | Subtitle locales: %s | CC locales: %s\n",
		strings.Join(audioLangs, ", "), strings.Join(subsLangs, ", "), strings.Join(ccLangs, ", "))

	subsLangs = filterAvailableLangs(subsLangs, subtitles, "Subtitle", info.EpisodeMetadata.EpisodeNumber)
	ccLangs = filterAvailableLangs(ccLangs, captions, "Closed caption", info.EpisodeMetadata.EpisodeNumber)

	// Build the list of subtitle and caption downloads.
	type subJob struct {
		url    string
		format string
		locale string
		isCC   bool
	}
	var subJobs []subJob
	for _, locale := range subsLangs {
		sub := subtitles[locale]
		subJobs = append(subJobs, subJob{url: sub.URL, format: sub.Format, locale: locale})
	}
	for _, locale := range ccLangs {
		cc := captions[locale]
		subJobs = append(subJobs, subJob{url: cc.URL, format: cc.Format, locale: locale, isCC: true})
	}

	// Download every subtitle, every audio dub and the video track concurrently.
	// Each dub has its own playback token and license keys, so they no longer
	// need to be serialized behind a single global key set.
	subTracks := make([]mediaTrack, len(subJobs))
	audioTracks := make([]mediaTrack, len(versions))
	var videoFile string

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil && err != nil {
			firstErr = err
		}
		errMu.Unlock()
	}

	for j, job := range subJobs {
		wg.Add(1)
		go func(j int, job subJob) {
			defer wg.Done()
			file, err := downloadSubs(job.url, job.format)
			if err != nil {
				fail(fmt.Errorf("downloading subtitles for %s: %w", trackTitle(job.locale), err))
				return
			}
			subTracks[j] = mediaTrack{file: file, locale: job.locale, format: job.format, isCC: job.isCC}
		}(j, job)
	}

	for i, version := range versions {
		wg.Add(1)
		go func(i int, version audioVersion) {
			defer wg.Done()

			episode := episodes[i]

			manifest, body, err := parseManifest(episode.ManifestURL)
			if err != nil {
				fail(fmt.Errorf("parsing manifest for %s: %w", version.locale, err))
				return
			}
			pssh := getPssh(body)
			if pssh == nil {
				fail(fmt.Errorf("PSSH not found for %s", version.locale))
				return
			}
			keys, err := getLicense(*pssh, version.contentId, episode.Token)
			if err != nil {
				fail(fmt.Errorf("getLicense for %s: %w", version.locale, err))
				return
			}

			var sets []onDemandAdaptationSet
			if isOnDemand(manifest) {
				var err error
				sets, err = parseOnDemand(body)
				if err != nil {
					fail(err)
					return
				}
			}

			if i == 0 {
				// The video and the first audio track share this version's keys,
				// so they download concurrently with each other and everything
				// else.
				type trackResult struct {
					file string
					err  error
				}
				videoCh := make(chan trackResult, 1)
				go func() {
					file, err := downloadVideoTrack(manifest, sets, *videoQuality, keys)
					videoCh <- trackResult{file, err}
				}()

				audioFile, err := downloadAudioTrack(manifest, sets, version.locale, *audioQuality, keys)
				if err != nil {
					// Join the video download before unwinding so its progress
					// bar can't race the deferred cleanup.
					<-videoCh
					fail(err)
					return
				}
				audioTracks[i] = mediaTrack{file: audioFile, locale: version.locale}

				video := <-videoCh
				if video.err != nil {
					fail(video.err)
					return
				}
				videoFile = video.file
			} else {
				audioFile, err := downloadAudioTrack(manifest, sets, version.locale, *audioQuality, keys)
				if err != nil {
					fail(err)
					return
				}
				audioTracks[i] = mediaTrack{file: audioFile, locale: version.locale}
			}

			if err := deleteStream(version.contentId, episode.Token); err != nil {
				fmt.Printf("Failed to remove the player stream for %s: %v\n", version.locale, err)
			}
			streamsMu.Lock()
			delete(activeStreams, version.contentId)
			streamsMu.Unlock()
		}(i, version)
	}

	wg.Wait()
	if firstErr != nil {
		panic(firstErr)
	}
	if len(subTracks) > 0 {
		fmt.Println("Downloaded subtitles!")
	}

	mergeEverything(videoFile, audioTracks, subTracks, outputFile, info)
	return nil
}

// transientRetries is how many extra attempts an episode gets when it fails for
// a reason other than rate limiting: a connection reset or a dropped route ends
// an episode without necessarily meaning anything is wrong with it.
const transientRetries = 3

// transientWait is the first retry delay for such a failure, doubled on each
// attempt: 15s, 30s, 1m.
const transientWait = 15 * time.Second

// downloadEpisodeWithRetry runs downloadEpisode, retrying the same episode
// with a growing delay whenever Crunchyroll rate-limits the account, instead
// of moving on to the next episode and immediately tripping the same rate
// limit again, and a few times over a transient network failure.
func downloadEpisodeWithRetry(baseContentId string, info EpisodeInfo, audioLangs, subsLangs, ccLangs []string, videoQuality, audioQuality *string, baseDirectory string) (err error) {
	transientAttempts := 0
	for {
		err := downloadEpisode(baseContentId, info, audioLangs, subsLangs, ccLangs, videoQuality, audioQuality, baseDirectory)
		if err == nil {
			backoff.resetRateLimit()
			return nil
		}

		if errors.Is(err, ErrRateLimited) {
			wait := backoff.rateLimitWait()
			retryAt := time.Now().Add(wait)
			fmt.Printf("Retrying this episode in %s [%s]...\n", wait.Round(time.Second), retryAt.Local().Format(time.Kitchen))
			time.Sleep(wait)
			continue
		}

		if transientAttempts >= transientRetries {
			return err
		}
		wait := transientWait << transientAttempts
		transientAttempts++
		fmt.Printf("Download of %v failed (%v), retrying in %s [attempt %v/%v]...\n",
			episodeMarker(info), err, wait, transientAttempts, transientRetries)
		time.Sleep(wait)
	}
}

func downloadSeason(videoQuality, audioQuality *string, audioLangs, subsLangs, ccLangs []string, episodes []SeasonEpisode, baseDirectory string) {
	fmt.Printf("Downloading season %v of %s (%v episodes)\n\n", episodes[0].SeasonNumber, episodes[0].SeriesTitle, len(episodes))

	var failed []string

	for _, episode := range episodes {
		info := EpisodeInfo{
			EpisodeMetadata: EpisodeMetadata{
				SeriesTitle:        episode.SeriesTitle,
				SeriesID:           episode.SeriesID,
				SeasonNumber:       episode.SeasonNumber,
				EpisodeNumber:      episode.EpisodeNumber,
				AudioLocale:        episode.AudioLocale,
				Versions:           episode.Versions,
				AvailabilityStarts: episode.AvailabilityStarts,
			},
			Title: episode.Title,
		}
		// Cached, so this is one request for the whole season.
		info.SeriesLaunchYear = getSeriesInfo(episode.SeriesID).LaunchYear

		if err := downloadEpisodeWithRetry(episode.ID, info, audioLangs, subsLangs, ccLangs, videoQuality, audioQuality, baseDirectory); err != nil {
			failed = append(failed, fmt.Sprintf("%v (%v) - %v", episodeMarker(info), info.Title, err))
		}
	}

	// Say it plainly at the end of the season: an episode lost to a dropped
	// connection otherwise just isn't there, and nothing points at it.
	if len(failed) > 0 {
		fmt.Printf("\n%v episode(s) of %v could not be downloaded:\n", len(failed), episodes[0].SeriesTitle)
		for _, line := range failed {
			fmt.Printf("  %v\n", line)
		}
		fmt.Printf("Running the same command again retries just these.\n\n")
	}
}
