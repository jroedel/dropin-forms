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
#   deploy/deploy.sh deploy       switch to main, pull it, build, upload,
#                                 restart, health-check
#   deploy/deploy.sh --skip-tests skip `make test` (do not make a habit of it)
#   deploy/deploy.sh restart      restart without shipping a new binary
#   deploy/deploy.sh backup       back up the database, app left running
#   deploy/deploy.sh rollback     put the previous binary back
#   deploy/deploy.sh status       is it up?
#   deploy/deploy.sh logs [n]     tail the server log
#
# `deploy` moves the checkout to main and fast-forwards it, because deploying
# means shipping main and there is no version of that where you wanted the
# branch you happened to be on. A dirty tree cancels the whole thing rather
# than being carried across. DEPLOY_ALLOW_BRANCH=1 deploys the current branch
# instead, for trying a fix on the server before merging it.
#
# deploy/ensure-main-test.sh exercises all of that in a throwaway repository.
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

# SELF and ARGV are how this script re-runs itself after pulling a newer copy
# of itself. See ensure_main.
readonly SELF="$SCRIPT_DIR/$(basename "${BASH_SOURCE[0]}")"
readonly ARGV=("$@")

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
	log "then run: deploy/deploy.sh deploy"
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

# hand_over_if_changed re-executes this script when the last git operation
# rewrote it.
#
# Called after anything that can move the checkout -- a branch switch or a
# fast-forward -- and it must be called *immediately* after, with nothing in
# between. Two hazards, and they are different:
#
#   - The rest of the deploy would otherwise run the previous procedure
#     against the new commit: old build flags, old pre-flight, old rollback.
#     That is not hypothetical. On 2026-09-17 a deploy from a checkout four
#     commits behind ran the version of this script that only warned about
#     being behind, built a month-old tree, and shipped a binary that could
#     not read the config it was given. The rollback worked, but the guard
#     that would have stopped it was sitting in the commits the deploy had not
#     pulled yet -- the one deploy a guard cannot protect is the deploy that
#     delivers it.
#   - bash reads a script incrementally rather than all at once, so rewriting
#     it mid-execution can make the shell run bytes from two different
#     versions of the same file. exec is what avoids that, and it only avoids
#     it if nothing has run in between.
hand_over_if_changed() {
	local was="$1" what="$2"

	[ "$(sha256sum "$SELF" | cut -d" " -f1)" = "$was" ] && return 0

	# A count, not a flag, and that distinction is a bug that reached a real
	# deploy. There are two points in ensure_main that can rewrite this file
	# -- the switch to main and the fast-forward -- and a deploy from a branch
	# whose local main is behind hits both: switching replaces the branch's
	# deploy.sh with main's older one, and the fast-forward then replaces it
	# again with the merged version. A flag cancelled that with "changed again
	# after re-running once", which was accurate and was not a loop.
	#
	# Bounded all the same, and tightly. One hand-over per mutation point
	# covers every legitimate case, so a third means origin is moving under
	# this deploy or something outside git is rewriting the file, and neither
	# is worth looping over.
	local handovers="${DEPLOY_SELF_UPDATED:-0}"

	# A value that is not a number can only come from the environment, and
	# comparing it numerically would abort with a bash error rather than a
	# sentence. Treated as one hand-over already spent, which fails safe.
	case "$handovers" in
	'' | *[!0-9]*) handovers=1 ;;
	esac

	if [ "$handovers" -ge 2 ]; then
		warn "deploy.sh has now changed $handovers times during one deploy."
		warn "either origin moved while this was running -- try again -- or"
		warn "something outside git is rewriting deploy/deploy.sh."
		die "cancelled: deploy.sh will not stop changing"
	fi

	log "$what changed deploy.sh; handing over to the new one"

	DEPLOY_SELF_UPDATED=$((handovers + 1)) exec bash "$SELF" "${ARGV[@]}"
}

# ensure_main brings the checkout to origin/main before anything is built.
#
# This used to be two warnings -- "uncommitted changes will be included" and
# "this checkout is N commits behind origin/main" -- and a warning during a
# deploy is read afterwards, while working out why production is running
# something nobody remembers merging. What actually goes to the server is
# whatever happens to be in the working tree, so that is the thing to pin down
# rather than to mention.
#
# It switches to main, fast-forwards it, and cancels if it cannot do either
# safely. It never discards anything: a dirty tree and a diverged history each
# cancel the deploy with the command to fix it, because the alternative is a
# deploy script that can lose somebody's work.
#
# # Why it switches rather than telling you to
#
# The first version of this refused when the checkout was not on main and
# printed `git switch main` for somebody to run. That is a worse tool than it
# looks. "Deploy" means "ship what is on main", every single time -- so the
# switch is not a decision being made, it is a step being announced, and a
# script that announces a step it could take is a script you type the same two
# commands into for the rest of the project. It came up three times in one
# afternoon before it was fixed.
#
# The branch that was current is named in the log, with the command to return
# to it, because the checkout is left on main afterwards and that should not
# be a surprise.
#
# The dirty check still comes first and still cancels the whole deploy, which
# is what makes the switch safe to do unattended: git would carry uncommitted
# changes across, or refuse halfway, and neither belongs inside a deploy.
#
# DEPLOY_ALLOW_BRANCH=1 is the escape hatch, for trying a fix on the server
# before merging it.
#
# # Why it re-executes itself
#
# Both the switch and the fast-forward can rewrite this very file, so each is
# followed immediately by hand_over_if_changed. The reasoning is on that
# function, and the "immediately" is load-bearing -- do not put anything
# between a git operation and its hand-over.
ensure_main() {
	require git

	# In CI the checkout is whatever commit was tagged, fetched by the
	# workflow. Moving it would deploy something other than the release that
	# was asked for, which is the opposite of the point.
	if [ -n "${CI:-}" ]; then
		log "CI: deploying the checked-out commit $(git -C "$REPO_DIR" rev-parse --short HEAD 2>/dev/null || echo '?')"
		return 0
	fi

	local branch dirty
	branch=$(git -C "$REPO_DIR" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')
	dirty=$(git -C "$REPO_DIR" status --porcelain 2>/dev/null | grep -cv '^??' || true)

	# A dirty tree cancels the whole thing, before the fetch and before
	# anything is asked of the server. Two reasons rather than one: a deploy
	# must ship something that exists in git, and a fast-forward into
	# uncommitted changes is how a deploy script eats somebody's work.
	if [ "${dirty:-0}" -gt 0 ]; then
		warn "$dirty uncommitted change(s) in tracked files:"
		git -C "$REPO_DIR" status --short 2>/dev/null | grep -v '^??' >&2 || true
		warn "commit or stash them, then deploy again."
		die "cancelled: the working tree has changes that are not in git"
	fi

	if [ "$branch" != main ]; then
		if [ "${DEPLOY_ALLOW_BRANCH:-}" = 1 ]; then
			# Deliberate, and sometimes necessary -- trying a fix on the
			# server before merging it. Loud, because the next person to
			# deploy main will silently replace it and wonder where it went.
			warn "DEPLOY_ALLOW_BRANCH=1: deploying '$branch' rather than main"
			log "building from $branch @ $(git -C "$REPO_DIR" rev-parse --short HEAD)"

			return 0
		fi

		# Captured before the switch, because switching branches can rewrite
		# this file -- which is in fact the common case, since the branch you
		# are on is usually the one that changed it.
		local was_before_switch
		was_before_switch=$(sha256sum "$SELF" | cut -d" " -f1)

		log "on '$branch'; switching to main, which is what production runs"

		# Plain `git switch main`, which also creates a local main from
		# origin/main when there is not one yet -- git does that itself when
		# exactly one remote has the branch. The tree is known clean here, so
		# this cannot carry changes across or stop halfway.
		# Output captured rather than discarded: -q keeps "Your branch is up
		# to date with 'origin/main'." out of a deploy log, and on a failure
		# git's own sentence is the only useful thing there is to show.
		local switch_said
		if ! switch_said=$(git -C "$REPO_DIR" switch -q main 2>&1); then
			warn "could not switch to main from '$branch'."
			[ -z "$switch_said" ] || warn "  git said: $switch_said"
			die "cancelled: could not get to main"
		fi

		# Named so that getting back is one command rather than a search
		# through the reflog. The checkout stays on main after this.
		log "you were on '$branch'; 'git switch $branch' returns to it"

		# Immediately. See hand_over_if_changed.
		hand_over_if_changed "$was_before_switch" "switching to main"
	fi

	log "fetching origin"
	git -C "$REPO_DIR" fetch -q origin main || die "cancelled: could not reach origin, and a checkout of unknown age is not deployable"

	# Both counts, because behind alone does not distinguish "four commits to
	# catch up on" from "diverged". Announcing a fast-forward and then
	# reporting that it was impossible is a log line that says the opposite of
	# what happened, which is exactly the sort of line somebody reads at speed
	# during an incident.
	local behind ahead
	behind=$(git -C "$REPO_DIR" rev-list --count HEAD..origin/main 2>/dev/null || echo 0)
	ahead=$(git -C "$REPO_DIR" rev-list --count origin/main..HEAD 2>/dev/null || echo 0)

	if [ "${behind:-0}" -gt 0 ] && [ "${ahead:-0}" -eq 0 ]; then
		log "fast-forwarding main $behind commit(s) to origin/main"
	fi

	# What this file looked like before the fast-forward.
	local was_before_merge
	was_before_merge=$(sha256sum "$SELF" | cut -d" " -f1)

	# --ff-only, so this can only ever move forward to what origin already
	# has. A diverged local main means somebody committed to it directly,
	# which the house rule forbids, and resolving that is not a deploy
	# script's business.
	if ! git -C "$REPO_DIR" merge --ff-only origin/main 2>/dev/null; then
		warn "local main has $ahead commit(s) origin/main does not, so it cannot be fast-forwarded."
		warn "nothing here commits to main directly -- everything lands through a pull request --"
		warn "so those commits want a branch and a PR rather than a deploy."
		die "cancelled: main has diverged from origin"
	fi

	# Immediately after the merge and nothing in between. See
	# hand_over_if_changed.
	hand_over_if_changed "$was_before_merge" "that update"

	log "building from main @ $(git -C "$REPO_DIR" rev-parse --short HEAD)"
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

	# First, before the build and before anything is asked of the server:
	# what goes out has to be a commit that exists on origin/main. This also
	# logs which commit that is, which is the other half of the record the
	# server keeps after a successful swap.
	ensure_main

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

	# The configuration goes out with the binary that reads it, when we are
	# somewhere that can render one.
	#
	# This is the fix for a real failure rather than tidiness. The binary and
	# its config used to travel separately -- the binary through CD, the config
	# through `make secrets-install` -- so a change to the config's *shape*
	# left the two disagreeing, in whichever order you did them: a new binary
	# requires settings an old config lacks, and an old binary refuses settings
	# a new config carries. Either way the running process is unaffected,
	# because it read its config at startup, and the failure waits until
	# something restarts it. Then it will not start at all.
	#
	# CI cannot do this: it holds only the deploy key, never the Stripe or SMTP
	# credentials, which is deliberate and worth keeping. So there, the config
	# already on the server is checked against the incoming binary instead, and
	# a mismatch refuses the deploy rather than discovering it later.
	local shipping_config=no
	if [ -r "$REPO_DIR/secrets.env" ]; then
		log "rendering config.toml and sending it with the binary"

		# Through a shell on the far side, never a local temporary file, so
		# the rendered secrets touch neither disk.
		if "$REPO_DIR/scripts/secrets" render |
			ssh "${SSH_OPTS[@]}" -p "$DEPLOY_SSH_PORT" "$DEPLOY_SSH_USER@$DEPLOY_SSH_HOST" \
				"cat > '$APP_DIR/config.toml.new' && chmod 600 '$APP_DIR/config.toml.new'"; then
			shipping_config=yes
		else
			die "the config could not be rendered or sent; nothing has been changed"
		fi
	else
		warn "no secrets.env here, so the config on the server is left as it is"
		warn "(expected in CI, which deliberately holds no Stripe or SMTP credentials)"
	fi

	# The pre-flight. Run while the outgoing binary is still serving and
	# nothing has been swapped, so a refusal here costs nothing.
	log "asking the new binary whether it accepts the config it will run with"

	local config_to_check=config.toml
	if [ "$shipping_config" = yes ]; then
		config_to_check=config.toml.new
	fi

	if ! remote_script <<EOF
set -euo pipefail
cd '$APP_DIR'

if [ ! -s '$APP.new' ]; then
  echo "deploy: $APP.new is missing or empty" >&2
  exit 1
fi
if [ ! -f '$config_to_check' ]; then
  echo "deploy: there is no $config_to_check on the server." >&2
  echo "deploy: run this from a machine with secrets.env so the config ships with the binary." >&2
  exit 1
fi

chmod 700 '$APP.new'
./$APP.new -config '$config_to_check' -check
EOF
	then
		remote_in_app "rm -f config.toml.new" || true
		warn "the new binary will not run with that config, so nothing has been swapped."
		warn "the service is still running on the old binary."
		if [ "$shipping_config" = no ]; then
			warn "this deploy shipped no config. Run 'deploy/deploy.sh deploy' from a machine"
			warn "with secrets.env, so the binary and its config go out together."
		fi
		die "deploy refused before touching anything"
	fi

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

# Both were already checked together by the pre-flight above, while the old
# binary was still serving. Re-checked cheaply here because everything below
# this line is destructive in order.
if [ '$shipping_config' = yes ] && [ ! -s config.toml.new ]; then
  echo "deploy: config.toml.new vanished between the check and the swap" >&2
  exit 1
fi

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

# The config moves in the same window, keeping the outgoing one, so that a
# rollback restores the pair that was known to work together. Rolling back
# only the binary would leave it reading a config written for its successor.
if [ '$shipping_config' = yes ]; then
  [ -f config.toml ] && cp -p config.toml config.toml.prev || true
  mv -f config.toml.new config.toml
fi

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

	remote_in_app "rm -f $APP.prev config.toml.prev" || true
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

# The config goes back with it. Restoring one without the other is how a
# rollback produces a second, different failure: the previous binary reading
# settings written for its successor, which it refuses outright.
if [ -f config.toml.prev ]; then
  mv -f config.toml config.toml.failed
  mv -f config.toml.prev config.toml
  echo "restored the previous config.toml (the failed one is kept as config.toml.failed)"
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

# No default subcommand, deliberately.
#
# This used to default to `deploy`, so `deploy/deploy.sh` with no arguments
# shipped to production. That is the wrong default for the same reason CD is
# triggered by a tag rather than by every push: a deploy should be something
# somebody asked for. It also makes the script safe to run while finding out
# what it does, which is exactly when nobody wants it to deploy.
usage() {
	cat >&2 <<'USAGE'
usage: deploy/deploy.sh <command>

read-only:
  probe       what this server offers
  ports       which loopback ports are free
  status      is it running, and do the public URLs answer
  logs [n]    tail the application log

changes the server:
  install     one-time: directories, .htaccess, cron
  deploy      build, ship the binary and its config, health-check, roll back on failure
  restart     stop and start, no new build
  rollback    put the previous binary and config back
  backup      back up the database, leaving the app running

There is no default command. `deploy` is the one that touches production.
USAGE
	exit 2
}

case "${1:-}" in
"") usage ;;
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
*)
	printf '\033[31mxx\033[0m unknown command %s\n\n' "'$1'" >&2
	usage
	;;
esac
