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
  `Buscar nuevos` queues it again; episodes already on disk or on the server are
  skipped, so only what's new gets downloaded. Asking for a series that is
  already queued or downloading returns that run instead of queueing a second
  one, so pressing the button twice (or `Buscar nuevos en todas`, which covers
  every series at once) costs nothing. `Cancelar` stops the download and
  the FFmpeg it started, and `Registro` shows that run's full log.
- **Ajustes**: the `etp_rt` cookie, the output directory, and local vs SFTP —
  everything `--audio-lang`, `--video-quality`, `--download-delay` and
  `CRDL_SFTP_*` do on the command line. Saved in `config.json` (mode 0600) in the
  data directory; the page never shows the cookie or the SFTP password back, it
  only says whether they're set.

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

### What is a `.wvd` file and do I really need one?

Yes, Crunchyroll uses DRM-only content. This file is used to get a Widevine license, which gives the keys to decrypt the media.

If you don't have a rooted Android device or are just lazy, search "ready to use cdms" and you'll find plenty of websites providing those files.

## License

This project is licensed under the MIT License. See [LICENSE.txt](LICENSE.txt)
