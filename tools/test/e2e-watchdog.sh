#!/usr/bin/env bash
# e2e-watchdog.sh <secs> <command...>: the command under timeout(1), which
# runs it in a process group of its own so that the watchdog stops all of it.
# The terminal's Ctrl-C does not reach that group, so it is passed on here as
# a TERM, which runs the e2e's own cleanup.
set -u
secs=$1
shift
timeout -k 300 "$secs" "$@" </dev/null &
pid=$!
trap 'kill -TERM "$pid" 2>/dev/null' INT TERM HUP
# A trapped signal cuts a wait short: wait again until the command is gone.
while :; do
	wait "$pid"
	rc=$?
	kill -0 "$pid" 2>/dev/null || break
done
exit "$rc"
