# Crunchyroll Downloader

[![Tests](https://img.shields.io/github/actions/workflow/status/CuteTenshii/crunchyroll-downloader/tests.yml?branch=master&label=tests)](https://github.com/CuteTenshii/crunchyroll-downloader/actions/workflows/tests.yml)
[![Latest release](https://img.shields.io/github/v/release/CuteTenshii/crunchyroll-downloader)](https://github.com/CuteTenshii/crunchyroll-downloader/releases/latest)
[![Downloads](https://img.shields.io/github/downloads/CuteTenshii/crunchyroll-downloader/total)](https://github.com/CuteTenshii/crunchyroll-downloader/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/CuteTenshii/crunchyroll-downloader)](go.mod)
[![License](https://img.shields.io/github/license/CuteTenshii/crunchyroll-downloader)](LICENSE.txt)

Downloads anime from Crunchyroll and outputs them in a MKV file.

You won't be banned or anything, I downloaded all Kaguya-Sama seasons to test during 30 mins and everything went fine

## Features

- Supports choosing the audio and subtitles language, including downloading multiple of each into a single file
- Supports choosing the audio and video quality
- Decrypts Widevine DRM (requires: a `.wvd` file or `client_id.bin` and `private_key.pem` files)
- Adds metadata (like episode name) to the MKV container
- Parallel segment downloads (10 workers) for faster downloads
- Retry with backoff on connection errors
- Batch download from a list of URLs
- Optional upload of each finished file to an SFTP server, deleting the local copy to save disk space
- Optional web interface (`--serve`) with a wishlist, a download queue, live progress, the full log of every run and the settings in a form, so a server can be left downloading unattended

## Requirements

- [FFmpeg](https://www.ffmpeg.org/download.html#get-packages)
- To download Premium-only content, a Crunchyroll Premium account. No, this can't be bypassed and a free trial should be enough
- Either a `.wvd` file, or a `client_id.bin` and `private_key.pem`

## Download

Check the [latest release](https://github.com/CuteTenshii/crunchyroll-downloader/releases/latest) and download the file that corresponds to your OS.

## Usage

- Open a Terminal/Command prompt, and go to the folder where you downloaded the binary/cloned the repo
- Run the program with the options you want:
```shell
Usage of ./crunchyroll-downloader:
  -audio-lang string
        Audio language(s), comma-separated (e.g. "ja-JP,en-US"). Add ALL for every available dub; explicit languages lead, the first available track is default, and ALL alone prefers ja-JP (default "ja-JP")
  -audio-quality string
        Audio quality (default "192k")
  -cc-lang string
        Closed caption language(s), comma-separated (e.g. "en-US,ALL"). Add ALL for every available caption; downloaded in addition to --subs-lang
  -debug-manifest
        Log raw episode playback JSON and manifest XML
  -download-delay duration
        Minimum delay between episode downloads, to help avoid Crunchyroll's rate limiting.
        Defaults to 40s; pass 0 to download back to back (default 40s)
  -etp-rt string
        The "etp_rt" cookie value of your account (or set CRDL_ETP_RT)
  -file string
        Path to a text file with one URL per line
  -serve
        Run the web interface instead of downloading (settings live in --data-dir, not in flags)
  -listen string
        Address the web interface listens on (with --serve) (default "127.0.0.1:8080")
  -data-dir string
        Where the web interface keeps its settings, job list and logs (with --serve; default $XDG_DATA_HOME/crunchyroll-downloader)
  -season int
        Season number. Not used if an episode link is entered
  -subs-lang string
        Subtitle language(s), comma-separated (e.g. "fr-FR,ALL"). Add ALL for every available subtitle; explicit languages lead, the first available track is default, and ALL alone prefers en-US (default "en-US")
  -renumber-seasons
        Number the regular seasons 1..n, closing the gaps Crunchyroll leaves where it lists a special or extras among them (e.g. Dr. STONE: Season 04 becomes Season 03). Specials and extras are unaffected
  -absolute-numbering
        Put every episode in Season 01 and keep its absolute episode number, for long-running shows Crunchyroll splits into artificial seasons (e.g. One Piece: S04E62 becomes S01E62)
  -url string
        URL of the episode/season to download
  -video-quality string
        Video quality (default "1080p")
```

Ex: to download the first season of *Hell's Paradise*:
```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/series/GJ0H7Q5ZJ/hells-paradise --season 1 --etp-rt replace_this
```

To download a specific episode:
```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/watch/GE00198973JAJP/dawn-and-confusion --etp-rt replace_this
```

To batch download from a file (one URL per line):
```shell
./crunchyroll-downloader --file list.txt --etp-rt replace_this --subs-lang pt-BR
```

To download multiple audio tracks and subtitles into a single file (the first available requested track of each kind is set as the default):
```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/watch/GE00198973JAJP/dawn-and-confusion --etp-rt replace_this --audio-lang ja-JP,en-US --subs-lang en-US,es-419,de-DE
```

Add `ALL` to any language list to include every available language; listed languages come first. For example, `--subs-lang fr-FR,ALL` makes French the default subtitle and adds the rest:
```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/watch/GE00198973JAJP/dawn-and-confusion --etp-rt replace_this --audio-lang ja-JP,ALL --subs-lang fr-FR,ALL --cc-lang ALL
```

Episodes are spaced 40 seconds apart by default (`-download-delay`), which is
what keeps a long season from tripping Crunchyroll's rate limit in the first
place. A single episode is unaffected: the gap only applies between episodes.
Raise it for large batches, or pass `--download-delay 0` to download back to back:

```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/series/GJ0H7Q5ZJ/hells-paradise --season 1 --etp-rt replace_this --download-delay 2m
```

If Crunchyroll rate-limits an episode anyway, it's retried in place instead of
moving on and tripping the same limit on the next one: 5 minutes at first,
doubling with every consecutive hit up to 2 hours, and back to 5 minutes after
the next successful download.

The limit is per client, not per account: copying a fresh `etp_rt` cookie from
the browser does **not** lift a block that's already in place, so retrying every
minute only spends attempts against the window. Sitting idle is what clears it.

## Output layout

Files are written in the layout media servers like Jellyfin expect, so pointing a
library at the output folder just works:

```
<output-dir>/<Series> (<Year>)/Season 01/<Series> (<Year>) S01E03 - <Episode title> [1080p].mkv
```

For example:

```
One Piece (1999)/Season 04/One Piece (1999) S04E62 - The First Line of Defense_ [1080p].mkv
```

The series folder carries the launch year so remakes that share a title stay
separate, and every season gets its own `Season NN` folder. The `[1080p]` tag
matches the video quality that was downloaded.

An episode is skipped on a later run when the same `SxxExx` already exists
locally or on the SFTP server, whatever its title or quality tag says. That
matters because the title comes from the language Crunchyroll serves it in: an
episode downloaded with `--subs-lang es-419` is named differently from the same
episode downloaded with `--subs-lang en-US`, and matching on the whole file name
would download it twice. The skip message names the file it matched, so a
quality change isn't silent:

```
Episode 5 is already on the server as "Cowboy Bebop (1998) S01E05 - La balada
de los ángeles caídos [720p].mkv", skipping...
```

### Long-running shows (`--absolute-numbering`)

Crunchyroll splits some shows into artificial seasons. One Piece is one season on
TMDB/TheTVDB, but Crunchyroll lists 31 ("Season 1: East Blue (1-61)", "Season 4:
Alabasta (62-143)", ...). With `--absolute-numbering` every episode is written to
`Season 01` and keeps its episode number, which is already absolute on
Crunchyroll for these shows:

```
./crunchyroll-downloader --url <series URL> --etp-rt <cookie> --absolute-numbering

One Piece (1999)/Season 01/One Piece (1999) S01E62 - The First Line of Defense... [1080p].mkv
```

Without the flag you get `Season 04/... S04E62`, which Jellyfin won't match
against a single-season entry in TMDB. Only use it when the show really is
numbered absolutely upstream; for a normal show with real seasons it would pack
every season into Season 01.

### Uploading to an SFTP server

Set `CRDL_SFTP_HOST` and each file is uploaded right after it's downloaded, so
the whole season never has to fit on the local disk. The remote tree mirrors the
local one, so the server gets exactly the same layout:

```
/home/you/Videos/Cowboy Bebop (1998)/Season 01/Cowboy Bebop (1998) S01E01 - ....mkv
  -> /srv/media/series/Cowboy Bebop (1998)/Season 01/Cowboy Bebop (1998) S01E01 - ....mkv
```

```shell
export CRDL_SFTP_HOST=192.168.1.10
export CRDL_SFTP_USER=media
export CRDL_SFTP_PASSWORD=secret
export CRDL_SFTP_REMOTE_DIR=/srv/media/series

./crunchyroll-downloader --url <series URL> --etp-rt <cookie> --season 1
```

| Variable | Default | Meaning |
| --- | --- | --- |
| `CRDL_ETP_RT` | *(unset)* | The `etp_rt` cookie, used when `--etp-rt` is not given |
| `CRDL_SFTP_HOST` | *(unset)* | Server to upload to. **Unset means the feature is off** |
| `CRDL_SFTP_PORT` | `22` | SSH port |
| `CRDL_SFTP_USER` | `root` | SSH user |
| `CRDL_SFTP_PASSWORD` | *(unset)* | Password login |
| `CRDL_SFTP_KEY` | *(unset)* | Path to an SSH private key, as an alternative to the password |
| `CRDL_SFTP_REMOTE_DIR` | `/` | Directory the output tree is written under |
| `CRDL_SFTP_KNOWN_HOSTS` | *(unset)* | Verify the server's host key against this `known_hosts` file |
| `CRDL_SFTP_KEEP_LOCAL` | `false` | Set to `1`/`true` to keep local files after uploading |

Notes:
- A file is uploaded to `<name>.mkv.part` and renamed once complete, so media
  servers never index a half-transferred episode
- If an upload fails, the local file is kept and the error is printed: nothing is
  deleted before it's safely on the server
- Episodes already on the server are skipped on a later run, so an interrupted
  download resumes instead of starting over
- The host key is **not** verified unless `CRDL_SFTP_KNOWN_HOSTS` is set, in which
  case a mismatched or unknown key aborts the upload (the local file is kept) and
  prints the exact `ssh-keygen -R` / `ssh-keyscan` command that fixes it:

```shell
ssh-keyscan -p 22 192.168.1.10 >> ~/.ssh/known_hosts
export CRDL_SFTP_KNOWN_HOSTS=~/.ssh/known_hosts
```

### Specials and extras

Crunchyroll sometimes lists, as a "season", something that isn't a season of the
series: a one-off special, or a pile of bonus clips. Those are filed where media
servers look for them instead of as numbered seasons of the series:

```
Dr. STONE (2019)/
├── Season 01/          episodes 1..24
├── Season 02/          episodes 1..11
├── Season 04/          ("Season 3" on Crunchyroll)
├── Season 05/          ("SCIENCE FUTURE")
├── Specials/           Dr. STONE (2019) S00E01 - RYUSUI.mkv
└── Extras/             the bonus season
```

When a special or an extras season sits among the real seasons in Crunchyroll's
listing, the remaining seasons keep Crunchyroll's numbers and leave a hole. Add
`--renumber-seasons` to number them 1..n instead, so the folders are contiguous
and match how TMDB/TheTVDB index the show:

```shell
./crunchyroll-downloader --url https://www.crunchyroll.com/series/GYEXQKJG6/dr-stone --renumber-seasons --etp-rt replace_this
```

```
Dr. STONE (2019)/          without the flag        with --renumber-seasons
├── Season 01/             1                       1
├── Season 02/             2                       2
├── Season 04/             4 ("Season 3")          3
└── Season 05/             5 ("SCIENCE FUTURE")    4
```

Specials become season 0 from the file name down to the container's
`season_number` tag, which is how a special is told apart from an episode. The
classification comes from the season's title, so a season titled "Special
Episode" or "Extras"/"Bonus ..." is treated as such.

## Keeping the per-track files

An episode is downloaded as one file per track (video, each audio dub, each
subtitle) and merged with FFmpeg at the end, which then deletes them. Setting
`CRDL_KEEP_TEMP=1` keeps them next to the usual temporary directory, so a sync
or quality problem can be traced back to the tracks it came from:

```shell
CRDL_KEEP_TEMP=1 ./crunchyroll-downloader --url <episode URL> --etp-rt <cookie>
```

## Web interface

The same binary can run as a small web server, which is how it's meant to be used
on a machine that downloads on its own: add series from a browser, let it work
through them, and see what it's doing.

```shell
./crunchyroll-downloader --serve --listen 127.0.0.1:8080 --data-dir /var/lib/crunchyroll-downloader/state
```

The page has two things:

- **Series**: paste a Crunchyroll `/series/…` link (all seasons, or one) and
  it's queued straight away and remembered. There is one row per series, not one
  per run: the row shows the latest run — the current episode, the per-track
  progress, whether it's uploading, why it's waiting (a rate-limit backoff, a
  retry) and the episodes that failed — and how many times it has run.
  The progress bar is cumulative across runs: retrying a stuck, failed or
  interrupted series, or searching for new episodes, doesn't reset it to zero.
  A fresh run starts at 0/0 until it announces its season, so the bar keeps
  showing the furthest point any previous run reached; episodes the new run
  re-skips were already counted, and only the ones it passes beyond that move
  the bar. If new episodes appear, the total grows and the bar dips until
  they're downloaded. `Buscar nuevos` queues it again; episodes already on disk
  or on the server are skipped, so only what's new gets downloaded. Asking for
  a series that is already queued or downloading returns that run instead of
  queueing a second one, so pressing the button twice (or `Buscar nuevos en
  todas`, which covers every series at once) costs nothing. `Cancelar` stops
  the download and the FFmpeg it started, and `Registro` shows that run's
  full log.
- **Priority**: every series carries a priority — `normal`, `media`, `alta` or
  `urgente` (0 to 3 in the API) — chosen when it's added and changeable from its
  row. The worker always starts the waiting series with the highest number, so a
  show you want now jumps ahead of the ones already queued; equal priorities keep
  the old first-in-first-out order, which is what the queue did before priorities
  existed. Changing a priority also reorders the run that series already has
  waiting. A run that is already downloading keeps going: stopping it to restart
  it is your call, and `Cancelar` is there for that.
- **Order of the list**: series that haven't downloaded yet (or are waiting in
  the queue) come first, then the one downloading right now, then the ones that
  already ran; each group is sorted by priority, highest first, so the top row is
  what happens next. A failed or cancelled series counts as "already ran": it had
  its turn, and whatever is left of it is a retry.
- **Ajustes**: the `etp_rt` cookie, the output directory, and local vs SFTP —
  everything `--audio-lang`, `--video-quality`, `--download-delay` and
  `CRDL_SFTP_*` do on the command line. Saved in `config.json` (mode 0600) in the
  data directory; the page never shows the cookie or the SFTP password back, it
  only says whether they're set. The cookie field is forgiving about what gets
  pasted into it: the bare value, `etp_rt=<value>`, `etp_rt:"<value>"`, a whole
  cookie header, or a JSON-ish line all work — it's cleaned down to the bare
  value before it's saved (and for the CLI, before it's sent). A paste with no
  recognizable value is refused instead of overwriting a working cookie, and a
  cookie that was already stored in a wrapped form is cleaned when the server
  next starts, so it doesn't need to be pasted again.

State lives in two JSON files in `--data-dir` (`config.json`, `jobs.json`) plus
`logs/`, so backing up or moving an install is copying that directory. A job that
was running when the server stopped comes back as `interrupted` with a retry
button.

### HTTPS and a password

The Go server speaks plain HTTP on the loopback address and has no login of its
own; put a reverse proxy with a password in front of it. `install.sh` does that
with Caddy:

```shell
sudo ./install.sh --cdm /path/to/device.wvd
```

It installs FFmpeg and Caddy, builds (or takes `--binary`) the program into
`/opt/crunchyroll-downloader`, installs the Widevine device next to it, creates a
`crdl` system user with `/var/lib/crunchyroll-downloader` (state) and
`/srv/downloads` (default output), writes the `crld-web` systemd service, asks for
the web password and writes a Caddy site with `basic_auth` and `reverse_proxy`.
Running it again upgrades the binary and keeps the settings. `--site name.example`
uses that address instead of `:80`, which lets Caddy get a certificate on its own.

Useful afterwards:

```shell
journalctl -u crld-web -f                                  # the service
/var/lib/crunchyroll-downloader/state/logs/*.log            # one file per job
systemctl status crld-web caddy
```

### Updating an existing install (deploying a new build)

`install.sh` is for a first install. For a machine that's already running
(`crld-web` installed, `/opt/crunchyroll-downloader`, the data directory), a
new build is swapped in without touching the settings, the queue or the
downloaded files:

```shell
# 1. Build locally, from the repo root, and check it first
go test ./...
go build -trimpath -o /tmp/crunchyroll-downloader.next .

# 2. Upload it next to the installed copy, under a .next name, so the running
#    service keeps executing the old binary until the swap
scp /tmp/crunchyroll-downloader.next root@<host>:/root/crdl-install/crunchyroll-downloader.next
scp deploy/swap.py root@<host>:/root/crdl-install/swap.py

# 3. On the server: pause the queue, install, restart, re-queue whatever the
#    restart interrupted, and unpause
ssh root@<host> 'CRDL_WEB_USER=<user> CRDL_WEB_PASSWORD=<pass> python3 /root/crdl-install/swap.py'
```

`deploy/swap.py` reads the web basic-auth credentials from `CRDL_WEB_USER` /
`CRDL_WEB_PASSWORD` (never hardcode them in the repo), pauses the queue
(`POST /api/pause`) so no job starts mid-swap, installs the `.next` binary
over `/opt/crunchyroll-downloader/crunchyroll-downloader`, restarts
`crld-web`, re-queues every job the restart left `interrupted`
(`POST /api/jobs/{id}/retry` — it resumes from what's already on disk), and
unpauses. It prints the titles it re-queued.

Verify afterwards that what runs is what you built:

```shell
md5sum /tmp/crunchyroll-downloader.next        # local
ssh root@<host> 'md5sum /opt/crunchyroll-downloader/crunchyroll-downloader; systemctl is-active crld-web'
```

Rules that keep a deploy safe: never overwrite the running binary in place,
never delete or edit `config.json` / `jobs.json` by hand (that's the queue,
the wishlist and the secrets), and don't run a second downloader by hand
while the service is up — the rate limit is per client, so two processes
race each other into a block.

## Using an AI coding agent

The web interface is the operator's surface; an agent is most useful for
the code, for diagnosing a failed run, and for deploying. These prompts are
meant to be copied as-is (fill in the `<…>` parts). They encode the lessons
from real incidents — the wrapped-cookie `invalid_grant`, the progress bar
that reset on every re-queue, the queue left hanging after a failure — so
the agent doesn't have to rediscover them.

Safety rules every prompt assumes (give them to the agent once, or paste
them in): never print, commit or log the `etp_rt` cookie, the SFTP password
or the web password; never delete `config.json`, `jobs.json` or `logs/`;
commit only the files the task touches (`git add <file>`, not `git add -A`);
run `go test ./...` and `go vet ./...` before committing.

### Diagnose a failed download

```text
A download in the web interface failed with this error: <paste the error,
 e.g. "Couldn't log in: crunchyroll rejected the etp_rt cookie (HTTP 400:
 invalid_grant)">.
Find out why, in this order, and report what you find before changing anything:
1. In web_parse.go / token.go, what does this exact error mean and where is
   it produced?
2. On the server, check the stored cookie's shape only (length, and whether
   it contains spaces, ';', '=' or quotes) — never print its value. A value
   stored as etp_rt:"<uuid>" or with a header wrapper is rejected by
   Crunchyroll; the app now cleans pastes on save and on load, so say
   whether the running build has that fix.
3. If the shape is clean, the cookie is most likely expired or revoked:
   tell me to paste a fresh one in Ajustes (bare value only) and retry.
Don't retry the download in a loop: the rate limit is per client, and a
fresh cookie does not lift a block.
```

### Add a series, or search for new episodes, without losing progress

```text
Add <series URL> to the web interface (or press "Buscar nuevos" for it) and
confirm it queued. If the series already downloaded episodes, the progress
bar must keep showing them — it is cumulative across runs (seriesSnapshot
in web_jobs.go), so a fresh 0/0 run must not blank it. Check the row via
GET /api/state: done/total should be at least what the previous run
reached, and the new run should only be "running"/"queued", not a duplicate.
If the series is already queued or downloading, do not queue a second run.
```

### Update the cookie or another setting

```text
Update the etp_rt cookie in the web interface's Ajustes with the value I
paste below. Paste it exactly as given to the API (PUT /api/config,
{"etpRt": "..."}); the server cleans wrappers such as etp_rt:, quotes,
whitespace or a trailing ';' before storing. Afterwards confirm via
GET /api/config that hasEtpRt is true (never ask it to return the value),
and do not echo the cookie back to me. If the save is rejected, tell me the
paste had no recognizable value instead of overwriting the old cookie.
```

### Fix a bug

```text
Fix this bug: <describe the symptom and how to reproduce it>.
Start from the symptom, not from a guessed file: find where the behaviour
is produced, reproduce it with a failing Go test first (web_test.go has
stubBinary / testStore for driving the web queue and the log parser without
downloading anything), then make the smallest change that fixes it. Run
`go test ./...` and `go vet ./...`. Commit only the files you touched, one
commit with a message like "fix(web): ...", push to master, and tell me the
commit hash. Do not deploy unless I ask.
```

### Deploy a new build to the existing server

```text
Deploy the current master to the existing server (<host>, root SSH).
Follow the "Updating an existing install" section of the README exactly:
go build -trimpath to /tmp/crunchyroll-downloader.next, scp it to
/root/crdl-install/crunchyroll-downloader.next on the server, then run
deploy/swap.py there with the web credentials from the environment (never
print them). Afterwards verify: the md5 of
/opt/crunchyroll-downloader/crunchyroll-downloader matches the local build,
`systemctl is-active crld-web` is active, and report which series swap.py
re-queued. Do not touch config.json, jobs.json or the downloaded files.
```

### Check that the server and queue are healthy

```text
Check the server at <host> without changing anything: is crld-web active and
since when (systemctl show crld-web -p ActiveEnterTimestamp)? Via the API
(GET /api/state, with the basic-auth credentials from the environment),
what is the queue doing — which series is running, which are queued, which
are interrupted/failed, and is the progress bar (done/total) consistent
with the runs each series has had? If something is stuck, say what it is
and what the retry / "Buscar nuevos" button would do, but don't press it
or restart anything without asking me first.
```

### Get oriented in the codebase

```text
Explain how this app works, from the code: what main.go does with --serve,
how web_jobs.go (the queue, seriesSnapshot) and web_parse.go (turning the
child process's output lines into progress) fit together, where the settings
live (web_config.go, config.json) and how a job ends up running the same
binary as a child process. Point at the specific files and functions, and
mention the non-obvious invariants an agent must not break: one job at a
time, interrupting a run must resume from what's on disk, secrets are never
sent back to the browser, and progress is cumulative across runs.
```

## Building

### Requirements

- [Go](https://go.dev/dl/)

### Guide

- Clone this repository
- Open a Terminal/Command prompt, and go to the folder where you cloned the repo
- Run `go build .`

## Help

### How do I get my `etp_rt` cookie?

- Go to https://crunchyroll.com
- Open Developer Tools
- Firefox: Go to *Storage* then *Cookies*<br />Chrome: Go to *Application* then *Cookies*
- Select the Crunchyroll domain, then copy the `etp_rt` cookie value

![](.github/screenshots/etp-rt-cookie.png)

Only the **value** is needed — not the `etp_rt=` name and not the surrounding
quotes. If what got pasted includes them (copying from DevTools often
produces `etp_rt:"<value>"`), Crunchyroll rejects it with
`Couldn't log in: crunchyroll rejected the etp_rt cookie (HTTP 400:
…invalid_grant)`. The app cleans such pastes on save and on the next start,
so a cookie already stored that way recovers on its own; if the error
persists after that, the cookie itself has expired or been revoked and a
fresh one has to be pasted.

### What is a `.wvd` file and do I really need one?

Yes, Crunchyroll uses DRM-only content. This file is used to get a Widevine license, which gives the keys to decrypt the media.

If you don't have a rooted Android device or are just lazy, search "ready to use cdms" and you'll find plenty of websites providing those files.

## License

This project is licensed under the MIT License. See [LICENSE.txt](LICENSE.txt)
