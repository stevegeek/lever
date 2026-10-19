#!/usr/bin/env bash
# Live Lima e2e of lever's lifecycle under races and faults, on the pinned
# scion. After one bring-up (a manager and a worker) it:
#   - holds the lifecycle lock and runs every command that takes it: each
#     exits 6 at once, names the holder and stages no enrolment ticket;
#   - races two lever commands at a time (up, apply, stop --keep-vm,
#     conversation new, worker resume, worker new-session): each pair ends
#     with both done, or one done and one clean refusal (exit 6 naming the
#     holder, or a worker verb's JSON refusal with a reason), and a plain
#     `lever up` then converges;
#   - kills the hub, the host broker, lever itself or the guest (limactl stop
#     -f) once a chosen step of `lever up` has started, and checks that the
#     next `lever up` converges on the same manager record, keeping its
#     conversation.
# Every run is bounded: scenarios that no longer fit CHAOS_BUDGET are
# reported as SKIP, never run half.
#
# Needs: Lima >= 2.0, Go, perl, python3, and the agent image as a docker
# archive: IMAGE_TAR, or IMAGE_CACHE naming an archive `make
# test-lima-stack-e2e IMAGE_CACHE=...` saved for this tree (the same key
# check). This script never builds the image: that alone can take longer
# than the budget.
#
# Env: SCION_VERSION (default: the release pin), IMAGE_TAR, IMAGE_CACHE, IMAGE
# (default lever.local/lever-claude-slim), LEVER (default ~/.local/bin/lever),
# CHAOS_NAME (default chaose2e; must end in e2e), CHAOS_BUDGET (seconds,
# default 1800), CHAOS_OFFSET (seconds between the two commands of a race,
# default 0.3), CHAOS_ONLY (a regex: run only the scenarios whose name
# matches), CHAOS_CALL_TIMEOUT (seconds for one command in the guest, default
# 180), KEEP=1 (leave the VM for inspection).
set -uo pipefail
exec </dev/null

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# Not NAME: an operator's shell may hold a NAME for a real instance, whose VM
# this run would delete.
NAME="${CHAOS_NAME:-chaose2e}"
case $NAME in
*e2e) ;;
*) echo "CHAOS_NAME=$NAME: the name must end in e2e (this run deletes the VM lever-<name>)"; exit 2 ;;
esac
VM="lever-${NAME}"
LEVER="${LEVER:-$HOME/.local/bin/lever}"
IMAGE="${IMAGE:-lever.local/lever-claude-slim}"
SCION_VERSION="${SCION_VERSION:-$(sed -n 's/^const ReleasePin = "\(.*\)"/\1/p' "$REPO_ROOT/internal/provision/scionbin/release.go")}"
BUDGET="${CHAOS_BUDGET:-1800}"
OFFSET="${CHAOS_OFFSET:-0.3}"
ONLY="${CHAOS_ONLY:-}"
STACK="$REPO_ROOT/tools/test/lima-stack-e2e.sh"

# fn_from <script> <name>: the definition of function <name> in <script>,
# a one-line one or one that ends at the next "}" in column 0. The stack
# e2e's helpers are reused as they are, not copied.
fn_from() {
	awk -v n="$2" '
		!on && index($0, n "() {") == 1 { on = 1; print; if ($0 ~ /}[[:space:]]*$/) exit; next }
		on { print; if ($0 ~ /^}/) exit }
	' "$1"
}
# The stack helpers' state.
fail=0
# shellcheck disable=SC2034 # read by the sourced say and end_phase
phase_name=""
# shellcheck disable=SC2034
phase_start=$SECONDS
phase_times=""
for f in ts end_phase say ok bad check checkx port_busy pick G X P running image_key hubbody cmdlines claude_up; do
	# shellcheck source=/dev/null
	source <(fn_from "$STACK" "$f")
	declare -F "$f" >/dev/null || { echo "$STACK no longer defines $f(); update this script"; exit 1; }
done
# shellcheck source=/dev/null
source <(fn_from "$REPO_ROOT/tools/test/lima-e2e.sh" mask_secrets)
declare -F mask_secrets >/dev/null || { echo "lima-e2e.sh no longer defines mask_secrets()"; exit 1; }
# CHAOS_SELFTEST=1 stops here, once the reused helpers resolved (the unit
# test of this script).
if [ "${CHAOS_SELFTEST:-0}" = 1 ]; then echo "chaos e2e helpers: ok"; exit 0; fi
skipped=0
skip() { printf '%s   SKIP: %s\n' "$(ts)" "$*"; skipped=$((skipped + 1)); }

command -v limactl >/dev/null 2>&1 || { echo "limactl not found"; exit 1; }
[ -x "$LEVER" ] || { echo "$LEVER not found (make install)"; exit 1; }
case "$(uname -m)" in arm64|aarch64) ARCH=arm64 ;; *) ARCH=amd64 ;; esac
LEVER_VERSION="$(sed -n 's/^const Version = "\(.*\)"/\1/p' "$REPO_ROOT/internal/cli/root.go")"
JAIL_ABI="$(sed -n 's/^const JailABI = \([0-9]*\)$/\1/p' "$REPO_ROOT/internal/jailabi/jailabi.go")"
export ARCH LEVER_VERSION JAIL_ABI

if [ -z "${IMAGE_TAR:-}" ] && [ -n "${IMAGE_CACHE:-}" ]; then
	if [ -f "$IMAGE_CACHE" ] && [ "$(cat "$IMAGE_CACHE.key" 2>/dev/null)" = "$(image_key)" ]; then
		IMAGE_TAR=$IMAGE_CACHE
	else
		echo "no reusable archive at $IMAGE_CACHE for this tree: run \`make test-lima-stack-e2e IMAGE_CACHE=$IMAGE_CACHE\` first"
		exit 1
	fi
fi
[ -n "${IMAGE_TAR:-}" ] || { echo "set IMAGE_TAR or IMAGE_CACHE (see the header)"; exit 1; }

# A hung guest must not outlast the budget: every call into it (X and P
# too) is bounded, ssh included. timeout gives the call a process group of
# its own, where a read of the terminal would stop it, so the run reads no
# terminal (exec </dev/null above).
G() { timeout -k 30 "${CHAOS_CALL_TIMEOUT:-180}" limactl shell --workdir / "$VM" -- "$@"; }

WORK="$(mktemp -d /tmp/lc.XXXXXX)"
INST="$WORK/i"
# Set by the clean slate: until then the VM by this name is not this run's.
slate=0
cleanup() {
	rc=$?
	# A race or the lock holder may still run when the run is cut short.
	jobs -p | xargs -r kill 2>/dev/null
	if [ "${KEEP:-0}" = 1 ]; then echo "KEEP=1: leaving $VM and $WORK"; return; fi
	pkill -f "(broker|remote) serve .*$INST/" 2>/dev/null
	[ "$slate" = 1 ] && limactl delete --force "$VM" >/dev/null 2>&1
	rm -rf "$WORK"
	exit $rc
}
trap cleanup EXIT
PATH="$(cd "$REPO_ROOT" && go env GOROOT)/bin:$PATH"
export PATH LEVER_PROGRESS=plain

JP=$(pick); AP=$(pick)
while [ "$AP" = "$JP" ]; do AP=$(pick); done

say "build (scion $SCION_VERSION, lever $LEVER_VERSION, jail ABI $JAIL_ABI, $ARCH)"
echo "lever: $("$LEVER" version 2>&1 | head -1)"
mkdir -p "$WORK/bin"
(cd "$REPO_ROOT" && GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -ldflags "-X github.com/stevegeek/lever/internal/agent.Version=$LEVER_VERSION" -o "$WORK/bin/lever-agent" ./cmd/lever-agent) || exit 1
export LEVER_AGENT_BIN="$WORK/bin/lever-agent"

mkdir -p "$INST/workspace" "$INST/workers/worker"
touch "$INST/workspace/.gitkeep" "$INST/workers/worker/.gitkeep"
echo "Manager for the chaos e2e. Do nothing unless asked." > "$INST/manager.md"
echo "Worker for the chaos e2e. Do nothing." > "$WORK/task.txt"
printf 'sk-ant-FAKE-chaos-%s' "$$" > "$INST/console-api-key"; chmod 600 "$INST/console-api-key"
ln -f "$IMAGE_TAR" "$INST/img.tar" 2>/dev/null || cp "$IMAGE_TAR" "$INST/img.tar"
cat > "$INST/lever.yaml" <<EOF
name: $NAME
scion:
  version: $SCION_VERSION
backend: lima
cpus: 4
memory: 6GiB
egress: open
tree: workspace
manager:
  image: $IMAGE
  image_tar: img.tar
  prompt_file: manager.md
  allow_ports: []
workers:
  - name: worker
    dir: workers/worker
    obtain: []
broker:
  jail_port: $JP
  admin_port: $AP
  llm_auth: api-key
  api_key_file: console-api-key
EOF

# lv <log> <secs> <args...>: one lever command in the instance, bounded.
lv() { local log=$1 secs=$2; shift 2; (cd "$INST" && timeout "$secs" "$LEVER" "$@" > "$log" 2>&1); }
upna() { lv "$1" 1800 up --no-attach; }
lockfile="$INST/.lever-state/conversation.lock"
# lock_free: no process holds the lifecycle lock.
lock_free() { python3 -c 'import fcntl,sys; f=open(sys.argv[1],"a"); fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)' "$lockfile" 2>/dev/null; }
# ticket: the staged enrolment ticket's hash, "none" without one. The tree is
# the manager's, so a ticket there is one the agent can read.
ticket() {
	local f="$INST/workspace/.lever/bootstrap.json"
	if [ -e "$f" ]; then python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest()[:16])' "$f" || echo unreadable; else echo none; fi
}
# mgrid: the hub record id of the manager ("none" with no record). A changed
# id means the record was deleted and created again.
mgrid() {
	hubbody "/api/v1/agents?projectId=$PROJ" | python3 -c 'import json,sys
a = [x for x in json.load(sys.stdin).get("agents", []) if x.get("slug") == sys.argv[1]]
print(a[0].get("id", "") if a else "none")' "$NAME"
}
started() { X inspect -f '{{.State.StartedAt}}' "lever--$NAME" 2>/dev/null; }
old_transcript() { P exec --user scion "lever--$NAME" test -s /home/scion/.claude/projects/-workspace/chaos-old.jsonl; }
claude_soon() { local _; for _ in $(seq 1 30); do claude_up && return 0; sleep 2; done; return 1; }
# fits <secs> <scenario>: the scenario is selected and fits what is left of
# the budget; otherwise it is reported as skipped.
fits() {
	if [ -n "$ONLY" ] && ! [[ "$2" =~ $ONLY ]]; then return 1; fi
	if [ $((BUDGET - SECONDS)) -lt "$1" ]; then skip "$2 (needs ~${1}s, $((BUDGET - SECONDS))s of the budget left)"; return 1; fi
	return 0
}
# holder_named <log>: lever's lock refusal, naming the command that holds it.
holder_named() { grep -qE '`[^`]+` \(pid [0-9]+, since [0-9:]+ UTC\) is changing this instance.s manager; nothing was changed' "$1"; }

# converged <what> <log> <id>: a plain up succeeded and left the manager
# running on the same record, with its old transcript, and the lock free.
converged() {
	local what=$1 log=$2 id=$3 rc
	upna "$log"; rc=$?
	if [ "$rc" != 0 ]; then tail -6 "$log" | mask_secrets | sed 's/^/      | /'; fi
	checkx "$what: the next up converges ($rc)" '[ "$rc" = 0 ]'
	check "$what: the manager runs" running
	checkx "$what: the same manager record ($(mgrid))" '[ "$(mgrid)" = "$id" ]'
	checkx "$what: nothing was started fresh" '! grep -q "starting FRESH" "$log"'
	check "$what: the old transcript is in the home" old_transcript
	check "$what: claude runs" claude_soon
	check "$what: the lifecycle lock is free" lock_free
}

say "clean slate"
slate=1
limactl delete --force "$VM" >/dev/null 2>&1 && echo "deleted stale $VM"
(cd "$INST" && "$LEVER" init >/dev/null 2>&1)

say "lever up"
upna "$WORK/up.log"; rc=$?
tail -12 "$WORK/up.log" | mask_secrets | sed 's/^/      | /'
checkx "the first up succeeds ($rc)" '[ "$rc" = 0 ]'
if running; then ok "the manager runs"; else bad "the manager runs"; exit 1; fi
PROJ=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["project_id"])' "$INST/.lever-state/hub-identity.json")
lv "$WORK/wstart.log" 600 worker start worker --task-file "$WORK/task.txt" --json; rc=$?
checkx "the worker starts ($rc)" '[ "$rc" = 0 ]'
P exec --user scion "lever--$NAME" sh -c 'mkdir -p /home/scion/.claude/projects/-workspace && echo chaos-old > /home/scion/.claude/projects/-workspace/chaos-old.jsonl'
ID=$(mgrid)
checkx "the manager has a hub record ($ID)" '[ -n "$ID" ] && [ "$ID" != none ]'

if fits 120 "held lock"; then
	say "every command refuses at once under a held lock, and stages nothing"
	python3 -c 'import fcntl,os,sys,time
f = open(sys.argv[1], "a+"); fcntl.flock(f, fcntl.LOCK_EX); f.truncate(0)
f.write("`chaos holder` (pid %d, since %s)" % (os.getpid(), time.strftime("%H:%M:%S UTC", time.gmtime()))); f.flush()
open(sys.argv[2], "w").close(); time.sleep(300)' "$lockfile" "$WORK/held" &
	holder=$!
	for _ in $(seq 100); do [ -e "$WORK/held" ] && break; sleep 0.1; done
	if [ ! -e "$WORK/held" ]; then
		bad "the test's holder took the lifecycle lock (the held-lock checks did not run)"
	else
		t0=$(ticket); s0=$(started)
		for c in "up --no-attach" "apply" "stop --keep-vm" "reload" "conversation new --force" "conversation backup"; do
			# shellcheck disable=SC2086 # c is a command and its flags
			lv "$WORK/held-cmd.log" 120 $c; rc=$?
			checkx "lever $c exits 6 ($rc)" '[ "$rc" = 6 ]'
			checkx "lever $c names the holder" 'grep -q "chaos holder" "$WORK/held-cmd.log" && holder_named "$WORK/held-cmd.log"'
			checkx "lever $c staged no ticket ($(ticket))" '[ "$(ticket)" = "$t0" ]'
		done
		checkx "the manager container was not touched" '[ "$(started)" = "$s0" ]'
	fi
	kill "$holder" 2>/dev/null; wait "$holder" 2>/dev/null
	check "the lifecycle lock is free after the holder" lock_free
fi

# cmd <key>: the lever arguments of a race participant.
cmd() {
	case $1 in
	up) echo "up --no-attach" ;;
	apply) echo "apply" ;;
	stop) echo "stop --keep-vm" ;;
	conv) echo "conversation new --force" ;;
	wresume) echo "worker resume worker --json" ;;
	wnew) echo "worker new-session worker --json --task-file $WORK/task.txt" ;;
	esac
}
# stages <key>: the command may stage a manager enrolment ticket.
stages() { [ "$1" = up ] || [ "$1" = apply ]; }
# outcome <key> <log> <rc> <other>: ok, refused (cleanly) or bad.
outcome() {
	local key=$1 log=$2 rc=$3 other=$4 allowed="busy no-operator-socket runtime-unavailable"
	# A stop takes the hub and the broker away under a worker verb.
	[ "$other" = stop ] && allowed="$allowed hub-refused not-live runtime-error"
	[ "$rc" = 0 ] && { echo ok; return; }
	case $key in
	wresume|wnew)
		# A worker verb refuses with its reason in one JSON line. Only the
		# reasons a race can bring about count: a role, revocation or
		# workspace refusal is a failure whatever ran alongside.
		if python3 -c 'import json,sys
for l in open(sys.argv[1]):
    l = l.strip()
    if l.startswith("{"):
        d = json.loads(l)
        sys.exit(0 if d.get("ok") is False and d.get("reason") in sys.argv[3].split() and d.get("exit") == int(sys.argv[2]) else 1)
sys.exit(1)' "$log" "$rc" "$allowed" 2>/dev/null; then echo refused; else echo bad; fi ;;
	*)
		if [ "$rc" = 6 ] && holder_named "$log"; then echo refused; else echo bad; fi ;;
	esac
}

# race <a> <b>: a and b at once (b CHAOS_OFFSET later), then the checks.
race() {
	local a=$1 b=$2 la="$WORK/race-$1-$2.a.log" lb="$WORK/race-$1-$2.b.log" t0 pa pb ra rb oa ob
	say "race: $a against $b"
	t0=$(ticket)
	# shellcheck disable=SC2046 # cmd's words are the arguments
	(cd "$INST" && exec timeout 900 "$LEVER" $(cmd "$a") > "$la" 2>&1) &
	pa=$!
	sleep "$OFFSET"
	# shellcheck disable=SC2046
	(cd "$INST" && exec timeout 900 "$LEVER" $(cmd "$b") > "$lb" 2>&1) &
	pb=$!
	reap "$pa" 900; ra=$?
	reap "$pb" 900; rb=$?
	oa=$(outcome "$a" "$la" "$ra" "$b"); ob=$(outcome "$b" "$lb" "$rb" "$a")
	echo "      | $a: $oa ($ra): $(tail -1 "$la" | mask_secrets | cut -c1-200)"
	echo "      | $b: $ob ($rb): $(tail -1 "$lb" | mask_secrets | cut -c1-200)"
	checkx "$a/$b: each ends done or refused cleanly ($oa, $ob)" '[ "$oa" != bad ] && [ "$ob" != bad ]'
	checkx "$a/$b: at most one refusal" '! { [ "$oa" = refused ] && [ "$ob" = refused ]; }'
	if ! stages "$a" && ! stages "$b"; then
		checkx "$a/$b: no ticket staged ($(ticket))" '[ "$(ticket)" = "$t0" ]'
	fi
	check "$a/$b: the lifecycle lock is free" lock_free
	converged "$a/$b" "$WORK/race-$a-$b.up.log" "$ID"
	lv "$WORK/race-$a-$b.w.log" 600 worker resume worker --json; rc=$?
	checkx "$a/$b: the worker resumes ($rc)" '[ "$rc" = 0 ]'
}

# reap <pid> <secs>: pid's exit status, stopping it (timeout passes the
# signal on to lever) once secs have passed.
reap() {
	local pid=$1 secs=$2 i
	for ((i = 0; i < secs * 2; i++)); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
	kill -0 "$pid" 2>/dev/null && kill "$pid" 2>/dev/null
	wait "$pid"
}

# step_started <log> <step>: lever's plain progress shows <step> begun.
step_started() { grep -qE "\] $2 \.\.\.( |$)" "$1"; }
# fault <kind> <step> <delay>: start a lever up that has work to do (after
# stop --keep-vm), and once <step> has begun and <delay> seconds passed,
# kill <kind>; then the next up must converge.
fault() {
	local kind=$1 step=$2 delay=$3 log="$WORK/fault-$1-$2.log" pid rc reached=0 i killed
	say "fault: kill the $kind once $step has begun (+${delay}s)"
	lv "$WORK/fault-stop.log" 900 stop --keep-vm || { bad "$kind@$step: stop --keep-vm before the fault"; return; }
	(cd "$INST" && exec timeout 1800 "$LEVER" up --no-attach > "$log" 2>&1) &
	pid=$!
	for i in $(seq 1 1800); do
		if step_started "$log" "$step"; then reached=1; break; fi
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.5
	done
	if [ "$reached" = 0 ]; then
		kill "$pid" 2>/dev/null; reap "$pid" 60
		tail -5 "$log" | mask_secrets | sed 's/^/      | /'
		bad "$kind@$step: the up never reached $step (after ${i} looks)"
		converged "$kind@$step" "$WORK/fault-$kind-$step.up.log" "$ID"
		return
	fi
	sleep "$delay"
	# A fault after the up ended tests nothing: that is a failure of the
	# scenario, not a pass.
	if ! kill -0 "$pid" 2>/dev/null; then
		bad "$kind@$step: the up ended before the fault (${delay}s after $step began); lower the delay"
	else
		killed=1
		case $kind in
		hub) G sh -c 'p=$(tr -cd 0-9 < ~/.scion/server.pid); [ -n "$p" ] && kill -9 "$p"' || killed=0 ;;
		broker) pkill -9 -f "broker serve .*$INST/" || killed=0 ;;
		lever) pkill -9 -P "$pid" || killed=0 ;;
		guest) timeout 300 limactl stop -f "$VM" >/dev/null 2>&1 || killed=0 ;;
		esac
		if [ "$killed" = 1 ]; then ok "$kind@$step: killed the $kind"; else bad "$kind@$step: found no $kind to kill"; fi
	fi
	reap "$pid" 300; rc=$?
	echo "      | the faulted up ended with $rc: $(tail -1 "$log" | mask_secrets | cut -c1-200)"
	converged "$kind@$step" "$WORK/fault-$kind-$step.up.log" "$ID"
}

# The scenarios in order of value: the first ones run even on a slow host.
# Each estimate covers the scenario and the converging up after it.
for r in "up apply" "up conv" "stop up" "apply wresume" "conv wnew" "wresume wnew"; do
	# shellcheck disable=SC2086
	set -- $r
	if fits 240 "race $1/$2"; then race "$1" "$2"; fi
done
for f in "hub start-manager 2" "broker start-manager 1" "lever start-manager 3" "guest start-manager 2"; do
	# shellcheck disable=SC2086
	set -- $f
	if fits 420 "fault $1@$2"; then fault "$1" "$2" "$3"; fi
done
for r in "up stop" "up wresume" "up wnew" "apply stop" "apply conv" "apply wnew" "stop conv" "stop wresume" "stop wnew" "conv wresume"; do
	# shellcheck disable=SC2086
	set -- $r
	if fits 240 "race $1/$2"; then race "$1" "$2"; fi
done
for f in "hub hub-identity 0" "guest scion-server 0" "broker mint-manager-bootstrap 0"; do
	# shellcheck disable=SC2086
	set -- $f
	if fits 420 "fault $1@$2"; then fault "$1" "$2" "$3"; fi
done

say "doctor"
doctor_out=$(cd "$INST" && timeout 900 "$LEVER" doctor 2>&1)
printf '%s\n' "$doctor_out" | grep -E '^(✗|!) ' | mask_secrets | sed 's/^/      | /'
checkx "doctor: no failed row" '! printf "%s\n" "$doctor_out" | grep -q "^✗ "'

say "teardown"
(cd "$INST" && timeout 900 "$LEVER" destroy 2>&1 | mask_secrets | tail -1)
if limactl list --format '{{.Name}}' 2>/dev/null | grep -qx "$VM"; then bad "VM $VM still present after destroy"; else ok "VM torn down"; fi

end_phase
printf '\nphase times:\n%s' "$phase_times"
echo "total ${SECONDS}s of a ${BUDGET}s budget; $skipped scenario(s) skipped"
if [ "$fail" = 0 ]; then echo "chaos e2e: PASS"; else echo "chaos e2e: FAIL"; fi
exit "$fail"
