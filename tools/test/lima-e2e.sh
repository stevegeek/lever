#!/usr/bin/env bash
# Live lima-backend e2e: bring a jail VM up, pass the §19 `lever acceptance`
# six-check capability gate under BOTH egress postures, assert the guest
# port-forward suppression our template depends on, prove the closed-egress
# re-bring-up never reopens egress (I2), then tear down. Non-interactive; needs
# Lima >= 2.0 (`brew install lima`).
#
# This is the lima arc's merge gate. It deliberately does NOT run a full `lever
# apply` (manager image + worker dispatch) — that is instance-specific and out of
# scope here; `lever acceptance` does its own broker-only bring-up (jail up +
# egress + host broker + tools + manager/worker enrol) with no container image.
#
# The gate exercises the live-only lima paths the unit tests can't: `limactl
# create/start --tty=false`, flag-forwarding through `limactl shell <vm> <cmd>`,
# host.lima.internal in-VM resolution, passwordless sudo, stdin forwarding
# through `limactl shell` (the scion + lever-agent install pipes), and
# header-free `limactl list --format`.
#
# Env overrides: NAME, JAIL_PORT, ADMIN_PORT, SUPPRESS_PORT, SCION_VERSION,
# KEEP=1 (skip teardown for inspection).
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NAME="${NAME:-limae2e}"
JAIL_PORT="${JAIL_PORT:-8443}"
ADMIN_PORT="${ADMIN_PORT:-8444}"
SUPPRESS_PORT="${SUPPRESS_PORT:-8898}"
MACHINE="lever-${NAME}"
LEVER="${LEVER:-$HOME/.local/bin/lever}"

# Ensure the REAL Go toolchain is on PATH regardless of CWD. lever shells out to
# `go` (the host-side scion cross-compile) from the instance dir; under an
# asdf-style shim that resolves the toolchain by walking up from CWD for a
# .tool-versions, a temp instance dir has none, so the shim fails (exit 126).
# Prepending the resolved GOROOT/bin — as GitHub's CI runners already have `go`
# directly on PATH — makes `go` resolve from any CWD. No-op when `go` is already
# a real binary on PATH.
export PATH="$(cd "$REPO_ROOT" && go env GOROOT)/bin:$PATH"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/lima-e2e.XXXXXX")"
INST="$WORK/instance"
BIN="$WORK/bin"
fail=0

say()  { printf '\n=== %s ===\n' "$*"; }
ok()   { printf '  PASS: %s\n' "$*"; }
bad()  { printf '  FAIL: %s\n' "$*"; fail=1; }

cleanup() {
  if [ "${KEEP:-0}" = "1" ]; then
    echo "KEEP=1: leaving VM $MACHINE + broker up; work dir $WORK"
    return
  fi
  pkill -f "lever broker serve.*$INST" 2>/dev/null
  limactl delete --force "$MACHINE" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

# --- preflight -------------------------------------------------------------------
command -v limactl >/dev/null 2>&1 || { echo "limactl not found (brew install lima)"; exit 1; }
say "lima $(limactl --version 2>&1 | head -1)"

# Lima's vz VMs run the host's native arch; build the in-guest lever-agent to match.
case "$(uname -m)" in
  arm64|aarch64) GUESTARCH=arm64 ;;
  x86_64|amd64)  GUESTARCH=amd64 ;;
  *) echo "unsupported host arch $(uname -m)"; exit 1 ;;
esac

# --- build binaries --------------------------------------------------------------
# lever-tool-db runs as a HOST subprocess of the broker (native), addressed by an
# absolute path in the config so the broker's minimal PATH is irrelevant.
# lever-agent is copied INTO the guest by the acceptance setup (linux/<guestarch>).
say "build lever-tool-db (host) + lever-agent (linux/$GUESTARCH)"
mkdir -p "$BIN"
( cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$BIN/lever-tool-db" ./cmd/lever-tool-db ) || { echo "build lever-tool-db failed"; exit 1; }
( cd "$REPO_ROOT" && GOOS=linux GOARCH="$GUESTARCH" CGO_ENABLED=0 go build -o "$BIN/lever-agent" ./cmd/lever-agent ) || { echo "build lever-agent failed"; exit 1; }
export LEVER_AGENT_BIN="$BIN/lever-agent"

# --- instance fixtures -----------------------------------------------------------
# Mirror the acceptance fixture (manager + executor worker `worker` + `db` tool)
# but on backend: lima, with an absolute tool path and a dummy 0600 api-key file
# for the closed (api-key) posture. The db tool auto-seeds ref.db (tables A/B/C).
say "build instance $INST"
mkdir -p "$INST/workspace" "$INST/workers/worker"
touch "$INST/workspace/.gitkeep" "$INST/workers/worker/.gitkeep"
cat > "$INST/manager.md" <<'EOF'
Manager: bring up the worker and delegate db.read to it for the acceptance run.
EOF
# Dummy key (never used — acceptance routes no LLM); satisfies api-key validation.
printf 'sk-ant-FAKE-lima-e2e-%s' "$$" > "$INST/console-api-key"
chmod 600 "$INST/console-api-key"

# write_config <open|closed> — regenerates $INST/lever.yaml for the given posture.
# open   → subscription broker, egress open (default).
# closed → api-key broker (dummy key file), egress closed (catch-all DROP).
write_config() {
  local posture="$1" authblock egressline
  if [ "$posture" = closed ]; then
    authblock=$'  llm_auth: api-key\n  api_key_file: console-api-key'
    egressline='egress: closed'
  else
    authblock='  llm_auth: subscription'
    egressline='egress: open'
  fi
  cat > "$INST/lever.yaml" <<EOF
name: $NAME
scion:
  version: ${SCION_VERSION:-b4c9911d}
backend: lima
$egressline
tree: workspace
manager:
  image: scionlocal/lever-claude
  prompt_file: manager.md
  allow_ports: []
  delegate:
    - tool: db
      op: read
      to: [worker]
workers:
  - name: worker
    dir: workers/worker
    obtain: []
broker:
  jail_port: $JAIL_PORT
  admin_port: $ADMIN_PORT
$authblock
  tools:
    - name: db
      command: [$BIN/lever-tool-db, -dsn, "file:$INST/ref.db"]
      backend: 127.0.0.1:3201
      operations:
        - name: read
          caveat_param:
            table: table
            filter: filter
      allowed_values:
        table: [A, B]
EOF
}

# --- clean slate -----------------------------------------------------------------
say "clean slate"
pkill -f "lever broker serve.*$INST" 2>/dev/null
limactl delete --force "$MACHINE" >/dev/null 2>&1 && echo "deleted stale $MACHINE"

# egress_closed_in_guest — true iff the LEVER_EGRESS chain has the catch-all DROP.
egress_closed_in_guest() {
  limactl shell "$MACHINE" sudo iptables -S LEVER_EGRESS 2>/dev/null | grep -qE '^-A LEVER_EGRESS -j DROP'
}

# stop_broker — stop the host-side broker via its PID file (fallback: pattern),
# leaving the VM and its egress chain intact. Used before the idempotent re-apply
# so the re-run gets a fresh broker: `lever acceptance`'s broker is a one-shot
# process tied to that invocation and is not meant to be reused across a second
# full gate. Stopping it does NOT touch the guest egress, so ApplyEgress on the
# re-run still takes the I2 skip path (the property under test).
stop_broker() {
  local pidf="$INST/.lever-state/broker.pid"
  [ -f "$pidf" ] && kill "$(cat "$pidf")" 2>/dev/null || true
  pkill -f "lever broker serve.*$INST" 2>/dev/null || true
  sleep 1
}

# run_acceptance — bring up the current-posture instance and run the six checks.
# `lever acceptance` does its own broker-only bring-up (jail up + egress + host
# broker + tools + manager/worker enrol) and leaves the broker running. Nothing
# is cleared between calls: a re-run against the still-running broker exercises the
# idempotent bootstrap-latch path (§4), and a posture switch is done with an
# explicit `lever down` first (§3), which stops the broker via its PID file.
run_acceptance() {
  ( cd "$INST" && "$LEVER" acceptance )
}

# ================================================================================
# 1. OPEN posture — full VM bring-up (slow: apt + rootless docker + scion) then the
#    six §19 checks.
# ================================================================================
say "OPEN posture: lever acceptance (first bring-up — several minutes)"
write_config open
if run_acceptance; then ok "acceptance PASSED (open egress)"; else bad "acceptance FAILED (open egress)"; fi

# ================================================================================
# 2. Guest port-forward suppression — the template hazard. A guest 0.0.0.0
#    listener must NOT be reachable on the host loopback (stock lima would forward
#    it; our template's ignore rules suppress that). Poll until the listener binds
#    in the guest so the check is never vacuous (a slow first python import must
#    not race a fixed sleep).
# ================================================================================
say "guest port-forward suppression (host loopback must NOT see a guest listener)"
# One backgrounded `limactl shell` runs an inline python (fed over stdin) that
# binds a raw 0.0.0.0 socket, SELF-VERIFIES it is listening (connects to itself),
# prints GUEST_BOUND, then holds the port open for the probe window. Readiness is
# detected host-locally by grepping that output — no rapid-fire `limactl shell`
# polling (which contends on the shared SSH ControlMaster and returns spurious
# 255s) and no fragile in-guest daemonization or slow http.server bind. A raw
# accept-and-close socket gives a crisp host signal: curl exit 7 = connection
# refused (suppressed, PASS); any connect (exit 0/52/28) = reachable (FAIL).
suppout="$WORK/supp.out"; : > "$suppout"
( limactl shell "$MACHINE" -- python3 - "$SUPPRESS_PORT" <<'PY' > "$suppout" 2>&1
import socket, sys, threading, time
port = int(sys.argv[1])
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", port)); s.listen(5)
def serve():
    while True:
        try:
            c, _ = s.accept(); c.close()
        except OSError:
            break
threading.Thread(target=serve, daemon=True).start()
socket.create_connection(("127.0.0.1", port), timeout=3).close()  # self-verify bound
print("GUEST_BOUND", flush=True)
time.sleep(25)
PY
) &
supp_pid=$!
bound=""
for _ in $(seq 1 20); do grep -q GUEST_BOUND "$suppout" 2>/dev/null && { bound=1; break; }; sleep 1; done
if [ -n "$bound" ]; then
  curl -sS -m 3 -o /dev/null "http://127.0.0.1:$SUPPRESS_PORT/" 2>/dev/null; hrc=$?
  if [ "$hrc" = "7" ]; then
    ok "guest listener NOT reachable on host loopback (forwarding suppressed)"
  else
    bad "guest listener reachable on host loopback (curl exit $hrc — port-forward NOT suppressed)"
  fi
else
  bad "guest listener never came up in-VM — suppression check vacuous"
fi
kill "$supp_pid" 2>/dev/null || true
limactl shell "$MACHINE" -- sh -c "pkill -f 'python3 -' 2>/dev/null" 2>/dev/null || true

# ================================================================================
# 3. Switch to CLOSED posture. Changing egress on a live instance is deliberately
#    unsupported (it would briefly reopen egress); the supported path is `lever
#    down` then a fresh bring-up. `lever down` stops the broker (via its PID file)
#    and tears the VM down; the persisted broker CA is reused. The closed bring-up
#    then re-provisions a fresh VM and runs the six checks under closed egress (the
#    dial-by-IP path: guest DNS is dropped, agents reach the broker by alias IP).
# ================================================================================
say "switch posture: lever down (open) then bring up CLOSED"
( cd "$INST" && "$LEVER" down ) || bad "lever down (open) failed"
write_config closed
if run_acceptance; then ok "acceptance PASSED (closed egress)"; else bad "acceptance FAILED (closed egress)"; fi
if egress_closed_in_guest; then ok "LEVER_EGRESS chain is closed (catch-all DROP present)"; else bad "closed posture did not install the catch-all DROP"; fi

# ================================================================================
# 4. Idempotent closed re-apply (I2: never briefly reopen egress). Re-run the gate
#    WITHOUT tearing down — against the still-running broker (idempotent bootstrap
#    latch) and the live closed egress chain (ApplyEgress takes the I2 skip path,
#    no flush/rebuild). The chain must stay closed throughout and the six checks
#    must still pass. (A full `lever apply` re-run is out of scope — no image;
#    `lever acceptance` drives the same EnsureUp/ApplyEgress I2 path.)
# ================================================================================
say "idempotent closed re-apply (egress must stay closed; I2)"
egress_closed_in_guest && echo "  pre: chain closed" || bad "pre-condition: chain not closed before re-apply"
stop_broker
if run_acceptance; then ok "acceptance PASSED on closed re-apply (idempotent)"; else bad "acceptance FAILED on closed re-apply"; fi
if egress_closed_in_guest; then ok "LEVER_EGRESS still closed after re-apply (egress never reopened)"; else bad "egress chain not closed after re-apply"; fi

# ================================================================================
# 5. Teardown.
# ================================================================================
say "teardown"
pkill -f "lever broker serve.*$INST" 2>/dev/null
if ( cd "$INST" && "$LEVER" down ); then
  if limactl list --format '{{.Name}} {{.Status}}' | grep -q "^$MACHINE "; then
    bad "VM $MACHINE still present after lever down"
  else
    ok "VM torn down"
  fi
else
  bad "lever down failed"
fi

# --- verdict ---------------------------------------------------------------------
say "result"
if [ "$fail" = "0" ]; then echo "lima e2e: PASS"; else echo "lima e2e: FAIL"; fi
exit "$fail"
