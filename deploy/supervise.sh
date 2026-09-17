#!/usr/bin/env bash
# Keep the app running, without systemd.
#
# systemd is running on this host, and both systemctl and loginctl are on the
# path -- but there is no user D-Bus, so `systemctl --user` answers "Failed to
# connect to bus: No medium found" and lingering cannot be enabled either. The
# binaries being present is what makes this worth stating: the obvious check,
# `command -v systemctl`, says yes and tells you nothing. So supervision is
# this script plus two crontab lines:
#
#   @reboot     <app dir>/supervise.sh start
#   */5 * * * * <app dir>/supervise.sh start
#
# `start` is idempotent, which is what lets one command be both the boot
# launcher and the five-minute watchdog.
#
# Three details below each cost somebody an evening in the project this was
# carried from, and each is commented where it happens rather than here.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

readonly APP=dropin-forms
readonly PIDFILE="$APP.pid"
readonly LOGFILE="$APP.log"
readonly LOCKFILE=supervise.lock

log() { printf '%s supervise: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

# running checks two things, because a pid file alone is a lie waiting to
# happen. kill -0 says something holds that pid; the cmdline check says it is
# *us* and not whatever the operating system handed the number to after our
# process died.
running() {
	[ -f "$PIDFILE" ] || return 1

	local pid
	pid=$(cat "$PIDFILE" 2>/dev/null) || return 1
	[ -n "$pid" ] || return 1

	kill -0 "$pid" 2>/dev/null || return 1
	grep -qa "$APP" "/proc/$pid/cmdline" 2>/dev/null || return 1
}

start() {
	# Serialise against the watchdog firing while a deploy is mid-restart.
	exec 9>"$LOCKFILE"
	if ! flock -w 30 9; then
		log "another supervise holds the lock; giving up"
		exit 1
	fi

	if running; then
		return 0
	fi

	[ -x "./$APP" ] || {
		log "./$APP is missing or not executable"
		exit 1
	}

	# </dev/null or the ssh session that invoked this never returns, and the
	# deploy hangs with no output. 9>&- closes the inherited flock descriptor:
	# leave it open and the child holds the lock forever, so the next `start`
	# blocks for its full 30 seconds and then fails.
	setsid nohup ./run.sh </dev/null >>"$LOGFILE" 2>&1 9>&- &

	local i
	for i in $(seq 1 40); do
		running && break
		sleep 0.25
	done

	if ! running; then
		log "did not come up; last lines of $LOGFILE:"
		tail -n 20 "$LOGFILE" >&2 || true
		exit 1
	fi

	log "started pid $(cat "$PIDFILE")"
}

stop() {
	if ! running; then
		rm -f "$PIDFILE"
		return 0
	fi

	local pid
	pid=$(cat "$PIDFILE")

	# SIGINT, not SIGTERM: the app installs a handler for os.Interrupt as well,
	# but INT is what the sibling settled on and what the app's graceful
	# shutdown is tested against. Either is caught; picking one and keeping it
	# means the grace period is exercised on every restart rather than only in
	# theory.
	kill -INT "$pid" 2>/dev/null || true

	local i
	for i in $(seq 1 60); do
		running || break
		sleep 0.5
	done

	if running; then
		log "pid $pid ignored SIGINT for 30s; killing"
		kill -KILL "$pid" 2>/dev/null || true
		sleep 1
	fi

	rm -f "$PIDFILE"
	log "stopped"
}

status() {
	if running; then
		echo "running (pid $(cat "$PIDFILE"))"
		return 0
	fi

	echo "not running"
	return 1
}

case "${1:-status}" in
start) start ;;
stop) stop ;;
restart)
	stop
	start
	;;
status) status ;;
*)
	echo "usage: supervise.sh [start|stop|restart|status]" >&2
	exit 2
	;;
esac
