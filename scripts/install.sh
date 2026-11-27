#!/usr/bin/env bash
# Crypta server installer.
#
#   curl -fsSL https://raw.githubusercontent.com/cestbon0309/Crypta/main/scripts/install.sh | bash
#
# While the repository is private, pass a GitHub token that can read it:
#
#   curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" \
#     https://raw.githubusercontent.com/cestbon0309/Crypta/main/scripts/install.sh | GITHUB_TOKEN=$GITHUB_TOKEN bash
#
# Answers can be given up front instead of typed:
#   CRYPTA_DIR              where to install (default ~/crypta)
#   CRYPTA_DOMAIN           domain pointing at this server; enables automatic HTTPS
#   RCLONE_CONF             path to rclone.conf; empty = demo mode without Google Drive
#   CRYPTA_REMOTE           name of the crypt remote, if the file has several
#   TELEGRAM_BOT_TOKEN      enables the Telegram plugin
#   TELEGRAM_ALLOWED_USERS  comma separated numeric Telegram user ids
#   CRYPTA_YES=1            never ask, use the values above
set -euo pipefail

REPO=${CRYPTA_REPO:-cestbon0309/Crypta}
URL=${CRYPTA_REPO_URL:-https://github.com/$REPO.git}
REF=${CRYPTA_REF:-main}
DIR=${CRYPTA_DIR:-$HOME/crypta}

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# The script itself arrives on stdin, so questions are read from the terminal.
ask() { # ask VAR "question" [default]
	local var=$1 q=$2 def=${3:-} ans
	if [ -n "${!var+x}" ]; then return; fi
	if [ "${CRYPTA_YES:-}" = 1 ] || ! { : </dev/tty; } 2>/dev/null; then
		printf -v "$var" '%s' "$def"
		return
	fi
	read -r -p "$q${def:+ [$def]}: " ans </dev/tty || ans=
	printf -v "$var" '%s' "${ans:-$def}"
}

SUDO=
if [ "$(id -u)" != 0 ]; then
	command -v sudo >/dev/null && SUDO=sudo
fi

command -v git >/dev/null || die "git is required; install it and run this again"

if ! command -v docker >/dev/null; then
	[ "$(uname -s)" = Linux ] || die "install Docker Desktop (or another Docker engine) first, then run this again"
	say "Installing Docker"
	curl -fsSL https://get.docker.com | $SUDO sh
fi
DOCKER=docker
docker info >/dev/null 2>&1 || DOCKER="$SUDO docker"
$DOCKER info >/dev/null 2>&1 || die "Docker is installed but not running"
$DOCKER compose version >/dev/null 2>&1 || die "the Docker Compose plugin is missing (package docker-compose-plugin)"

say "Fetching Crypta into $DIR"
# The token is passed per command so it is never written to .git/config.
git_auth() {
	if [ -n "${GITHUB_TOKEN:-}" ]; then
		git -c "http.https://github.com/.extraheader=Authorization: Basic $(printf 'x-access-token:%s' "$GITHUB_TOKEN" | base64 | tr -d '\n')" "$@"
	else
		git "$@"
	fi
}
if [ -d "$DIR/.git" ]; then
	git_auth -C "$DIR" fetch --depth 1 origin "$REF"
	git -C "$DIR" checkout -q -B "$REF" FETCH_HEAD
else
	git_auth clone --depth 1 --branch "$REF" "$URL" "$DIR" ||
		die "could not download $REPO; for a private repository set GITHUB_TOKEN"
fi
cd "$DIR"

if [ -f .env ]; then
	say "Keeping the existing $DIR/.env"
else
	say "Configuring"
	ask CRYPTA_DOMAIN "Domain pointing at this server (empty: no HTTPS, local network only)"
	ask RCLONE_CONF "Path to rclone.conf with your crypt remote (empty: demo mode without Google Drive)"
	ask TELEGRAM_BOT_TOKEN "Telegram bot token (empty: no Telegram plugin)"
	if [ -n "$TELEGRAM_BOT_TOKEN" ]; then
		ask TELEGRAM_ALLOWED_USERS "Telegram user ids allowed to use the bot, comma separated"
	fi

	demo=
	if [ -n "$RCLONE_CONF" ]; then
		RCLONE_CONF=${RCLONE_CONF/#\~/$HOME}
		[ -f "$RCLONE_CONF" ] || die "$RCLONE_CONF does not exist"
		install -m 600 "$RCLONE_CONF" rclone.conf
	else
		demo=1
		: >rclone.conf
	fi

	if [ -n "$CRYPTA_DOMAIN" ]; then
		url=https://$CRYPTA_DOMAIN
		bind=127.0.0.1:8080
	else
		ip=$(hostname -I 2>/dev/null | awk '{print $1}') || ip=
		[ -n "$ip" ] || ip=$(ipconfig getifaddr en0 2>/dev/null) || ip=
		url=http://${ip:-127.0.0.1}:8080
		bind=0.0.0.0:8080
	fi

	umask 077
	cat >.env <<ENV
CRYPTA_PUBLIC_URL=$url
CRYPTA_DOMAIN=$CRYPTA_DOMAIN
RCLONE_CONF=./rclone.conf
CRYPTA_REMOTE=${CRYPTA_REMOTE:-}
CRYPTA_DEMO=$demo
CRYPTA_BIND=$bind
TELEGRAM_BOT_TOKEN=$TELEGRAM_BOT_TOKEN
TELEGRAM_ALLOWED_USERS=${TELEGRAM_ALLOWED_USERS:-}
TELEGRAM_REMOTE_DIR=Telegram
TELEGRAM_API_BASE=
ENV
fi

val() { sed -n "s/^$1=//p" .env | tail -1; }
profiles=()
[ -n "$(val CRYPTA_DOMAIN)" ] && profiles+=(--profile https)
[ -n "$(val TELEGRAM_BOT_TOKEN)" ] && profiles+=(--profile telegram)

say "Building and starting"
$DOCKER compose ${profiles[@]+"${profiles[@]}"} up -d --build

say "Waiting for the server"
for _ in $(seq 60); do
	if $DOCKER compose exec -T core wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
		ok=1
		break
	fi
	sleep 2
done
[ "${ok:-}" = 1 ] || die "the server did not start; see: cd $DIR && $DOCKER compose logs core"

say "Crypta is running at $(val CRYPTA_PUBLIC_URL)"
[ -n "$(val CRYPTA_DEMO)" ] && echo "Demo mode: files stay on this server. Put your rclone.conf in $DIR, clear CRYPTA_DEMO in .env and run this again to use Google Drive."
[ -z "$(val CRYPTA_DOMAIN)" ] && echo "No domain set: the app only pairs over plain http on the local network."
echo
$DOCKER compose exec -T core crypta pair
cat <<DONE

New pairing code:  cd $DIR && $DOCKER compose exec core crypta pair
Progress page:     $(val CRYPTA_PUBLIC_URL)  (token: $DOCKER compose exec core crypta token)
Update:            run this installer again
DONE
