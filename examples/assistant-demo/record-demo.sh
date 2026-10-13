#!/usr/bin/env bash
# Record an asciinema cast of the assistant-demo into demo.cast in this dir.
#
# Requires asciinema on PATH plus the demo prerequisites from README.md
# (weather-stub already running in another terminal). Run it from this
# directory:
#
#   ./record-demo.sh
#
# The setup (version, dry-run, doctor) is scripted; then you get an
# interactive `lever up`. Type `morning` at the manager, watch the standup,
# then Ctrl-b d to detach and stop the cast.
set -euo pipefail

command -v asciinema >/dev/null || { echo "install asciinema first: https://asciinema.org" >&2; exit 1; }
command -v lever     >/dev/null || { echo "lever not on PATH — run 'make all' in the repo root" >&2; exit 1; }

CAST="${1:-demo.cast}"

asciinema rec --overwrite --title "lever assistant-demo" "$CAST" -c 'bash -lc "
  set -e
  echo \"\$ lever version\";           lever version;            sleep 1
  echo; echo \"\$ lever apply --dry-run\"; lever apply --dry-run; sleep 2
  echo; echo \"# scaffolding operator skills\"; echo \"\$ lever init\"; lever init; sleep 2
  echo; echo \"# bringing the assistant up — type: morning\"; sleep 1
  lever up
"'

echo
echo "Recorded $CAST."
echo "Preview:  asciinema play $CAST"
echo "Share:    asciinema upload $CAST   (or render a GIF with agg: agg $CAST demo.gif)"
