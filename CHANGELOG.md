# Changelog

## Unreleased

- Series can now be given a priority (0-3: normal, media, alta, urgente) when they are added and changed afterwards from their row. The queue always starts the waiting series with the highest priority; equal priorities keep the old first-in-first-out order, so with no priority set the queue behaves exactly as before. Changing a priority also reorders the run that series already has waiting, and a run already downloading is left alone. The list is grouped: series that haven't downloaded yet (or are queued) first, then the one downloading, then the ones that already ran, each group by priority
- Dependencies updated (mp4ff 0.63, sftp 1.13.12, x/crypto 0.59, go-mpd) and `go.mod` now requires Go 1.26
- PSSH extraction now parses the manifest directly instead of relying on `go-mpd`'s `Descriptor`. Upstream changed the struct tag from `pssh` to `cenc:pssh`, which `encoding/xml` reads as a literal element name and so never matches a namespaced `<cenc:pssh>`; upgrading the module silently made every download fail with "PSSH not found". A test now covers both the prefixed and unprefixed element
- An invalid episode URL now fails with `episode X not found (HTTP 404); check the URL` instead of crashing with `panic: runtime error: index out of range [0] with length 0`
- Episodes are now laid out as `<Series> (<Year>)/Season NN/<Series> (<Year>) SxxExx - <Title>.mkv` so Jellyfin and other media servers match them against their own databases without manual renaming
- Added an extra request per series (cached) to fetch the series launch year used in the folder name
- Added `--absolute-numbering` for shows Crunchyroll splits into artificial seasons (One Piece, Detective Conan): episodes go to `Season 01` with their absolute episode number, matching how TMDB/TheTVDB index them
- An episode is now skipped when the same `SxxExx` already exists locally or on the SFTP server, instead of requiring an exact file name match. Changing `--subs-lang` renames the file (the title is served in that language), which used to download the whole episode again and leave a duplicate on the server
- Fixed the audio running several seconds ahead of the picture in every downloaded episode. Crunchyroll's audio track declares its edit list media time in the manifest's 10^7 timescale while the track itself is 48 kHz, so the value `426667` means 0.04s to a correct reader but 8.889s to FFmpeg. Because every track is merged as a separate input, FFmpeg aligned them by those start times and pushed the picture 8.889s forward. The merge now passes `-ignore_editlist 1` for every input, which leaves one frame of residual offset from the codec priming samples instead of nine seconds (`inputArgs` in `output.go`, `output_merge_test.go`)
- Added `CRDL_KEEP_TEMP` to keep the downloaded per-track temporary files after a merge, so a sync or quality problem can be traced back to the tracks it came from
- An episode that failed for a reason other than rate limiting (a connection reset, a dropped route) used to be abandoned while the run moved on to the next one, leaving a hole in the season that nothing reported. It's now retried three times (15s, 30s, 1m) and, if it still fails, listed at the end of the season with the exact episodes that need another run
- Rate limiting is now handled without being asked to. A rate-limited episode retries after 5 minutes instead of 1 (doubling up to 2 hours), and episodes are spaced 40s apart by default (`--download-delay`), because Crunchyroll's limit is per client rather than per account: a fresh cookie on the same machine stays blocked, and retrying every minute only spends attempts against the window. Pass `--download-delay 0` to download back to back
- "Video quality X not found, deferring to ..." now names the quality actually used (`720p`) instead of an internal representation ID
- Added optional SFTP upload: setting `CRDL_SFTP_HOST` uploads every file to the server right after it's downloaded, mirroring the local folder layout, and then deletes the local copy so a full season doesn't need to fit on the local disk. Configured with the `CRDL_SFTP_*` environment variables (`README`); episodes already on the server are skipped on a later run, and a failed upload keeps the local file
- Added `CRDL_SFTP_KNOWN_HOSTS` to verify the SFTP server's host key; without it the key is accepted as-is (LAN default). A mismatch or unknown host is reported with the exact `ssh-keygen -R` / `ssh-keyscan` command to fix it
- A season Crunchyroll lists that isn't a season of the series now goes where media servers look for it: a special ("Dr. STONE Special Episode - RYUSUI") goes to `Specials/` as season 0, and an Extras or bonus season goes to `Extras/`, instead of both landing in numbered season folders of the series and showing up in Jellyfin as episodes it doesn't have
- Added `--renumber-seasons` for shows where a special or an extras season sits among the real seasons in Crunchyroll's listing: the regular seasons are numbered 1..n so the folders are contiguous and match TMDB/TheTVDB (Dr. STONE goes from `Season 01, 02, 04, 05` to `Season 01..04`). Specials and extras aren't counted, so they never cause a gap
- `--etp-rt` can be supplied through `CRDL_ETP_RT` instead of the command line, so the cookie doesn't sit in shell history or in a scheduled command
- Added `CRDL_PROGRESS=plain`, which turns the animated progress bars into one plain line per update (`progress: video 62% 22.0 Mbps`), for logs and for anything that isn't a terminal
- Added a web interface (`--serve`), so a machine can be left downloading without a terminal: a wishlist of series that queues a job when added and can be re-queued later for new episodes, a one-at-a-time queue with the current episode, per-track progress and the reason a job is waiting, the full log of every run (streamed by offset, downloadable as a file), cancel that also stops the FFmpeg the job started, and a settings form for the cookie, the output directory, local vs SFTP and the quality/language/delay options. The page runs the same binary as a child process with the same flags, so the verified download path is untouched
- The web interface lists one row per series instead of one per run: a series downloaded ten times over its life is still one thing the operator wants, so the row shows its latest run (progress, current episode, waiting reason, failures) and how many runs it has had
- A series that is already queued or downloading is no longer queued twice: "search new" returns the run that is already there, it survives the button being pressed twice, or applied to every series at once
- A job that was running when the server stopped is shown as `interrupted` with a retry button instead of being stuck as running
- The web settings are stored in `config.json` (mode 0600) and never sent back to the browser: `GET /api/config` reports `hasEtpRt` / `hasSecret` instead of the values
- Added `install.sh` and `deploy/`: installs FFmpeg and Caddy, the binary and the Widevine device into `/opt/crunchyroll-downloader`, a `crdl` user and its directories, the `crld-web` systemd service, and a Caddy site with a hashed password in front of `127.0.0.1:8080`
- `install.sh` restarts Caddy after writing its site: the package starts Caddy before the file exists, so `enable --now` alone left it serving its own placeholder page with no password in front of the queue
- A run that found every episode already downloaded no longer leaves an empty season folder behind on the local disk: the folder is created when an episode is really going to be downloaded, and a local file deleted after its upload takes its empty folder with it
- The page reports a run as `failed` with the episodes that could not be downloaded even when the program exits immediately: its output is read from a pipe this process owns, so the last lines (the failure list) can no longer be lost when the child exits before they are read

## 1.6.0

- Added `--download-delay` to wait a minimum time between episode downloads, which helps avoid Crunchyroll's rate limiting on season and batch downloads ([#42](https://github.com/CuteTenshii/crunchyroll-downloader/issues/42), [#57](https://github.com/CuteTenshii/crunchyroll-downloader/issues/57))
- A rate-limited episode is now retried in place, waiting `--download-delay` (or 1 minute if unset) and doubling up to 30 minutes on repeated hits, instead of moving on and hitting the same limit on the next episode
- `ALL` can now be combined with specific languages in `--audio-lang`, `--subs-lang` and `--cc-lang` (e.g. `--subs-lang fr-FR,ALL`). The listed languages come first and the first one is the default track. `ALL` on its own prefers `ja-JP` audio and `en-US` subtitles
- Subtitles and closed captions are now collected from every downloaded audio version, so captions that only exist on a dub (like the English dub's English captions) are no longer reported as unavailable
- FFmpeg is now found when it's in the same folder as the downloader, which previously failed on Windows with "cannot run executable found relative to current directory". A missing FFmpeg is reported at startup instead of after every track has downloaded ([#61](https://github.com/CuteTenshii/crunchyroll-downloader/issues/61))
- An invalid or expired `--etp-rt` cookie now exits with a clear message instead of printing "Access token expired. Refetching one..." forever
- Fixed a crash when removing a player stream failed during cleanup

## 1.5.1

- Audio dubs, subtitles, closed captions and the video track now download concurrently instead of one after another, making multi-language downloads much faster
- Added progress bars showing percentage, segment count and download speed (Mbps) for every active download
- Playback errors now surface the API's error reason, and rate-limited (429) responses print a hint to wait or use a different account ([#47](https://github.com/CuteTenshii/crunchyroll-downloader/issues/47))
- Fixed a bug where retrying a request after refreshing an expired access token sent an empty or truncated body, breaking license requests

## 1.4.0

- Added support for on-demand (single-file) manifests and normalized non-Widevine PSSH system IDs, fixing "PSSH not found" and PlayReady errors on some episodes ([#8](https://github.com/CuteTenshii/crunchyroll-downloader/issues/8), [#38](https://github.com/CuteTenshii/crunchyroll-downloader/issues/38), [#39](https://github.com/CuteTenshii/crunchyroll-downloader/issues/39))
- Fixed a crash when the playback response "error" field is a boolean or number instead of a string ([#29](https://github.com/CuteTenshii/crunchyroll-downloader/issues/29))
- Fixed audio language selection so dubs resolve from the versions list instead of silently downloading the original language ([#35](https://github.com/CuteTenshii/crunchyroll-downloader/issues/35))
- Unavailable subtitle and caption locales are now skipped instead of aborting the whole episode ([#27](https://github.com/CuteTenshii/crunchyroll-downloader/issues/27))
- Unavailable audio locales are now skipped, downloading whichever of the requested languages are available ([#28](https://github.com/CuteTenshii/crunchyroll-downloader/issues/28))
- Fixed URL parsing for locale-prefixed links such as /fr/series/...

## 1.3.0

- Large downloads no longer buffer the entire file in memory. Segments are written to disk as they arrive, so memory use no longer scales with the length of the title — movie-length media previously crashed with an out-of-memory error
- The decryption key is now selected by matching each track's key ID, instead of always using the first key in the Widevine license, which could decrypt to garbage
- Added `--cc-lang` to download closed captions (dubtitles) in addition to `--subs-lang`
- `--audio-lang all` and `--subs-lang all` download every available track
- Subtitles no longer depend on which audio track came first
- Added CI: tests, cross-platform builds, vet and gofmt on every push and pull request

## 1.2.0

- Added support for downloading multiple audio tracks and multiple subtitles into a single file (`--audio-lang ja-JP,en-US`, `--subs-lang en-US,es-419`). The first of each is marked as the default track, and each track is tagged with its language so media servers can select them.
- An episode is skipped if any requested audio or subtitle language is unavailable for it.
- Parallel segment downloads (10 workers) for much faster downloads
- Retry with backoff on connection errors instead of crashing
- Added `--urls` flag to batch download from a text file with one URL per line
- Invalid URLs in batch mode are skipped instead of stopping the whole process

## 1.1.1

- Optimized code, tried to handle errors
- Some random fixes
- Added a way to automatically refetch an access token if the current one expires

## 1.1.0

- Added support for downloading entire seasons
- Fixed MPD parsing
- Temporary downloaded files (video, audio segments and subtitles) are now stored in the OS temporary files then deleted
- Fixed FFmpeg merge command
- Docs improvements
- Support for `device_id.bin` and `private_key.pem` files

## 1.0.0

Initial release
