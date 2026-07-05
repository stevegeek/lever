#!/usr/bin/env bash
# Record an asciinema cast of the assistant-demo. Produces demo.cast in this dir.
#
# Requires: asciinema (https://asciinema.org) on PATH, plus the demo prerequisites
# from README.md (lever + the agent image built, the two demo tools on PATH, an
# OAuth token at ~/.scion/oauth-token, and weather-stub already running in
# another terminal). Run it FROM this directory.
#
#   ./record-demo.sh
#
# The deterministic setup (version, dry-run, doctor) is captured automatically;
# then the script hands you an interactive `lever up` so the morning standup —
# the payoff — is a real recorded conversation, not a canned one. Type `morning`
# at the manager, watch the standup, then Ctrl-b d to detach and stop the cast.
set -euo pipefail

command -v asciinema >/dev/null || { echo "install asciinema first: https://asciinema.org" >&2; exit 1; }
command -v lever     >/dev/null || { echo "lever not on PATH — run 'make all' in the repo root" >&2; exit 1; }

CAST="${1:-demo.cast}"

# The recorded session: a short scripted intro over REAL commands, then the live
# standup. `asciinema rec -c` records the given command; we drive the setup with
# a heredoc and drop into `lever up` for the interactive part.
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
