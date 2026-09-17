#!/usr/bin/env bash
# Deploy drop-in forms to the Hetzner konsoleH account.
#
# This file is committed; the server's identity is not. Host, account, port and
# paths come from secrets.env (gitignored) or from the environment, which is
# what the CI runner supplies. See secrets.env.example.
#
# Usage:
#   deploy/deploy.sh probe        what does this server support? (read-only)
#   deploy/deploy.sh ports        are our loopback ports free? (read-only)
#   deploy/deploy.sh install      directories, .htaccess, cron. Safe to re-run,
#                                 and required after changing a port.
#   deploy/deploy.sh              build, upload, restart, health-check
#   deploy/deploy.sh --skip-tests skip `make test` (do not make a habit of it)
#   deploy/deploy.sh restart      restart without shipping a new binary
#   deploy/deploy.sh backup       back up the database, app left running
#   deploy/deploy.sh rollback     put the previous binary back
#   deploy/deploy.sh status       is it up?
#   deploy/deploy.sh logs [n]     tail the server log
#
# THE ORDER OF A DEPLOY IS THE POINT OF THIS SCRIPT. Everything destructive
# happens after the thing that could fail cheaply, the database is copied only
# while the writer is stopped, the binary is swapped by rename rather than
# overwrite, and a failed health check on the loopback rolls back. Each of
# those is commented where it happens.
#
# One inherited rule is repeated here because it reads as a bug and is not:
# only a failed *loopback* check rolls a binary back. A public check cannot
# tell a bad binary from a misconfigured web server, and in the sibling project
# it reverted a perfectly good release while Apache was answering 403 and the
# app was healthy.
set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
readonly APP=dropin-forms

case "${1:-}" in
-h | --help | help)
	awk '/^# Usage:/{f=1} f && !/^#/{exit} f{sub(/^# ?/, ""); print}' "${BASH_SOURCE[0]}"
	exit 0
	;;
esac

log() { printf '\033[1m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!!\033[0m %s\n' "$*" >&2; }
die() {
	printf '\033[31mxx\033[0m %s\n' "$*" >&2
	exit 1
}
require() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

# --- configuration ----------------------------------------------------------

if [ -f "$REPO_DIR/secrets.env" ]; then
	set -a
	# shellcheck disable=SC1091
	. "$REPO_DIR/secrets.env"
	set +a
fi

: "${DEPLOY_SSH_HOST:?set DEPLOY_SSH_HOST, in secrets.env or the environment}"
: "${DEPLOY_SSH_USER:?set DEPLOY_SSH_USER, in secrets.env or the environment}"
DEPLOY_SSH_PORT="${DEPLOY_SSH_PORT:-22}"
APP_DIR="${APP_DIR:-$APP}"
EMBED_PORT="${EMBED_PORT:?set EMBED_PORT}"
ADMIN_PORT="${ADMIN_PORT:?set ADMIN_PORT}"
EMBED_HOST="${EMBED_HOST:?set EMBED_HOST}"
ADMIN_HOST="${ADMIN_HOST:?set ADMIN_HOST}"
EMBED_DOCROOT="${EMBED_DOCROOT:?set EMBED_DOCROOT}"
ADMIN_DOCROOT="${ADMIN_DOCROOT:?set ADMIN_DOCROOT}"
KEEP_BACKUPS="${KEEP_BACKUPS:-14}"

# A leading ~ in an unquoted assignment is expanded by the *local* shell, so
# the value silently becomes this machine's home directory and is sent to the
# server as an absolute path that means nothing there. The symptom is mkdir
# failing on a path under /home/<your-username>.
case "$APP_DIR$EMBED_DOCROOT$ADMIN_DOCROOT" in
/home/* | /Users/*) die "a path in secrets.env was tilde-expanded locally; write paths relative to the remote home, with no leading ~" ;;
esac

# In CI the key arrives as an environment variable rather than as a file on
# disk. There is no StrictHostKeyChecking=no anywhere in this script: the host
# key is pinned from secrets.env, so a changed host key stops the deploy
# instead of being accepted silently.
SSH_OPTS=()
if [ -n "${DEPLOY_SSH_KEY_B64:-}" ] && [ -n "${CI:-}" ]; then
	keydir=$(mktemp -d)
	trap 'rm -rf "$keydir"' EXIT
	printf '%s' "$DEPLOY_SSH_KEY_B64" | base64 -d >"$keydir/id"
	printf '%s' "${DEPLOY_KNOWN_HOSTS_B64:?set DEPLOY_KNOWN_HOSTS_B64}" | base64 -d >"$keydir/known_hosts"
	chmod 600 "$keydir/id" "$keydir/known_hosts"
	SSH_OPTS=(-i "$keydir/id" -o "UserKnownHostsFile=$keydir/known_hosts" -o IdentitiesOnly=yes -o BatchMode=yes)
fi

remote() { ssh "${SSH_OPTS[@]}" -p "$DEPLOY_SSH_PORT" "$DEPLOY_SSH_USER@$DEPLOY_SSH_HOST" "$@"; }
remote_script() { ssh "${SSH_OPTS[@]}" -p "$DEPLOY_SSH_PORT" "$DEPLOY_SSH_USER@$DEPLOY_SSH_HOST" bash -s; }
remote_in_app() { remote "cd '$APP_DIR' && $*"; }
push() { scp -q "${SSH_OPTS[@]}" -P "$DEPLOY_SSH_PORT" "$1" "$DEPLOY_SSH_USER@$DEPLOY_SSH_HOST:$2"; }

# --- read-only checks -------------------------------------------------------

cmd_probe() {
	log "what this server offers"
	remote_script <<'EOF'
set -u
printf 'uname          %s\n' "$(uname -srm)"
printf 'home           %s\n' "$HOME"
printf 'shell          %s\n' "$SHELL"
for c in flock setsid sqlite3 curl crontab systemctl loginctl; do
  printf '%-14s %s\n' "$c" "$(command -v "$c" || echo MISSING)"
done
printf 'user dbus      %s\n' "${DBUS_SESSION_BUS_ADDRESS:-absent}"
printf 'systemctl user %s\n' "$(systemctl --user is-system-running 2>&1 | head -1)"
printf 'crontab        %s\n' "$(crontab -l 2>/dev/null | grep -c . || echo 0) line(s)"
EOF
}

# cmd_ports answers "is our pair free, and if not, what is?".
#
# This account is shared, so a loopback port is not ours to assume. The check
# is bash's own /dev/tcp rather than ss or netstat: a successful connect proves
# something is listening, and it needs no tool that might be missing. ss output
# is printed too when it exists, because "in use" is a worse answer than "in
# use by this".
cmd_ports() {
	log "loopback listeners, and whether our two ports are free"
	remote_script <<EOF
set -u

if command -v ss >/dev/null 2>&1; then
  echo "--- listening on the loopback ---"
  ss -ltnp 2>/dev/null | awk 'NR==1 || \$4 ~ /^127\.0\.0\.1:/ || \$4 ~ /^\[::1\]:/'
  echo
fi

for p in $EMBED_PORT $ADMIN_PORT; do
  if (exec 3<>/dev/tcp/127.0.0.1/\$p) 2>/dev/null; then
    echo "port \$p: IN USE"
  else
    echo "port \$p: free"
  fi
done

echo
printf 'free ports between 8400 and 8500: '
n=0
for p in \$(seq 8400 8500); do
  if ! (exec 3<>/dev/tcp/127.0.0.1/\$p) 2>/dev/null; then
    printf '%s ' "\$p"
    n=\$(( n + 1 ))
    [ "\$n" -ge 12 ] && break
  fi
done
echo
EOF
}

# assert_not_exposed refuses to deploy if the document root is serving the
# application directory. It is a string of live HTTPS requests rather than a
# permission check, because what matters is what a stranger can fetch, not what
# the file mode says.
assert_not_exposed() {
	local host path url code bad=0

	for host in "$EMBED_HOST" "$ADMIN_HOST"; do
		for path in config.toml "$APP.db" run.sh supervise.sh secrets.env; do
			url="https://$host/$path"
			code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 "$url" 2>/dev/null || echo 000)
			if [ "$code" = "200" ]; then
				warn "$url answers 200 -- the document root is serving the application directory"
				bad=1
			fi
		done
	done

	[ "$bad" -eq 0 ] || die "refusing to deploy while application files are publicly readable"
}

# assert_front_end_matches catches a .htaccess pointing at the wrong port.
#
# The port lives in two places -- config.toml, written by `make
# secrets-install`, and the .htaccess of each document root, written by
# `deploy/deploy.sh install` -- so changing it in secrets.env and running only
# the first leaves Apache proxying to a socket nobody is listening on. The
# symptom is the confusing one: the app is healthy on the loopback and the
# public URL answers 502, which reads like the Apache problem it technically is
# and sends you looking at document roots.
assert_front_end_matches() {
	check_htaccess_port "$EMBED_DOCROOT" "$EMBED_PORT" embed
	check_htaccess_port "$ADMIN_DOCROOT" "$ADMIN_PORT" admin
}

check_htaccess_port() {
	local docroot=$1 want=$2 name=$3 got

	got=$(remote "grep -oE '127\.0\.0\.1:[0-9]+' '$docroot/.htaccess' 2>/dev/null | head -1 | cut -d: -f2" || true)

	if [ -z "$got" ]; then
		die "there is no readable .htaccess at $docroot -- run: deploy/deploy.sh install"
	fi

	if [ "$got" != "$want" ]; then
		die "the $name .htaccess proxies to port $got but secrets.env says $want -- run: deploy/deploy.sh install"
	fi
}

# --- one-time setup ---------------------------------------------------------

cmd_install() {
	require ssh
	require scp

	log "creating directories and installing the front end"

	# The document root must exist before it appears in konsoleH's docroot
	# picker, which is why this runs before anybody clicks anything.
	remote_script <<EOF
set -euo pipefail
mkdir -p '$APP_DIR' '$APP_DIR/backups' '$EMBED_DOCROOT' '$ADMIN_DOCROOT'

# 711, not 700. Apache is not this account's user, so it needs to traverse the
# application directory to reach the document root inside it -- and 700 answers
# 403 on every URL, including ones that do not exist, which is a memorable
# afternoon.
chmod 711 '$APP_DIR'
chmod 755 '$EMBED_DOCROOT' '$ADMIN_DOCROOT'
chmod 700 '$APP_DIR/backups'
EOF

	local tmp
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' RETURN

	sed "s/__APP_PORT__/$EMBED_PORT/" "$SCRIPT_DIR/htaccess.template" >"$tmp/embed.htaccess"
	sed "s/__APP_PORT__/$ADMIN_PORT/" "$SCRIPT_DIR/htaccess.template" >"$tmp/admin.htaccess"

	push "$tmp/embed.htaccess" "$EMBED_DOCROOT/.htaccess"
	push "$tmp/admin.htaccess" "$ADMIN_DOCROOT/.htaccess"

	# 644. Apache must be able to read it, and a mode it cannot read produces a
	# body saying "unable to read htaccess file" rather than anything useful.
	remote "chmod 644 '$EMBED_DOCROOT/.htaccess' '$ADMIN_DOCROOT/.htaccess'"

	push "$SCRIPT_DIR/run.sh" "$APP_DIR/run.sh"
	push "$SCRIPT_DIR/supervise.sh" "$APP_DIR/supervise.sh"
	remote "chmod 700 '$APP_DIR/run.sh' '$APP_DIR/supervise.sh'"

	install_cron

	log "done. Next: point each konsoleH document root at"
	log "    $EMBED_DOCROOT   (for $EMBED_HOST)"
	log "    $ADMIN_DOCROOT   (for $ADMIN_HOST)"
	log "then run: make secrets-install && deploy/deploy.sh"
}

# install_cron adds our two lines and must not disturb anybody else's.
#
# This account is shared. The probe found 55 crontab lines already, belonging to
# the other sites on it, and `crontab <file>` replaces the whole table rather
# than appending to it -- so a mistake here does not break this service, it
# silently stops somebody else's backups. Hence: the existing table is saved to
# a timestamped file in the app directory first, the line count is asserted
# before the new table is installed, and the counts are printed either way.
install_cron() {
	log "installing the @reboot launcher and the five-minute watchdog"

	remote_script <<EOF
set -euo pipefail
marker='# dropin-forms (managed by deploy.sh)'
mkdir -p '$APP_DIR'

# Absolute, resolved on the server. APP_DIR is relative to the remote home by
# design, and cron on Debian happens to run jobs with the working directory set
# to $HOME, so a relative path works here -- by coincidence rather than by
# contract. The failure it would cause is the silent kind: the watchdog never
# fires and nobody finds out until the app fails to come back from a reboot.
case '$APP_DIR' in
/*) app_abs='$APP_DIR' ;;
*)  app_abs="\$HOME/$APP_DIR" ;;
esac
[ -x "\$app_abs/supervise.sh" ] || { echo "crontab: \$app_abs/supervise.sh is not executable; refusing to install a cron line that cannot run" >&2; exit 1; }

stamp=\$(date -u +%Y%m%dT%H%M%SZ)
backup="$APP_DIR/crontab-\$stamp.txt"
crontab -l >"\$backup" 2>/dev/null || : >"\$backup"
chmod 600 "\$backup"

before=\$(grep -c . "\$backup" || true)
theirs=\$(grep -v "\$marker" "\$backup" | grep -c . || true)
echo "crontab: \$before line(s) now, \$theirs of them not ours; saved to \$backup"

tmp=\$(mktemp)
grep -v "\$marker" "\$backup" >"\$tmp" || true
{
  echo "\$marker"
  echo "@reboot \$app_abs/supervise.sh start  \$marker"
  echo "*/5 * * * * \$app_abs/supervise.sh start  \$marker"
} >>"\$tmp"

# Refuse rather than install a table that lost somebody else's lines. The
# arithmetic is exact: everything that was not ours, plus our three.
want=\$(( theirs + 3 ))
got=\$(grep -c . "\$tmp" || true)
if [ "\$got" -ne "\$want" ]; then
  echo "crontab: refusing to install \$got lines when \$want were expected; restore from \$backup by hand" >&2
  rm -f "\$tmp"
  exit 1
fi

crontab "\$tmp"
rm -f "\$tmp"

after=\$(crontab -l 2>/dev/null | grep -c . || true)
echo "crontab: \$after line(s) installed"
crontab -l | grep dropin-forms
EOF
}

# --- the deploy -------------------------------------------------------------

report_source() {
	local branch head dirty behind
	branch=$(git -C "$REPO_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')
	head=$(git -C "$REPO_DIR" rev-parse --short HEAD 2>/dev/null || echo '?')
	log "building from $branch @ $head"

	dirty=$(git -C "$REPO_DIR" status --porcelain 2>/dev/null | grep -cv '^??' || true)
	if [ "${dirty:-0}" -gt 0 ]; then
		warn "$dirty uncommitted change(s) in tracked files will be included in this build"
	fi

	git -C "$REPO_DIR" fetch -q origin 2>/dev/null || true
	behind=$(git -C "$REPO_DIR" rev-list --count HEAD..origin/main 2>/dev/null || echo 0)
	if [ "${behind:-0}" -gt 0 ]; then
		warn "this checkout is $behind commit(s) behind origin/main"
	fi
}

health_loopback() {
	local port name i
	port=$1
	name=$2

	for i in $(seq 1 20); do
		if remote "curl -fsS --max-time 5 http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
			log "$name is healthy on the loopback"
			return 0
		fi
		sleep 1.5
	done

	return 1
}

health_public() {
	local host i
	host=$1

	for i in $(seq 1 10); do
		if curl -fsS --max-time 10 "https://$host/healthz" >/dev/null 2>&1; then
			log "https://$host/healthz answers"
			return 0
		fi
		sleep 1.5
	done

	return 1
}

cmd_deploy() {
	local skip_tests="${1:-no}"

	require go
	require ssh
	require scp
	require curl

	report_source

	log "checking the document root is not serving the application directory"
	assert_not_exposed

	log "checking each .htaccess proxies to the port the config uses"
	assert_front_end_matches

	if [ "$skip_tests" = no ]; then
		log "make test"
		(cd "$REPO_DIR" && make test)
	else
		warn "skipping tests"
	fi

	log "building a static linux/amd64 binary"
	# CGO off on purpose: the SQLite driver is pure Go, so the result is one
	# file with no libc on the server to match against. An ordinary build links
	# the builder's glibc and dies there with GLIBC_2.xx not found.
	(cd "$REPO_DIR" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath -ldflags='-s -w' -o "$SCRIPT_DIR/.$APP-linux" ./cmd/$APP)

	log "uploading ($(du -h "$SCRIPT_DIR/.$APP-linux" | cut -f1))"
	push "$SCRIPT_DIR/.$APP-linux" "$APP_DIR/$APP.new"
	rm -f "$SCRIPT_DIR/.$APP-linux"

	# These may have changed alongside the binary; keep them in step.
	push "$SCRIPT_DIR/run.sh" "$APP_DIR/run.sh"
	push "$SCRIPT_DIR/supervise.sh" "$APP_DIR/supervise.sh"

	log "stopping, backing up, swapping the binary, starting"
	local swapped=yes
	remote_script <<EOF || swapped=no
set -euo pipefail
cd '$APP_DIR'

# Checked BEFORE anything is stopped. Everything below is destructive in order,
# so a missing or truncated upload has to fail while the service is still
# happily running rather than after it is down with no binary to run.
if [ ! -s '$APP.new' ]; then
  echo "deploy: $APP.new is missing or empty; refusing to touch the running app" >&2
  exit 1
fi
chmod 700 '$APP.new' run.sh supervise.sh

[ -f config.toml ] || { echo "deploy: config.toml is missing; run make secrets-install first" >&2; exit 1; }

./supervise.sh stop || true

# Our own app is down now, so anything still holding these ports belongs to
# another site on this shared account. Checked HERE, between the stop and the
# first destructive step, because the alternative is what actually happened on
# the first deploy: the binary was swapped, the start failed on a port clash,
# and the rollback had no previous binary to put back. bash's /dev/tcp is used
# rather than ss so the check cannot fail for want of a tool.
for p in $EMBED_PORT $ADMIN_PORT; do
  if (exec 3<>/dev/tcp/127.0.0.1/\$p) 2>/dev/null; then
    echo "deploy: port \$p is still in use with our app stopped, so something else on this account is listening there." >&2
    echo "deploy: run 'deploy/deploy.sh ports' to see what is free, then change EMBED_PORT/ADMIN_PORT in secrets.env and re-run 'make secrets-install'." >&2
    echo "deploy: refusing to swap the binary." >&2
    exit 1
  fi
done

# With the writer stopped a plain copy cannot be torn, which is exactly why the
# backup happens here and not while the app is live.
if [ -f '$APP.db' ]; then
  mkdir -p backups
  stamp=\$(date -u +%Y%m%dT%H%M%SZ)
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 '$APP.db' ".backup 'backups/$APP-\$stamp.db'"
  else
    cp '$APP.db' "backups/$APP-\$stamp.db"
    [ -f '$APP.db-wal' ] && cp '$APP.db-wal' "backups/$APP-\$stamp.db-wal" || true
  fi
  ls -1t backups/$APP-*.db 2>/dev/null | tail -n +\$(( $KEEP_BACKUPS + 1 )) | xargs -r rm -f || true
  echo "backed up to backups/$APP-\$stamp.db"
fi

# A rename, not a write in place: replacing a running executable's bytes gives
# ETXTBSY, swapping the inode does not. The outgoing binary is kept so a failed
# health check can put it back.
[ -f '$APP' ] && mv -f '$APP' '$APP.prev' || true
mv -f '$APP.new' '$APP'

./supervise.sh start
EOF

	local app_ok=yes
	if [ "$swapped" = no ]; then
		warn "the remote restart failed"
		app_ok=no
	else
		health_loopback "$EMBED_PORT" embed || app_ok=no
		health_loopback "$ADMIN_PORT" admin || app_ok=no
	fi

	if [ "$app_ok" = no ]; then
		warn "the app did not become healthy on the loopback -- rolling back"
		do_rollback
		remote_in_app "tail -n 40 $APP.log" || true
		die "deploy failed and was rolled back"
	fi

	remote_in_app "printf '%s\\n' '$(git -C "$REPO_DIR" rev-parse HEAD 2>/dev/null || echo unknown)' > deployed-commit.txt" || true

	local public_ok=yes
	health_public "$EMBED_HOST" || public_ok=no
	health_public "$ADMIN_HOST" || public_ok=no

	if [ "$public_ok" = no ]; then
		# The release is KEPT. The binary is fine; the web front end is not,
		# and reverting a good release over an Apache problem discards the
		# release and hides the real cause.
		warn "the app is healthy on the loopback but a public URL does not answer."
		warn "the new binary has been KEPT -- this is an Apache problem, not a bad build."
		warn "check, in this order:"
		warn "  1. the konsoleH document root for each host:"
		warn "       $EMBED_HOST -> $EMBED_DOCROOT"
		warn "       $ADMIN_HOST -> $ADMIN_DOCROOT"
		warn "  2. each .htaccess exists and is mode 644"
		warn "     (a body saying \"unable to read htaccess file\" means exactly this)"
		warn "  3. $APP_DIR is mode 711"
		warn "     (403 on every path, including ones that do not exist, means exactly this)"
		exit 1
	fi

	remote_in_app "rm -f $APP.prev" || true
	log "deployed"
}

do_rollback() {
	remote_script <<EOF
set -euo pipefail
cd '$APP_DIR'
./supervise.sh stop || true
if [ -f '$APP.prev' ]; then
  mv -f '$APP' '$APP.failed'
  mv -f '$APP.prev' '$APP'
  echo "restored the previous binary (the failed one is kept as $APP.failed)"
else
  echo "no previous binary to restore" >&2
fi
./supervise.sh start || true
EOF
}

# --- the small ones ---------------------------------------------------------

cmd_restart() {
	remote_in_app "./supervise.sh restart"
	health_loopback "$EMBED_PORT" embed || die "embed did not come back"
	health_loopback "$ADMIN_PORT" admin || die "admin did not come back"
}

cmd_backup() {
	log "backing up the database, leaving the app running"
	remote_script <<EOF
set -euo pipefail
cd '$APP_DIR'
[ -f '$APP.db' ] || { echo "no database yet, nothing to back up"; exit 0; }
mkdir -p backups
stamp=\$(date -u +%Y%m%dT%H%M%SZ)

# sqlite3 .backup is safe against a live writer; plain cp is not, because it
# can capture a torn page set while WAL is mid-transaction.
if command -v sqlite3 >/dev/null 2>&1; then
  sqlite3 '$APP.db' ".backup 'backups/$APP-\$stamp.db'"
  echo "backups/$APP-\$stamp.db (sqlite3 .backup)"
else
  cp '$APP.db' "backups/$APP-\$stamp.db"
  [ -f '$APP.db-wal' ] && cp '$APP.db-wal' "backups/$APP-\$stamp.db-wal" || true
  echo "backups/$APP-\$stamp.db (cp -- no sqlite3 on this host)"
fi

ls -1t backups/$APP-*.db 2>/dev/null | tail -n +\$(( $KEEP_BACKUPS + 1 )) | xargs -r rm -f || true
ls -1t backups/ | head -3
EOF
}

cmd_status() {
	remote_in_app "./supervise.sh status" || true
	echo
	local host
	for host in "$EMBED_HOST" "$ADMIN_HOST"; do
		printf '%-28s ' "https://$host/healthz"
		curl -sS -o /dev/null -w '%{http_code}\n' --max-time 15 "https://$host/healthz" || echo "no answer"
	done
}

cmd_logs() { remote_in_app "tail -n ${1:-80} -f $APP.log"; }

case "${1:-deploy}" in
probe) cmd_probe ;;
ports) cmd_ports ;;
install) cmd_install ;;
deploy) cmd_deploy no ;;
--skip-tests) cmd_deploy yes ;;
restart) cmd_restart ;;
rollback) do_rollback ;;
backup) cmd_backup ;;
status) cmd_status ;;
logs) cmd_logs "${2:-80}" ;;
*) die "unknown command '$1' (try: probe, ports, install, deploy, restart, rollback, backup, status, logs)" ;;
esac
