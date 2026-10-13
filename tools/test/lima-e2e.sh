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
# Host ports: the broker's jail and admin ports, the db tool's backend and the
# port the suppression check listens on in the guest (which must be free on
# the host too, or the check would reach the host's listener). Each is picked
# free on 127.0.0.1 unless set, so a run never lands on another lever
# instance's ports (lever#181). A port set explicitly that is in use stops the
# run before anything starts, naming the process holding it.
#
# Env overrides: NAME, JAIL_PORT, ADMIN_PORT, TOOL_PORT, SUPPRESS_PORT,
# SCION_VERSION, KEEP=1 (skip teardown for inspection). On failure the end of
# the broker and tool logs is printed (masked), with or without KEEP=1.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NAME="${NAME:-limae2e}"
MACHINE="lever-${NAME}"
LEVER="${LEVER:-$HOME/.local/bin/lever}"

# --- host ports ------------------------------------------------------------------
# Chosen before anything is created, so a refusal leaves nothing to clean up.
# port_busy <port>: something accepts connections on 127.0.0.1:<port>.
port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

# port_holder <port>: the process listening on <port>, as best the host can tell.
port_holder() {
  local out=""
  if command -v lsof >/dev/null 2>&1; then
    out="$(lsof -nP -iTCP:"$1" -sTCP:LISTEN -Fpc 2>/dev/null | awk '
      /^p/ && !p { p = substr($0, 2) } /^c/ && !c { c = substr($0, 2) }
      END { if (p) printf "pid %s (%s)", p, c }')"
  fi
  if [ -z "$out" ] && command -v ss >/dev/null 2>&1; then
    out="$(ss -ltnpH "sport = :$1" 2>/dev/null | sed -n 's/.*users:(\(.*\))$/\1/p' | head -1)"
  fi
  echo "${out:-an unknown process (lsof/ss could not tell)}"
}

# pick_port <var>: keep <var> when set (refusing to start if it is in use),
# else set it to a free port at or above $next_port, not one already taken by
# this run.
picked=" "
next_port=$((20000 + RANDOM % 9000))
pick_port() {
  local var="$1" val="${!1:-}"
  if [ -n "$val" ]; then
    if port_busy "$val"; then
      echo "$var=$val is in use on 127.0.0.1 by $(port_holder "$val"); stop it or pick another port"
      exit 1
    fi
  else
    val=$next_port
    while port_busy "$val" || [[ "$picked" == *" $val "* ]]; do
      val=$((val + 1))
      [ "$val" -lt 65000 ] || { echo "no free port found for $var"; exit 1; }
    done
    next_port=$((val + 1))
  fi
  picked="$picked$val "
  printf -v "$var" '%s' "$val"
}
for v in JAIL_PORT ADMIN_PORT TOOL_PORT SUPPRESS_PORT; do pick_port "$v"; done
if [ "$(printf '%s\n' "$JAIL_PORT" "$ADMIN_PORT" "$TOOL_PORT" "$SUPPRESS_PORT" | sort -u | wc -l)" -ne 4 ]; then
  echo "JAIL_PORT, ADMIN_PORT, TOOL_PORT and SUPPRESS_PORT must differ:$picked"; exit 1
fi


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

# Every line carries the time, and each phase its duration, so a slow run
# shows where the time went (lever#184). lever itself prints its bring-up
# steps as plain timestamped lines (LEVER_PROGRESS=plain) and records them in
# the instance's .lever-state/logs/.
export LEVER_PROGRESS="${LEVER_PROGRESS:-plain}"
# UTC, like lever's log lines, so a line here and one in a log tail compare.
ts()   { date -u +%H:%M:%SZ; }
phase_name=""; phase_start=$SECONDS; phase_times=""
end_phase() {
  [ -n "$phase_name" ] || return 0
  d=$((SECONDS - phase_start))
  printf '%s --- %s took %ds\n' "$(ts)" "$phase_name" "$d"
  phase_times="$phase_times$(printf '  %5ds  %s' "$d" "$phase_name")
"
}
say()  { end_phase; phase_name="$*"; phase_start=$SECONDS; printf '\n%s === %s ===\n' "$(ts)" "$*"; }
ok()   { printf '%s   PASS: %s\n' "$(ts)" "$*"; }
bad()  { printf '%s   FAIL: %s\n' "$(ts)" "$*"; fail=1; }

# mask_secrets hides anything shaped like a credential, on top of lever's own
# masking in these logs: every shape lever's redaction masks (a Go test,
# tools/test, holds the two together), plus PEM private keys, URL user
# info and the value of any secret/token/password/key field. perl, because
# BSD sed has no case-insensitive substitution; without perl nothing is
# printed rather than something unmasked.
mask_secrets() {
  if ! command -v perl >/dev/null 2>&1; then
    echo "      | (perl not found: log not shown unmasked)"
    return 0
  fi
  perl -pe '
    s/(^|\s)(-e|--env)([ =])([A-Za-z_][A-Za-z0-9_]*=)\S*/$1$2$3$4***/g;
    s/sk-ant-[A-Za-z0-9_-]+/sk-ant-***/g;
    s/\bscion_pat_[A-Za-z0-9_-]+/scion_pat_***/g;
    s/\b(gh[pousr]_|github_pat_)[A-Za-z0-9_]+/$1***/g;
    s/(authorization["\x27]?\s*[:=]\s*).*/$1***/gi;
    s/(bearer\s+)[A-Za-z0-9._~+\/=-]+/$1***/gi;
    s/\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*/***jwt***/g;
    s#(://)[^/\s:@]+:[^/\s@]+@#$1***@#g;
    $k = 1 if /-----BEGIN [A-Z ]*PRIVATE KEY-----/;
    if ($k) { $k = 0 if /-----END [A-Z ]*PRIVATE KEY-----/; $_ = "[private key masked]\n"; }
    s/((?:secret|token|password|passwd|credential|private[_-]?key|api[_-]?key)[A-Za-z_-]*["\x27]?\s*[:=]\s*["\x27]?)[^"\x27\s,}]+/$1***/gi;
    s/(--?[a-z0-9-]*(?:secret|token|password)[a-z0-9-]*)(?<!-file)(?<!-path)(?<!-dir)(?<!-env)(?<!-name)\s+[^\s-]\S*/$1 ***/gi;
  '
}

# show_logs prints the end of the instance's broker and tool logs, masked, so a
# failed run explains itself without KEEP=1. lever masks credentials in
# these files; they hold no keys. The exit trap prints them on a failed run
# unless a failed acceptance already did.
logs_shown=""
show_logs() {
  logs_shown=1
  st="$INST/.lever-state"
  for f in "$st/broker.out.log" "$st/broker.log" "$st"/tool-logs/*.log; do
    [ -f "$f" ] || continue
    printf '%s   --- last lines of %s:\n' "$(ts)" "${f#$WORK/}"
    tail -n 30 "$f" | mask_secrets | sed 's/^/      | /'
  done
}

cleanup() {
  rc=$?
  if { [ "$rc" != "0" ] || [ "$fail" != "0" ]; } && [ -z "$logs_shown" ]; then
    show_logs
  fi
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
echo "host ports: jail $JAIL_PORT, admin $ADMIN_PORT, db tool $TOOL_PORT, suppression check $SUPPRESS_PORT"

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
# Stamp the versions the Makefile stamps (LEVER_AGENT_LDFLAGS /
# LEVER_TOOL_DB_LDFLAGS) so the MCP serverInfo matches a `make` build.
LEVER_VERSION="$(sed -n 's/^const Version = "\(.*\)"/\1/p' "$REPO_ROOT/internal/cli/root.go")"
( cd "$REPO_ROOT" && CGO_ENABLED=0 go build -ldflags "-X main.Version=$LEVER_VERSION" -o "$BIN/lever-tool-db" ./cmd/lever-tool-db ) || { echo "build lever-tool-db failed"; exit 1; }
( cd "$REPO_ROOT" && GOOS=linux GOARCH="$GUESTARCH" CGO_ENABLED=0 go build -ldflags "-X github.com/stevegeek/lever/internal/agent.Version=$LEVER_VERSION" -o "$BIN/lever-agent" ./cmd/lever-agent ) || { echo "build lever-agent failed"; exit 1; }
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
  # Must stay >= ce96122c: a pre-enforcement pin cannot catch the as_needed
  # injection or plaintext-encoding regressions this suite exists for.
  version: ${SCION_VERSION:-e82a2a08}
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
      backend: 127.0.0.1:$TOOL_PORT
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

# egress_one_reject_in_guest — true iff the chain's only REJECT, in either
# family, is the metadata /32 (lever#182); everything else it refuses is a DROP.
egress_one_reject_in_guest() {
  local v4 v6
  v4=$(limactl shell "$MACHINE" sudo iptables -S LEVER_EGRESS 2>/dev/null | grep -e '-j REJECT')
  v6=$(limactl shell "$MACHINE" sudo ip6tables -S LEVER_EGRESS 2>/dev/null | grep -e '-j REJECT')
  [ "$v4" = "-A LEVER_EGRESS -d 169.254.169.254/32 -j REJECT --reject-with icmp-host-prohibited" ] && [ -z "$v6" ]
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
  if ( cd "$INST" && "$LEVER" acceptance ); then
    return 0
  fi
  show_logs
  return 1
}

# ================================================================================
# 1. OPEN posture — full VM bring-up (slow: apt + podman + scion) then the
#    six §19 checks.
# ================================================================================
say "OPEN posture: lever acceptance (first bring-up — several minutes)"
write_config open
if run_acceptance; then ok "acceptance PASSED (open egress)"; else bad "acceptance FAILED (open egress)"; fi
if egress_one_reject_in_guest; then ok "LEVER_EGRESS rejects only the metadata /32 (open egress)"; else bad "LEVER_EGRESS REJECT rules are not exactly the metadata /32 (open egress)"; fi
# lever#183: provisioning installs and starts no dockerd (scion runs podman).
if limactl shell "$MACHINE" pgrep -x dockerd >/dev/null 2>&1; then bad "a dockerd runs in the guest"; else ok "no dockerd in the guest"; fi
# lever#201: the guest keeps the core of a crashed process named "tmux: server"
# (and drops other dumps), root only, even with the crashing process's core
# size limit at 0. Two python processes stand in: one renames itself and
# segfaults, a decoy segfaults under its own name. A second "tmux: server"
# crash right after is dropped (one kept core a minute), and the kernel runs
# one core handler at a time (core_pipe_limit=1).
core_capture_in_guest() {
  limactl shell "$MACHINE" -- sh -s <<'SH'
set -u
d=/var/lib/lever/cores
sudo sh -c "rm -f $d/core.tmux.*" || exit 1
ulimit -c 0
python3 -c 'import os, signal; os.kill(os.getpid(), signal.SIGSEGV)' 2>/dev/null
python3 -c 'import ctypes, os, signal; ctypes.CDLL(None).prctl(15, b"tmux: server", 0, 0, 0); os.kill(os.getpid(), signal.SIGSEGV)' 2>/dev/null
i=0
while [ "$i" -lt 20 ]; do
  n=$(sudo sh -c "ls $d" 2>/dev/null | grep -c '^core\.tmux\.[0-9]*\.[0-9]*$')
  [ "$n" -ge 1 ] && break
  sleep 0.5; i=$((i+1))
done
python3 -c 'import ctypes, os, signal; ctypes.CDLL(None).prctl(15, b"tmux: server", 0, 0, 0); os.kill(os.getpid(), signal.SIGSEGV)' 2>/dev/null
sleep 1
echo "pattern=$(cat /proc/sys/kernel/core_pattern)"
echo "pipelimit=$(cat /proc/sys/kernel/core_pipe_limit)"
echo "cores=$n"
echo "after=$(sudo sh -c "ls $d" 2>/dev/null | grep -c '^core\.tmux\.[0-9]*\.[0-9]*$')"
echo "dir=$(sudo stat -c '%a %U' $d)"
echo "file=$(sudo sh -c "stat -c '%a %U %s' $d/core.tmux.*" 2>/dev/null | head -1)"
sudo sh -c "rm -f $d/core.tmux.*"
SH
}
core_out=$(core_capture_in_guest 2>&1)
if printf '%s\n' "$core_out" | grep -q '^pattern=|/usr/local/libexec/lever-core-handler %P %t %e$' &&
  printf '%s\n' "$core_out" | grep -q '^pipelimit=1$' &&
  printf '%s\n' "$core_out" | grep -q '^cores=1$' &&
  printf '%s\n' "$core_out" | grep -q '^after=1$' &&
  printf '%s\n' "$core_out" | grep -q '^dir=700 root$' &&
  printf '%s\n' "$core_out" | grep -Eq '^file=600 root [1-9][0-9]*$'; then
  ok "the guest keeps a crashed tmux server's core, root only, drops the decoy's and a second within the minute"
else
  bad "guest tmux core capture: $(printf '%s' "$core_out" | tr '\n' ' ')"
fi

# What it proves: internal/apply/guardlive_vm_test.go (TestReadOnlyGuardLiveInRealGuest).
say "read-only guard live check (keep-id containers over the tree mount)"
guardout="$WORK/guard.out"
( cd "$REPO_ROOT" && LEVER_GUARD_VM_PREFIX="limactl,shell,$MACHINE" \
    LEVER_GUARD_TREE_HOST="$INST/workspace" LEVER_GUARD_TREE_GUEST=/lever \
    go test -tags integration -count=1 -timeout 10m -v \
      -run '^TestReadOnlyGuardLiveInRealGuest$' ./internal/apply/ ) > "$guardout" 2>&1
sed 's/^/      | /' "$guardout"
# A test killed before its own cleanup leaves these behind, and later steps
# mount the same workspace.
rm -rf "$INST"/workspace/guard-e2e-*
limactl shell "$MACHINE" -- sh -s <<'SH' 2>&1 | sed 's/^/      | leftover removed: /'
export XDG_RUNTIME_DIR=/run/user/$(id -u)
podman ps -a --format '{{.Names}}' | grep '^lever-guard-e2e-' | xargs -r podman rm -f -t 0
podman images --format '{{.Repository}}' | grep '^localhost/lever-guard-e2e-' | xargs -r podman rmi -f
SH
guard_stages=$(sed -nE 's/^ *[A-Za-z0-9_]+_test\.go:[0-9]+: (.* on the host): (followed by the mount|reported) \(tree mount ([^)]*)\).*/\1: \2 (\3)/p' "$guardout" |
  awk 'NR>1{printf "; "}{printf "%s",$0}')
if grep -q '^--- PASS: TestReadOnlyGuardLiveInRealGuest' "$guardout"; then
  ok "read-only guard: namespaces as required, real mounts held, nested namespace refused, moved directory reported; ${guard_stages:-no host-replace stage seen}"
else
  bad "read-only guard live check (output above)"
fi

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
if egress_one_reject_in_guest; then ok "LEVER_EGRESS rejects only the metadata /32 (closed egress)"; else bad "LEVER_EGRESS REJECT rules are not exactly the metadata /32 (closed egress)"; fi

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
# 5. Log lines. Every line the broker and its tool wrote, across all three
#    bring-ups, has lever's one shape (UTC time, level, msg, component), and
#    the dummy API key appears in none of them.
# ================================================================================
say "log lines: one shape, no credential"
logline_re='^time=[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}Z level=(DEBUG|INFO|WARN|ERROR) msg=.* component=[a-z0-9-]+( |$)'
st="$INST/.lever-state"
key="$(cat "$INST/console-api-key")"
for f in "$st/broker.out.log" "$st/broker.log" "$st"/tool-logs/*.log; do
  [ -f "$f" ] || { bad "no ${f#$WORK/}"; continue; }
  odd="$(grep -vE "$logline_re" "$f" | grep -c .)"
  if [ "$odd" = "0" ]; then
    ok "$(grep -c . "$f") lines in lever's log shape: ${f#$WORK/}"
  else
    bad "$odd lines not in lever's log shape: ${f#$WORK/}"
    grep -vE "$logline_re" "$f" | grep . | head -n 5 | mask_secrets | sed 's/^/      | /'
  fi
  if grep -qF "$key" "$f"; then bad "the API key appears in ${f#$WORK/}"; fi
done

# ================================================================================
# 6. Teardown.
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
end_phase; phase_name=""
say "result"
printf 'phase times:\n%s' "$phase_times"
if [ "$fail" = "0" ]; then echo "lima e2e: PASS"; else echo "lima e2e: FAIL"; fi
exit "$fail"
