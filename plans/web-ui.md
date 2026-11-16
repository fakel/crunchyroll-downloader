# Web UI + service install for crunchyroll-downloader

## Context

The tool today is a CLI: one URL (or `--file` list) per run, options as flags,
`etp_rt`/SFTP settings from the environment, progress drawn with ANSI bars on the
terminal, and everything else (which series, what's queued, what failed) kept in
the operator's head or in `/tmp/crdl-batch.sh`.

The goal is to run it unattended on a Proxmox LXC: add series to a wishlist from
a browser, let it download in the background, watch progress and errors, and
choose whether episodes land on a local directory or are pushed to an SFTP
server. Plain HTML/CSS/JS, no build step, no framework.

## Decisions

- **One binary, one new mode.** `--serve` on the existing binary. Nothing else to
  build or deploy.
- **The web layer never reimplements downloading.** A job runs the same binary as
  a child process with flags and `CRDL_*` env. The download path stays the code
  that was verified against real episodes, a crash can't take the server down,
  and `SxxExx` skipping, rate-limit backoff and the new transient retries keep
  working untouched.
- **Auth is Caddy's job, not the app's.** The Go server binds `127.0.0.1:8080`
  and speaks plain HTTP; Caddy sits in front with `basic_auth` and a hashed
  password. No session code, no cookie handling, no crypto in the app.
- **Wishlist is a list, downloads are one-shot.** Adding a series stores it and
  queues a job (all seasons by default, or one season). A "search new" button
  re-queues it later; the existing `SxxExx` skip means only the missing episodes
  are fetched. No periodic scraping, because each scan spends requests against
  the per-IP rate limit that already cost hours.
- **State is a JSON file, not a database.** Wishlist + jobs in
  `jobs.json`, written through an atomic rename. A single-user LXC doesn't justify
  SQLite, and the file can be inspected or repaired by hand.
- **One job at a time**, single worker goroutine. Two concurrent runs would just
  race each other into the same rate limit.
- **Secrets are editable from the UI** and stored in `config.json` (mode 0600).
  `GET /api/config` never returns them: it reports `hasEtpRt` / `hasPassword`.

## Files to modify

| Path | Change |
| --- | --- |
| `main.go` | `-serve`, `-listen`, `-data-dir` flags; branch into `serveHTTP` before the `--url` requirement |
| `progress.go` | plain (non-ANSI) progress lines when `CRDL_PROGRESS=plain`, so a piped log is readable and parseable |
| `web.go` (new) | HTTP server, routes, static files, request/response types |
| `web_jobs.go` (new) | job model, persistent store, single worker, child process runner, log parser |
| `web_config.go` (new) | config struct, load/save, env translation for the child |
| `web/static/index.html` (new) | the page: wishlist, queue, progress, log, settings |
| `web/static/app.css` (new) | layout, no framework |
| `web/static/app.js` (new) | polling, rendering, form → API |
| `web_test.go` (new) | config round-trip, log parser, queue transitions, handler status codes |
| `install.sh` (new) | deps, build, dirs, systemd unit, Caddy site |
| `deploy/crld-web.service` (new) | systemd unit |
| `deploy/Caddyfile` (new) | reverse proxy + basic auth template |
| `README.md`, `CHANGELOG.md` | document the mode and the install |

## Reuse

- Flag defaults in `main.go` (`audioLang`, `subtitlesLang`, `ccLang`, `videoQuality`, `audioQuality`, `downloadDelay`, `absoluteNum`, `seasonNumber`) — the settings form mirrors them, and one place maps names to flags.
- `remoteFromEnv()` and the `envSFTP*` constants in `remote.go` — `web_config.go` writes exactly those variables into the child's env; no second SFTP path.
- `sanitizeFilename` (`utils.go`) for log file names.
- `episodeMarker`/`seasonEpisodeNumbers` semantics — the UI labels an episode with the same `SxxExx` the skip logic uses.
- Existing retry/backoff in `download.go` — untouched, inherited by every job.

## Job model

```go
type Job struct {
    ID       string
    URL      string
    SeriesID string      // wishlist entry it came from, "" for ad-hoc
    Season   int        // 0 = all seasons
    Options  Options    // quality, langs, delay, absolute numbering
    Status   Status     // queued | running | done | failed | cancelled | interrupted
    Total    int        // episodes, from "Downloading season N of X (24 episodes)"
    Done     int        // counted from "Download finished! Output file:"
    Current  string     // "S01E05 - La balada de los ángeles caídos"
    Tracks   map[string]float64 // "video", "audio 日本語" -> percent
    Uploading bool
    Failures []string   // "S01E19 - A la modernidad - ..."
    LogPath  string
    Err      string
    CreatedAt, StartedAt, FinishedAt time.Time
}
```

Wishlist entries and jobs are separate arrays in the same store; deleting a
wishlist entry keeps its history.

## Log parsing

The child runs with `CRDL_PROGRESS=plain`, which turns each progress bar into one
stable line; the parser matches on prefixes only, never on prose:

| Line | Effect |
| --- | --- |
| `Downloading season N of X (24 episodes)` | job series title + total |
| `Downloading: <title> (S01E05) from X` | current episode, reset tracks |
| `progress: video 42% 22.0 Mbps` | `Tracks["video"] = 42` |
| `progress: audio 日本語 42% 21.9 Mbps` | `Tracks["audio 日本語"] = 42` |
| `Episode N is already … skipping` | counted as done |
| `Download finished! Output file: …` | done++, uploading if SFTP |
| `Uploaded <name>` / `Upload failed …` | upload state |
| `Waiting … before the next download` | waiting state + ETA |
| `Retrying this episode in 5m0s [9:56PM]` | rate-limited state + retry time |
| `N episode(s) of X could not be downloaded:` + following lines | `Failures` |

Anything unmatched still goes to the log verbatim, so the full text is never
lost. Raw `\x1b[...]` sequences are stripped defensively.

## HTTP API

| Route | Purpose |
| --- | --- |
| `GET /` | the page (embedded with `go:embed`) |
| `GET /api/state` | wishlist, jobs, running job, config (secrets masked) |
| `POST /api/series` | `{url, season?}` → add to wishlist and queue |
| `DELETE /api/series/{id}` | remove from wishlist |
| `POST /api/series/{id}/download` | "search new" → queue a job |
| `POST /api/jobs` | ad-hoc job, same options as flags |
| `POST /api/jobs/{id}/cancel` | kill the child's process group |
| `DELETE /api/jobs/{id}` | drop from history |
| `GET /api/jobs/{id}/log?offset=N` | log tail as `{lines, next}` |
| `GET /api/jobs/{id}/log/download` | the whole file |
| `GET/PUT /api/config` | settings; empty secret fields keep the stored value |

Polling (`/api/state` + log offset every 1–2 s) instead of SSE: one mechanism for
everything, and a LAN UI doesn't need push.

## Child process

```go
cmd := exec.Command(cfg.BinaryPath, args...)
cmd.Env = cfg.childEnv()        // CRDL_ETP_RT + CRDL_SFTP_*
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}   // cancel kills ffmpeg too
```

Cancel sends `syscall.Kill(-pid, SIGTERM)`. On startup, jobs left in `running`
become `interrupted` (the process didn't survive the restart), which the UI shows
with a "retry" button.

## Frontend

One page, three columns collapsing to one on narrow screens:

```
+---------------------------------------------------------------+
| crunchyroll-downloader            [settings] [pause queue]    |
+---------------------------+-----------------------------------+
| WISHLIST                  | QUEUE                             |
| [URL input] [season] [add]| S01E05 ███████░░░ 62%  (2/24)     |
|                           |   video 62% · audio 日本語 58%    |
| Dr. STONE (2019)          |   uploading…                      |
|  search new | remove      | --------------------------------- |
| Cowboy Bebop (1998)       | failed job + failure list         |
+---------------------------+-----------------------------------+
| LOG (current job, tail, auto-scroll, download full log)       |
+---------------------------------------------------------------+
```

Settings form: cookie `etp_rt`, download directory, destination (local / SFTP with
host, port, user, password, key, remote dir, known_hosts, keep-local), video and
audio quality, audio/subs/cc languages, delay, absolute numbering.

## `install.sh`

Runs as root on Debian/Ubuntu (proxmox LXC). Idempotent.

- [x] 1. `apt-get install -y ffmpeg caddy ca-certificates`
- [x] 2. Build the binary (or use a prebuilt one passed as `--binary`), install to `/opt/crunchyroll-downloader/crunchyroll-downloader`
- [x] 3. Install the CDM: `--cdm <file>` or copy `device.wvd` next to the binary; abort with a clear message if neither exists
- [x] 4. Create the system user `crdl` and dirs: `/var/lib/crunchyroll-downloader` (state, logs, 0700), `/srv/downloads` (default local target)
- [x] 5. systemd unit `crld-web.service`: `--serve --listen 127.0.0.1:8080 --data-dir /var/lib/crunchyroll-downloader/state`, `User=crdl`, `WorkingDirectory=/opt/crunchyroll-downloader`, `Restart=always`, `NoNewPrivileges`, `PrivateTmp`
- [x] 6. Ask for the web password (or `--password-file`), `caddy hash-password`, write `/etc/caddy/Caddyfile` with `basic_auth` + `reverse_proxy 127.0.0.1:8080`
- [x] 7. `systemctl enable --now crld-web caddy`, print the URL and the log paths

## Steps

- [x] 1. `progress.go`: `CRDL_PROGRESS=plain` → one line per bar, no cursor moves
- [x] 2. `web_config.go`: struct, JSON load/save (0600, atomic), env translation, defaults
- [x] 3. `web_jobs.go`: store, statuses, single worker, atomic persistence
- [x] 4. Child runner: argv building, env, stdout scanner, process-group cancel
- [x] 5. Log parser per the table above
- [x] 6. `web.go`: routes, embedded static files, validation and error responses
- [x] 7. `web/static/*`: the page
- [x] 8. `main.go`: `-serve`/`-listen`/`-data-dir` branch
- [x] 9. `deploy/` + `install.sh`
- [x] 10. Tests: `go test` with a stub "binary" (configurable path) that prints canned lines
- [x] 11. Docs

## Verification

- `go vet ./...`, `go test ./...`, `go build`
- `./crunchyroll-downloader --serve --listen 127.0.0.1:8080 --data-dir /tmp/crld-state`
- Through the UI: add a series, queue it, watch a real episode progress and land (local, then with SFTP configured), read the log, cancel mid-episode and confirm ffmpeg dies with it
- Restart the server mid-download: the job shows `interrupted`, retry skips what's already on the server
- In a fresh LXC: `install.sh`, then confirm `systemctl status`, the Caddy password prompt, and that the page is reachable and unreachable without credentials
