#!/usr/bin/env bash
#
# Installs the Crunchyroll Downloader web interface on a Debian/Ubuntu machine or
# a Proxmox LXC: the binary, the Widevine device, a system user, a systemd service
# and a Caddy reverse proxy with a password in front.
#
# Run it as root, from the source directory (or pass --binary). It is idempotent:
# running it again upgrades the binary and leaves the settings in place.
#
#   ./install.sh --cdm /path/to/device.wvd
#   ./install.sh --binary ./crunchyroll-downloader --cdm ./device.wvd --password-file /root/crdl-pass
#
set -euo pipefail

APP_NAME="crunchyroll-downloader"
INSTALL_DIR="/opt/${APP_NAME}"
STATE_DIR="/var/lib/${APP_NAME}"
DOWNLOAD_DIR="/srv/downloads"
SERVICE_USER="crdl"
SERVICE_FILE="/etc/systemd/system/crld-web.service"
CADDYFILE="/etc/caddy/Caddyfile"
LISTEN="127.0.0.1:8080"

BINARY=""
CDM=""
PASSWORD_FILE=""
SITE_ADDRESS=":80"
WEB_USER="crdl"

usage() {
	cat <<EOF
Usage: $0 [options]

  --binary PATH        Use this binary instead of building from source
  --cdm PATH           The Widevine device (.wvd) to install
  --password-file PATH Read the web password from this file (first line)
  --site ADDRESS       Caddy site address (default "$SITE_ADDRESS"; use a DNS
                       name to get an automatic HTTPS certificate)
  --web-user NAME      Web login user (default "$WEB_USER")
  -h, --help           This text

Without --binary the script builds ./crunchyroll-downloader from the directory it
lives in, which needs Go installed (see the README).
EOF
}

log() { printf '\n== %s\n' "$*"; }
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--binary) BINARY="$2"; shift 2 ;;
	--cdm) CDM="$2"; shift 2 ;;
	--password-file) PASSWORD_FILE="$2"; shift 2 ;;
	--site) SITE_ADDRESS="$2"; shift 2 ;;
	--web-user) WEB_USER="$2"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option: $1 (try --help)" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run this as root"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------- dependencies
log "Installing ffmpeg, Caddy and the CA certificates"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ffmpeg caddy ca-certificates >/dev/null
command -v ffmpeg >/dev/null || die "ffmpeg is still missing after installation"
command -v caddy >/dev/null || die "caddy is still missing after installation (is the Caddy apt repository set up?)"

# --------------------------------------------------------------------- binary
log "Installing the program into ${INSTALL_DIR}"
install -d -m 755 "$INSTALL_DIR"

if [ -n "$BINARY" ]; then
	[ -f "$BINARY" ] || die "no such binary: $BINARY"
	install -m 755 "$BINARY" "${INSTALL_DIR}/${APP_NAME}"
else
	command -v go >/dev/null || die "Go isn't installed. Install it (see the README) or build the binary elsewhere and pass --binary"
	[ -f "${SCRIPT_DIR}/go.mod" ] || die "run this from the source directory, or pass --binary"
	(cd "$SCRIPT_DIR" && go build -trimpath -o "${INSTALL_DIR}/${APP_NAME}" .)
	chmod 755 "${INSTALL_DIR}/${APP_NAME}"
fi

# ------------------------------------------------------------------------ CDM
log "Installing the Widevine device"
if [ -z "$CDM" ]; then
	for candidate in "${SCRIPT_DIR}/device.wvd" "${SCRIPT_DIR}/cdms/device.wvd" "${INSTALL_DIR}/device.wvd"; do
		if [ -f "$candidate" ]; then CDM="$candidate"; break; fi
	done
fi
[ -n "$CDM" ] || die "no .wvd found. Pass one with --cdm <file>, or put device.wvd next to this script."
[ -f "$CDM" ] || die "no such CDM file: $CDM"
install -m 644 "$CDM" "${INSTALL_DIR}/device.wvd"

# --------------------------------------------------------------- user and dirs
log "Creating the ${SERVICE_USER} user and its directories"
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
	useradd --system --home-dir "$STATE_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
fi

install -d -m 700 -o "$SERVICE_USER" -g "$SERVICE_USER" "${STATE_DIR}/state"
install -d -m 755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$DOWNLOAD_DIR"

# ------------------------------------------------------------------- systemd
log "Writing ${SERVICE_FILE}"
install -m 644 "${SCRIPT_DIR}/deploy/crld-web.service" "$SERVICE_FILE"
systemctl daemon-reload

# --------------------------------------------------------------------- Caddy
log "Configuring the web password"
PASSWORD=""
if [ -n "$PASSWORD_FILE" ]; then
	[ -r "$PASSWORD_FILE" ] || die "cannot read $PASSWORD_FILE"
	PASSWORD="$(head -n 1 "$PASSWORD_FILE")"
else
	read -r -s -p "Password for the web interface (user ${WEB_USER}): " PASSWORD
	printf '\n'
	read -r -s -p "Repeat it: " PASSWORD_CONFIRM
	printf '\n'
	[ "$PASSWORD" = "$PASSWORD_CONFIRM" ] || die "the passwords don't match"
fi
[ -n "$PASSWORD" ] || die "the password can't be empty"

# Reading from stdin keeps the password out of the process list, but Caddy before
# 2.8 needs a terminal and fails with EOF, so fall back to --plaintext there
# (visible in `ps` for the moment the hash is computed, on a single-user box).
if HASH="$(printf '%s' "$PASSWORD" | caddy hash-password 2>/dev/null)" && [ -n "$HASH" ]; then
	:
else
	HASH="$(caddy hash-password --plaintext "$PASSWORD")"
fi
[ -n "$HASH" ] || die "caddy hash-password produced nothing; is this Caddy 2.x?"
unset PASSWORD PASSWORD_CONFIRM

# Caddy renamed `basicauth` to `basic_auth` in 2.8; use whichever this build has.
CADDY_VERSION="$(caddy version | head -n 1)"
CADDY_MAJOR="$(printf '%s' "$CADDY_VERSION" | sed -nE 's/^v?([0-9]+)\..*/\1/p')"
CADDY_MINOR="$(printf '%s' "$CADDY_VERSION" | sed -nE 's/^v?[0-9]+\.([0-9]+).*/\1/p')"
HASH_DIRECTIVE="basic_auth"
if [ -n "$CADDY_MAJOR" ] && [ -n "$CADDY_MINOR" ] &&
	{ [ "$CADDY_MAJOR" -lt 2 ] || { [ "$CADDY_MAJOR" -eq 2 ] && [ "$CADDY_MINOR" -lt 8 ]; }; }; then
	HASH_DIRECTIVE="basicauth"
fi
if [ "$HASH_DIRECTIVE" = "basicauth" ]; then
	log "Caddy ${CADDY_VERSION} is older than 2.8, using the 'basicauth' directive"
fi

log "Writing ${CADDYFILE} (site ${SITE_ADDRESS})"
install -d -m 755 "$(dirname "$CADDYFILE")"
sed -e "s|^:80 {|${SITE_ADDRESS} {|" \
	-e "s|__CRDL_USER__|${WEB_USER}|" \
	-e "s|__CRDL_HASH__|${HASH}|" \
	-e "s|basic_auth {|${HASH_DIRECTIVE} {|" \
	"${SCRIPT_DIR}/deploy/Caddyfile" >"$CADDYFILE"
chmod 644 "$CADDYFILE"

caddy validate --config "$CADDYFILE" --adapter caddyfile >/dev/null 2>&1 ||
	die "Caddy rejected ${CADDYFILE}; check it and run 'caddy validate --config ${CADDYFILE}'"

# -------------------------------------------------------------------- service
log "Starting the services"
systemctl enable --now crld-web >/dev/null
systemctl enable --now caddy >/dev/null
systemctl restart crld-web
# The apt package starts Caddy before its Caddyfile exists, so a plain
# "enable --now" leaves it serving its own placeholder page. Restart it so the
# site above is the one that actually loads.
systemctl restart caddy

sleep 1
if ! systemctl is-active --quiet crld-web; then
	die "crld-web didn't stay up. Check 'journalctl -u crld-web -n 50'"
fi

HOSTNAME_ADDR="$(hostname -I 2>/dev/null | awk '{print $1}')"
printf '\nDone.\n\n'
printf '  Open:        http://%s/\n' "${HOSTNAME_ADDR:-<this-machine>}"
printf '  Login:       user %s, the password you just typed\n' "$WEB_USER"
printf '  Settings:    the page itself (cookie, download dir, SFTP), stored in %s/state/config.json\n' "$STATE_DIR"
printf '  Downloads:   %s (or wherever the settings point)\n' "$DOWNLOAD_DIR"
printf '  Service log: journalctl -u crld-web -f\n'
printf '  Job logs:    %s/state/logs/*.log\n' "$STATE_DIR"
printf '  Caddy log:   /var/log/caddy/ (if configured)\n\n'
