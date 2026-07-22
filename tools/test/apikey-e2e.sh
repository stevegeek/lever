#!/usr/bin/env bash
# Live end-to-end test for api-key LLM mode (headline path), using a fake upstream
# so no real Anthropic key/cost is involved.
#
# Asserts the full chain works: a uniform api-key instance brings up; the manager
# boots, enrols, mints a capability biscuit, and writes it into settings.json; the
# manager's claude turn routes through the broker /llm proxy, which STRIPS the
# biscuit and INJECTS the real (fake) Console key, reaching the fake upstream. Key
# isolation is verified: the real key appears in NO container byte; the agent holds
# only the biscuit + a placeholder. The G2 renew sidecar is confirmed running.
# Finally it asserts closed-egress containment: from inside the jail the
# allowlisted broker jail_port is reachable while the non-allowlisted admin_port and
# the public internet are DROPped.
#
# Prereqs: OrbStack + rootless podman; the lever-claude image built with the
# current hook + lever-agent (the Makefile target rebuilds the host `lever` and the
# image bins). Run via `make test-apikey-e2e` or directly.
#
# Env overrides: NAME (instance/machine name), JAIL_PORT, ADMIN_PORT, FAKE_PORT,
# KEEP=1 (skip teardown for inspection).
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NAME="${NAME:-apikeye2e}"
JAIL_PORT="${JAIL_PORT:-8443}"
ADMIN_PORT="${ADMIN_PORT:-8444}"
FAKE_PORT="${FAKE_PORT:-8098}"
MACHINE="lever-${NAME}"
CONTAINER="lever--${NAME}"
LEVER="${LEVER:-$HOME/.local/bin/lever}"
FAKEKEY="sk-ant-FAKEKEY-e2e-do-not-use-$$"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/apikey-e2e.XXXXXX")"
INST="$WORK/instance"
FAKE_LOG="$WORK/fake.log"
FAKE_PID=""
fail=0

say()  { printf '\n=== %s ===\n' "$*"; }
ok()   { printf '  PASS: %s\n' "$*"; }
bad()  { printf '  FAIL: %s\n' "$*"; fail=1; }
inctr(){ orb -m "$MACHINE" bash -lc "podman exec $CONTAINER sh -lc '$*'" 2>/dev/null; }

cleanup() {
  # `go run` forks a child, so kill the fakeupstream by its listen address (the
  # parent PID alone leaks the child); the broker is detached (Setpgid) so pkill
  # by its instance config path.
  pkill -f "fakeupstream -addr 127.0.0.1:$FAKE_PORT" 2>/dev/null
  [ -n "$FAKE_PID" ] && kill "$FAKE_PID" 2>/dev/null
  if [ "${KEEP:-0}" = "1" ]; then
    echo "KEEP=1: leaving machine $MACHINE + broker up; work dir $WORK"
    return
  fi
  pkill -f "lever broker serve.*$INST" 2>/dev/null
  orb delete -f "$MACHINE" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

# --- build the api-key test instance --------------------------------------------
say "build instance $INST"
mkdir -p "$INST/workspace"
touch "$INST/workspace/.gitkeep"
printf '%s' "$FAKEKEY" > "$INST/console-api-key"; chmod 600 "$INST/console-api-key"
cat > "$INST/manager.md" <<'EOF'
You are a test manager for an api-key live e2e. Respond with exactly the single
word PONG and nothing else. Do not use any tools.
EOF
cat > "$INST/lever.yaml" <<EOF
name: $NAME
scion:
  version: ${SCION_VERSION:-b4c9911d}
backend: orbstack
egress: closed
tree: workspace
manager:
  image: scionlocal/lever-claude
  prompt_file: manager.md
  allow_ports: []
broker:
  jail_port: $JAIL_PORT
  admin_port: $ADMIN_PORT
  llm_auth: api-key
  api_key_file: console-api-key
  llm_upstream: http://127.0.0.1:$FAKE_PORT
EOF

# --- clean slate (a stale closed-egress machine can't be re-applied) -------------
say "clean slate"
pkill -f "lever broker serve.*$INST" 2>/dev/null
orb delete -f "$MACHINE" >/dev/null 2>&1 && echo "deleted stale $MACHINE"

# --- fake upstream ---------------------------------------------------------------
say "start fake upstream on 127.0.0.1:$FAKE_PORT"
( cd "$REPO_ROOT" && go run ./tools/test/fakeupstream -addr "127.0.0.1:$FAKE_PORT" -log "$FAKE_LOG" ) >/dev/null 2>&1 &
FAKE_PID=$!
for _ in $(seq 1 20); do curl -s -o /dev/null "http://127.0.0.1:$FAKE_PORT/" && break; sleep 0.5; done
: > "$FAKE_LOG"  # clear the readiness probe line

# --- bring up --------------------------------------------------------------------
say "lever apply"
( cd "$INST" && "$LEVER" apply ) || { bad "lever apply failed"; exit 1; }
ok "apply succeeded"

# --- wait for the manager's claude turn to hit /llm ------------------------------
say "await broker /llm -> fake upstream (the injected real key)"
hit=""
for _ in $(seq 1 60); do
  if grep -q "x-api-key=\"$FAKEKEY\"" "$FAKE_LOG" 2>/dev/null; then hit=1; break; fi
  sleep 1
done
if [ -n "$hit" ]; then ok "fake upstream received the injected key"; else bad "no /llm traffic with the injected key within 60s"; fi

# --- assertions ------------------------------------------------------------------
say "assert: biscuit stripped, real key injected"
line="$(grep "x-api-key=\"$FAKEKEY\"" "$FAKE_LOG" | tail -1)"
echo "  upstream: $line"
echo "$line" | grep -q 'authorization=""' && ok "capability biscuit stripped (authorization empty)" || bad "biscuit not stripped"

say "assert: container has the biscuit + IP base url, NOT the real key"
env_tok="$(inctr 'cat /home/scion/.claude/settings.json' | tr ',{}' '\n' | grep -c ANTHROPIC_AUTH_TOKEN)"
[ "${env_tok:-0}" -ge 1 ] && ok "settings.json has ANTHROPIC_AUTH_TOKEN" || bad "settings.json missing ANTHROPIC_AUTH_TOKEN"
inctr 'cat /home/scion/.claude/settings.json' | grep -q 'ANTHROPIC_BASE_URL.*:'"$JAIL_PORT"'/llm' && ok "ANTHROPIC_BASE_URL points at the broker /llm" || bad "ANTHROPIC_BASE_URL wrong"
realct="$(inctr "grep -rl '$FAKEKEY' /home/scion 2>/dev/null | wc -l")"
[ "${realct:-1}" = "0" ] && ok "real key in NO container byte under /home/scion" || bad "real key LEAKED into the container ($realct files)"
inctr 'printenv ANTHROPIC_API_KEY' | grep -q 'placeholder' && ok "container ANTHROPIC_API_KEY is the placeholder sentinel" || bad "container ANTHROPIC_API_KEY is not the placeholder"

say "assert: G2 renew sidecar running"
inctr 'ps aux | grep -q "[l]ever-agent renew --loop" && echo yes' | grep -q yes && ok "lever-renew sidecar is running" || bad "renew sidecar not running"

# --- closed-egress containment (the headline property) ---------------------------------
# Differential, host-internet-independent: on the broker's alias IP the allowlisted
# jail_port must be reachable while the non-allowlisted admin_port is DROPped (the
# catch-all/alias DROP makes a non-allowlisted port hang → curl timeout, exit 28).
# This proves the egress allowlist does port-level filtering on the alias, not just
# "the host happens to have no route".
say "assert: closed-egress allowlist (jail_port reachable, admin_port + public internet DROPped)"
aliasip="$(inctr 'cat /home/scion/.claude/settings.json' | grep -oE 'https://[0-9.]+:' | head -1 | sed -E 's#https://([0-9.]+):#\1#')"
if [ -z "$aliasip" ]; then
  bad "could not resolve broker alias IP from settings.json"
else
  echo "  broker alias IP: $aliasip (jail_port=$JAIL_PORT admin_port=$ADMIN_PORT)"
  # jail_port: allowed → TCP connects (TLS may fail without a client cert, but NOT a timeout).
  jx="$(inctr "curl -sS -k --max-time 6 -o /dev/null https://$aliasip:$JAIL_PORT/ ; echo \$?" | tail -1)"
  if [ "$jx" = "28" ] || [ "$jx" = "7" ]; then bad "jail_port unreachable (exit $jx) — allowlist too tight"; else ok "jail_port reachable (curl exit $jx, TCP allowed)"; fi
  # admin_port on the alias: NOT allowlisted for the jail → DROP → timeout (exit 28).
  ax="$(inctr "curl -sS --max-time 6 -o /dev/null http://$aliasip:$ADMIN_PORT/epoch ; echo \$?" | tail -1)"
  [ "$ax" = "28" ] && ok "admin_port DROPped from the jail (curl timeout)" || bad "admin_port reachable from the jail (exit $ax) — loopback admin surface exposed"
  # arbitrary public internet (literal IP, bypasses the dropped DNS): catch-all DROP → timeout.
  px="$(inctr "curl -sS --max-time 6 -o /dev/null https://1.1.1.1/ ; echo \$?" | tail -1)"
  [ "$px" = "28" ] && ok "public internet DROPped (curl timeout to 1.1.1.1)" || bad "public internet reachable from the jail (exit $px) — egress not closed"
fi

# --- verdict ---------------------------------------------------------------------
say "result"
if [ "$fail" = "0" ]; then echo "  ✅ api-key e2e PASSED"; else echo "  ❌ api-key e2e FAILED"; fi
exit "$fail"
