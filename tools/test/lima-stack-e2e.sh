#!/usr/bin/env bash
# Live Lima e2e of the whole stack on the pinned scion: a real `lever up`
# with the agent image (lever-admin links the runtime broker and pins the
# experiments off), a manager and a worker, messages both ways, remote access
# for an operator and a contact, the experiment and broker fence, a contact
# removed while an agent loops the controller rename, an agent's image
# registry, runtime and gcloud settings writes (a worker still starts on lever's
# image, and the next apply undoes them), `lever stop` and a resume, `lever
# up` applying a config change, a warm `lever up`, `lever up --wait` under a
# held lock, `lever up` over a stale hub pid file (a live decoy and a dead
# pid), `lever conversation new` on the running manager, and doctor. `make test-lima-e2e` covers the jail and the broker
# without scion; this covers what a scion pin bump can break.
#
# Needs: Lima >= 2.0, Go, perl, python3. The agent image comes from IMAGE_TAR (a
# docker archive holding $IMAGE:<arch>) or, without it, is built inside the
# guest from the scion module at SCION_VERSION (the guest then needs to reach
# Debian, npm and Docker Hub). `go mod download` of that version must work on
# the host.
#
# Env: SCION_VERSION (default: the release pin), IMAGE_TAR, IMAGE (default
# lever.local/lever-claude-slim), SAVE_IMAGE_TAR (copy the archive lever loaded
# there, e.g. for the hub auth e2e), IMAGE_CACHE (an
# archive path: reuse the archive an earlier run saved there when the key file
# beside it matches this tree's in-jail fingerprint, pins, arch and image, else
# build the image and save it there), LEVER (default ~/.local/bin/lever),
# STACK_NAME (default stacke2e; must end in e2e), KEEP=1 (leave the VM for
# inspection).
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# Not NAME: an operator's shell may hold a NAME for a real instance, whose VM
# this run would delete.
NAME="${STACK_NAME:-stacke2e}"
case $NAME in
*e2e) ;;
*) echo "STACK_NAME=$NAME: the name must end in e2e (this run deletes the VM lever-<name>)"; exit 2 ;;
esac
VM="lever-${NAME}"
LEVER="${LEVER:-$HOME/.local/bin/lever}"
IMAGE="${IMAGE:-lever.local/lever-claude-slim}"
# AGENT_IMAGE is the name agents run $IMAGE under (internal/imageref.Agent).
AGENT_NAME="${IMAGE#lever.local/}"
SCION_VERSION="${SCION_VERSION:-$(sed -n 's/^const ReleasePin = "\(.*\)"/\1/p' "$REPO_ROOT/internal/provision/scionbin/release.go")}"
SCION_MODULE=github.com/GoogleCloudPlatform/scion
OPERATOR=operator@example.com
CONTACT=contact1@example.com

ts() { date -u +%H:%M:%SZ; }
fail=0
phase_name=""; phase_start=$SECONDS; phase_times=""
end_phase() {
	[ -n "$phase_name" ] || return 0
	d=$((SECONDS - phase_start))
	phase_times="$phase_times$(printf '  %5ds  %s' "$d" "$phase_name")
"
}
say() { end_phase; phase_name="$*"; phase_start=$SECONDS; printf '\n%s === %s ===\n' "$(ts)" "$*"; }
ok() { printf '%s   PASS: %s\n' "$(ts)" "$*"; }
bad() { printf '%s   FAIL: %s\n' "$(ts)" "$*"; fail=1; }
check() { local what=$1; shift; if "$@"; then ok "$what"; else bad "$what"; fi; }
# checkx evaluates a condition in this shell, so it sees the script's
# variables and functions.
checkx() { local what=$1; shift; if eval "$*"; then ok "$what"; else bad "$what"; fi; }
source <(sed -n '/^mask_secrets() {/,/^}/p' "$REPO_ROOT/tools/test/lima-e2e.sh")

command -v limactl >/dev/null 2>&1 || { echo "limactl not found"; exit 1; }
[ -x "$LEVER" ] || { echo "$LEVER not found (make install)"; exit 1; }
# A short work dir: the broker's operator socket lives under the state dir,
# and a UNIX socket path is limited to about 100 bytes.
WORK="$(mktemp -d /tmp/ls.XXXXXX)"
INST="$WORK/i"
# Set by the clean slate: until then the VM by this name is not this run's.
slate=0
cleanup() {
	rc=$?
	if [ "${KEEP:-0}" = 1 ]; then echo "KEEP=1: leaving $VM and $WORK"; return; fi
	# By the instance path, not the binary's name: LEVER may name a binary not
	# called lever, and a daemon left behind keeps any lock this run holds.
	pkill -f "(broker|remote) serve .*$INST/" 2>/dev/null
	[ "$slate" = 1 ] && limactl delete --force "$VM" >/dev/null 2>&1
	rm -rf "$WORK"
	exit $rc
}
trap cleanup EXIT
# The real Go toolchain first: lever runs `go` from the instance dir, where an
# asdf-style shim finds no .tool-versions.
export PATH="$(cd "$REPO_ROOT" && go env GOROOT)/bin:$PATH" LEVER_PROGRESS=plain

port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
pick() { local p=$((21000 + RANDOM % 8000)); while port_busy $p; do p=$((p + 1)); done; echo $p; }
JP=$(pick); AP=$(pick); RP=$(pick)
while [ "$AP" = "$JP" ]; do AP=$(pick); done
while [ "$RP" = "$JP" ] || [ "$RP" = "$AP" ]; do RP=$(pick); done

case "$(uname -m)" in arm64|aarch64) ARCH=arm64 ;; *) ARCH=amd64 ;; esac
AGENT_IMAGE="lever.local/$AGENT_NAME:$ARCH"
LEVER_VERSION="$(sed -n 's/^const Version = "\(.*\)"/\1/p' "$REPO_ROOT/internal/cli/root.go")"
JAIL_ABI="$(sed -n 's/^const JailABI = \([0-9]*\)$/\1/p' "$REPO_ROOT/internal/jailabi/jailabi.go")"

say "build (scion $SCION_VERSION, lever $LEVER_VERSION, jail ABI $JAIL_ABI, $ARCH)"
echo "lever: $("$LEVER" version 2>&1 | head -1)"
mkdir -p "$WORK/bin"
(cd "$REPO_ROOT" && GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -ldflags "-X github.com/stevegeek/lever/internal/agent.Version=$LEVER_VERSION" -o "$WORK/bin/lever-agent" ./cmd/lever-agent) || exit 1
export LEVER_AGENT_BIN="$WORK/bin/lever-agent"

mkdir -p "$INST/workspace" "$INST/workers/worker"
touch "$INST/workspace/.gitkeep" "$INST/workers/worker/.gitkeep"
echo "Manager for the stack e2e. Do nothing unless asked." > "$INST/manager.md"
printf 'sk-ant-FAKE-stack-%s' "$$" > "$INST/console-api-key"; chmod 600 "$INST/console-api-key"
# write_config <image_tar line> [extra allowed_users lines]: lever loads the
# image from a docker archive, since the host may have no docker.
write_config() {
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
$1
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
remote:
  enabled: true
  base_url: "https://stack.example.invalid"
  port: $RP
  allowed_users:
    - $OPERATOR
    - { login: $CONTACT, tier: contact, agents: [$NAME] }
${2:-}
EOF
}
# image_key: what the in-guest build depends on. The in-jail sources enter
# through fingerprint.txt, so a change the fingerprint test has not recorded
# yet reuses a stale archive.
image_key() {
	{
		echo "$SCION_VERSION $LEVER_VERSION $JAIL_ABI $ARCH $IMAGE"
		cat "$REPO_ROOT/internal/jailabi/fingerprint.txt" "$REPO_ROOT/image/scion-slim/Dockerfile"
	} | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())'
}
if [ -z "${IMAGE_TAR:-}" ] && [ -n "${IMAGE_CACHE:-}" ]; then
	if [ -f "$IMAGE_CACHE" ] && [ "$(cat "$IMAGE_CACHE.key" 2>/dev/null)" = "$(image_key)" ]; then
		echo "reusing the agent image archive $IMAGE_CACHE"
		IMAGE_TAR=$IMAGE_CACHE
	else
		echo "no reusable archive at $IMAGE_CACHE: building the agent image"
	fi
fi
IMAGE_LINE=""
if [ -n "${IMAGE_TAR:-}" ]; then
	ln -f "$IMAGE_TAR" "$INST/img.tar" 2>/dev/null || cp "$IMAGE_TAR" "$INST/img.tar"
	IMAGE_LINE="  image_tar: img.tar"
	write_config "$IMAGE_LINE"
else
	write_config ""
fi

G() { limactl shell --workdir / "$VM" -- "$@"; }
X() { G sh -c "XDG_RUNTIME_DIR=/run/user/\$(id -u) podman $*"; }
running() { X ps --format '{{.Names}}' | grep -qx "lever--$NAME"; }
# hubproxyenv: the running hub carries lever's proxy-mode env (matched in the
# guest; nothing else of its environment leaves the jail).
hubproxyenv() { G sh -c 'pid=$(tr -cd 0-9 < ~/.scion/server.pid); tr "\000" "\n" < /proc/$pid/environ | grep -qx SCION_SERVER_AUTH_MODE=proxy'; }
# hubbody <path>: a hub GET's body with the controller PAT (over stdin).
hubbody() { G sh -c 'read -r t; curl -sS -m 20 -H "Authorization: Bearer $t" "http://127.0.0.1:8080$1"' _ "$1" < "$INST/.lever-state/controller.pat"; }
mgr() { X exec "lever--$NAME" lever-manager "$@" 2>&1; }
# hub <method> <path> [body]: an HTTP status from the jail hub with the
# controller PAT, which goes over stdin, never argv.
hub() {
	local m=$1 p=$2 body=${3:-}
	G sh -c 'read -r t; curl -sS -m 20 -o /dev/null -w "%{http_code}" -X "$1" -H "Authorization: Bearer $t" -H "Content-Type: application/json" ${3:+--data "$3"} "http://127.0.0.1:8080$2"' _ "$m" "$p" "$body" < "$INST/.lever-state/controller.pat"
}
# proxy <login> <curl args...>: a request through the host proxy as that login.
proxy() {
	local login=$1; shift
	curl -sS -m 30 -H "Host: 127.0.0.1:$RP" -H "Tailscale-User-Login: $login" "$@"
}
pcode() { local login=$1; shift; proxy "$login" -o /dev/null -w '%{http_code}' "$@"; }
# refused <login> <curl args...>: a 403 from the proxy's own route fence, not
# from an earlier check or the hub.
refused() {
	local login=$1 out; shift
	out=$(proxy "$login" -w '\n%{http_code}' "$@")
	[ "${out##*$'\n'}" = 403 ] && [[ "$out" == *"does not forward this route"* ]]
}
mintrefused() {
	local login=$1 out; shift
	out=$(proxy "$login" -w '\n%{http_code}' "$@")
	[ "${out##*$'\n'}" = 403 ] && [[ "$out" == *"does not hand out hub credentials"* ]]
}
doctor_out=""
doctor_run() { doctor_out=$(cd "$INST" && timeout 900 "$LEVER" doctor 2>&1); }
doctor_row() { printf '%s\n' "$doctor_out" | grep -E "^(✓|✗|!|-) $1" | head -1; }
row_ok() { local r; r=$(doctor_row "$1"); echo "      | ${r:-(no row)}" | cut -c1-300; [[ "$r" == ✓* ]]; }
# up runs `lever apply`: `lever up` ends by attaching, which needs a terminal.
up() { (cd "$INST" && timeout 3600 "$LEVER" apply 2>&1 | tee "$WORK/up.log" | mask_secrets | tail -${1:-25}); }

say "clean slate"
slate=1
limactl delete --force "$VM" >/dev/null 2>&1 && echo "deleted stale $VM"
(cd "$INST" && "$LEVER" init >/dev/null 2>&1)

if [ -z "${IMAGE_TAR:-}" ]; then
	say "first lever up (stops at the image: none yet)"
	up 8
	limactl list --format '{{.Name}}' 2>/dev/null | grep -qx "$VM" || { bad "no VM after the first up"; exit 1; }

	say "build the slim agent image in the guest (scion $SCION_VERSION)"
	src=$( (cd / && go mod download -json "$SCION_MODULE@$SCION_VERSION") | sed -n 's/.*"Dir": "\(.*\)".*/\1/p')
	[ -n "$src" ] && [ -f "$src/go.mod" ] || { bad "go mod download $SCION_MODULE@$SCION_VERSION"; exit 1; }
	C="$WORK/ctx"; mkdir -p "$C/slim/bin" "$C/lever/bin" "$C/lever/scionhook"
	(cd "$src" && GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -trimpath -tags no_embed_web \
		-ldflags "-s -w -X $SCION_MODULE/cmd/sciontool/commands.Version=$SCION_VERSION -X $SCION_MODULE/cmd/sciontool/commands.Commit=$SCION_VERSION" \
		-o "$C/slim/bin/sciontool" ./cmd/sciontool &&
		GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -trimpath -tags no_embed_web \
			-ldflags "-s -w -X $SCION_MODULE/pkg/version.Version=$SCION_VERSION -X $SCION_MODULE/pkg/version.Commit=$SCION_VERSION" \
			-o "$C/slim/bin/scion" ./cmd/scion) || { bad "building sciontool and scion"; exit 1; }
	(cd "$REPO_ROOT" &&
		GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -ldflags "-X github.com/stevegeek/lever/internal/agent.Version=$LEVER_VERSION" -o "$C/lever/bin/lever-agent" ./cmd/lever-agent &&
		GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -ldflags "-X main.Version=$LEVER_VERSION" -o "$C/lever/bin/lever-tool-db" ./cmd/lever-tool-db &&
		GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -o "$C/lever/bin/lever-manager" ./cmd/lever-manager) || { bad "building lever's in-jail binaries"; exit 1; }
	cp "$REPO_ROOT/cmd/lever-agent/scionhook/pre-start" "$C/lever/scionhook/pre-start"; chmod 0755 "$C/lever/scionhook/pre-start"
	cp "$REPO_ROOT/image/lever-claude/Dockerfile" "$C/lever/Dockerfile"
	cp "$REPO_ROOT/image/scion-slim/Dockerfile" "$C/slim/Dockerfile"
	G rm -rf /tmp/stackctx
	limactl copy -r "$C" "$VM:/tmp/stackctx" >/dev/null || { bad "copying the image context"; exit 1; }
	if G bash -c "cd /tmp/stackctx/slim && podman build --platform linux/$ARCH --build-arg SCION_VERSION=$SCION_VERSION -t lever-scion-slim:$ARCH . >/tmp/stack-build.log 2>&1 &&
		cd /tmp/stackctx/lever && podman build --platform linux/$ARCH --build-arg SCION_BASE=lever-scion-slim:$ARCH --build-arg LEVER_VERSION=$LEVER_VERSION --build-arg LEVER_JAIL_ABI=$JAIL_ABI -t $IMAGE:$ARCH . >>/tmp/stack-build.log 2>&1"; then
		ok "agent image built in the guest"
	else
		G tail -n 20 /tmp/stack-build.log | sed 's/^/      | /'
		bad "agent image build"; exit 1
	fi
	G rm -rf /tmp/stackctx
	# podman names a local build of an unqualified ref localhost/<ref>; lever
	# looks the archive up by the docker form of manager.image.
	case "$IMAGE" in
	lever.local/*) names="$IMAGE:$ARCH" ;;
	*) names="docker.io/$IMAGE:$ARCH" ;;
	esac
	tagged=1
	for n in $names; do [ "$n" = "$IMAGE:$ARCH" ] || G podman tag "$IMAGE:$ARCH" "$n" || tagged=0; done
	if [ "$tagged" = 1 ] &&
		G podman save -m --format docker-archive -o /tmp/stack-img.tar $names >/dev/null 2>&1 &&
		limactl copy "$VM:/tmp/stack-img.tar" "$INST/img.tar" >/dev/null; then
		ok "agent image saved to an archive for lever to load"
	else
		bad "saving the agent image"; exit 1
	fi
	G rm -f /tmp/stack-img.tar
	IMAGE_LINE="  image_tar: img.tar"
	write_config "$IMAGE_LINE"
	if [ -n "${IMAGE_CACHE:-}" ]; then
		if cp "$INST/img.tar" "$IMAGE_CACHE.tmp" && mv "$IMAGE_CACHE.tmp" "$IMAGE_CACHE" && image_key > "$IMAGE_CACHE.key"; then
			ok "agent image archive cached at $IMAGE_CACHE"
		else
			bad "caching the image archive at $IMAGE_CACHE"
		fi
	fi
fi
if [ -n "${SAVE_IMAGE_TAR:-}" ]; then
	if cp "$INST/img.tar" "$SAVE_IMAGE_TAR"; then ok "agent image archive saved to $SAVE_IMAGE_TAR"; else bad "saving the image archive to $SAVE_IMAGE_TAR"; fi
fi

say "lever up on scion $SCION_VERSION"
up 80
if running; then ok "the manager runs"; else bad "the manager runs"; exit 1; fi
checkx "the jail runs scion $SCION_VERSION" 'grep -q "scion $SCION_VERSION ok" "$WORK/up.log"'
rec="$INST/.lever-state/controller.pat.json"
checkx "the controller PAT has a recorded expiry" 'python3 -c "import json,sys,datetime as d; e=json.load(open(sys.argv[1])).get(\"expires_at\",\"\"); t=d.datetime.fromisoformat(e.replace(\"Z\",\"+00:00\")); n=d.datetime.now(d.timezone.utc); sys.exit(0 if d.timedelta(days=300)<t-n<d.timedelta(days=366) else 1)" "$rec"'
checkx "the controller PAT carries inbox:read" 'python3 -c "import json,sys; sys.exit(0 if \"inbox:read\" in json.load(open(sys.argv[1]))[\"requested_scopes\"] else 1)" "$rec"'
cat > "$WORK/granted.py" <<'PY'
import json, sys
g = json.load(open(sys.argv[1])).get("granted_scopes") or []
want = ["skill:read", "skill:list", "template:read", "template:list", "harness_config:read", "harness_config:list", "inbox:read"]
missing = [w for w in want if w not in g]
print("      | granted:", ",".join(g), "missing:", ",".join(missing))
sys.exit(1 if missing else 0)
PY
check "the controller PAT was granted the baseline reads and inbox:read" python3 "$WORK/granted.py" "$rec"
check "the hub runs in proxy auth mode" hubproxyenv
check "the hub identity record says fresh" grep -q '"mode": *"fresh"' "$INST/.lever-state/hub-identity.json"
PROJ=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["project_id"])' "$INST/.lever-state/hub-identity.json")
checkx "the project lists the broker" 'hubbody "/api/v1/projects/$PROJ/providers" | grep -q brokerId'
check "lever-admin pinned the experiments off" grep -q '"off": *\[' "$INST/.lever-state/hub-experiments.json"
for x in hub.conduit hub.artifacts hub.chat_thread_groups federation.project_visitors; do
	check "lever-admin pinned $x off" python3 -c 'import json,sys; sys.exit(sys.argv[2] not in json.load(open(sys.argv[1])).get("off", []))' "$INST/.lever-state/hub-experiments.json" "$x"
done
check "no dev token in the jail" G sh -c '! test -e ~/.scion/dev-token'
cimage() { X inspect -f '{{.ImageName}}' "$1" 2>/dev/null; }
# The pull must fail, so its status is dropped: under pipefail it would fail
# the check even when the output says blocked.
checkx "the jail's podman refuses a pull from lever.local" '{ X pull lever.local/none:1 2>&1 || :; } | grep -qi blocked'
checkx "the manager runs lever's image under its lever.local name ($(cimage "lever--$NAME"))" '[ "$(cimage "lever--$NAME")" = "$AGENT_IMAGE" ]'
check "no registry called lever.local resolves in the guest" G sh -c '! getent hosts lever.local'

say "manager, worker and messages"
# X joins its arguments into one shell line, so no argument here holds a space.
mgr agent start worker --task probe | tail -2 | sed 's/^/      | /'
checkx "the manager lists the worker" 'mgr agent list | grep -q worker'
checkx "the manager messages the worker" 'X exec "lever--$NAME" lever-manager msg send --to worker stack-e2e-hello >/dev/null 2>&1'
checkx "the manager reads its inbox through the broker (notifications)" 'X exec "lever--$NAME" lever-manager msg list >/dev/null 2>&1'
checkx "lever msg send to the manager" '(cd "$INST" && "$LEVER" msg send --to "$NAME" "stack e2e host note" 2>&1) | grep -q Sent'
checkx "the controller PAT lists notifications (inbox:read)" '[ "$(hub GET /api/v1/notifications)" = 200 ]'

say "the experiment fence on the hub"
for p in /api/v1/conduit/grant-keys /api/v1/artifacts /api/v1/thread-groups; do
	code=$(hub GET "$p")
	checkx "hub answers $p with 404 (experiment off): $code" '[ "$code" = 404 ]'
done

say "the experiment fence in the remote proxy"
code=$(pcode "$OPERATOR" "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "the operator has a live session through the proxy ($code)" '[ "$code" = 200 ]'
code=$(pcode "$CONTACT" "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "the contact has a live session through the proxy ($code)" '[ "$code" = 200 ]'
for login in "$OPERATOR" "$CONTACT"; do
	for p in /api/v1/conduit /api/v1/conduit/grant-keys /api/v1/artifacts /api/v1/artifacts/shared/x /api/v1/artifacts/view/x /api/v1/admin/conduit/grant-keys/rotate /auth/test-identity/redeem /api/v1/thread-groups; do
		checkx "proxy refuses $p for $login" 'refused "$login" "http://127.0.0.1:$RP$p"'
	done
	checkx "proxy refuses a federation token for $login" \
		'mintrefused "$login" -X POST -H "Content-Type: application/json" --data "{\"audience\":\"https://peer.example\"}" "http://127.0.0.1:$RP/api/v1/auth/federation-token"'
	checkx "proxy refuses an experiments write for $login" \
		'refused "$login" -X PUT -H "Content-Type: application/json" --data "{\"overrides\":{\"hub.conduit\":true}}" "http://127.0.0.1:$RP/api/v1/admin/experiments"'
	checkx "proxy refuses a broker registration for $login" \
		'refused "$login" -X POST -H "Content-Type: application/json" --data "{\"name\":\"remote-broker\"}" "http://127.0.0.1:$RP/api/v1/brokers"'
done

say "doctor"
doctor_run
printf '%s\n' "$doctor_out" | grep -E '^(✗|!) ' | mask_secrets | sed 's/^/      | /'
check "doctor: scion conduit and artifacts" row_ok "scion conduit and artifacts"
check "doctor: remote access" row_ok "remote access"
check "doctor: hub authentication" row_ok "hub authentication"
check "doctor: hub identities" row_ok "hub identities"
check "doctor: remote web role (bound before any sign-in)" row_ok "remote web role"
checkx "doctor: no failed row" '! printf "%s\n" "$doctor_out" | grep -q "^✗ "'

say "a contact removed while an agent loops the controller rename, with a token due"
# Any agent can call the hub's workstation identity route, which renames scion's
# dev user. A fresh instance has none (the controller is lever-controller), so
# the loop must change nothing lever relies on: the apply mints the due token
# and succeeds, and the removal takes effect at the proxy and at the hub. The
# hub auth e2e runs the same step on a migrated hub, where the dev user is the
# controller.
write_config "$IMAGE_LINE" "    - { login: contact2@example.com, tier: contact, agents: [$NAME] }"
(cd "$INST" && timeout 1800 "$LEVER" reload 2>&1 | mask_secrets | tail -3)
code=$(pcode contact2@example.com "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "contact2 signs in before the removal ($code)" '[ "$code" = 200 ]'
cat > "$WORK/loop.py" <<'PY'
import json, os, time, urllib.request
tok = open(os.path.expanduser("~/.scion/scion-token")).read().strip()
ep = os.environ.get("SCION_HUB_ENDPOINT") or os.environ.get("SCION_HUB_URL")
end = time.time() + float(os.environ.get("LOOP_SECS", "600"))
n = 0
while time.time() < end:
    req = urllib.request.Request(ep + "/api/v1/system/identity", data=json.dumps({"email": "agent-chosen@example.org"}).encode(), method="PUT",
                                 headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5)
        n += 1
        if n == 1:
            open("/tmp/loop.ok", "w").write("1")
    except Exception:
        pass
    time.sleep(0.002)
PY
limactl copy "$WORK/loop.py" "$VM:/tmp/loop.py"
X cp /tmp/loop.py "lever--$NAME:/tmp/loop.py"
X exec -d -e LOOP_SECS=600 "lever--$NAME" python3 /tmp/loop.py
sleep 3
checkx "the agent's rename loop reaches the hub" 'X exec "lever--$NAME" test -e /tmp/loop.ok'
# Other recorded scopes make the mint urgent: a token only in its renew window
# would be left for a later apply with a warning.
python3 - "$rec" <<'PY'
import json, sys
p = sys.argv[1]; d = json.load(open(p))
d["requested_scopes"] = ["agent:manage"]
json.dump(d, open(p, "w"))
PY
ppid0=$(cat "$INST/.lever-state/remote.pid" 2>/dev/null)
write_config "$IMAGE_LINE"
(cd "$INST" && timeout 1800 "$LEVER" reload > "$WORK/race.log" 2>&1); race_rc=$?
tail -4 "$WORK/race.log" | mask_secrets
code=$(pcode contact2@example.com "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "the removed contact is refused under the rename loop ($code)" '[ "$code" = 403 ]'
doctor_run
check "doctor: the removed contact's hub user is suspended under the loop" row_ok "remote web role"
checkx "the apply under the loop succeeds (no dev user to contest) ($race_rc)" '[ "$race_rc" = 0 ]'
checkx "it re-minted the due controller token" 'python3 -c "import json,sys; sys.exit(0 if \"inbox:read\" in json.load(open(sys.argv[1]))[\"requested_scopes\"] else 1)" "$rec"'
check "doctor: hub identities under the loop (the controller untouched)" row_ok "hub identities"
X exec "lever--$NAME" pkill -f loop.py
sleep 2
up 5
check "the manager runs after the loop stops" running
checkx "the controller keeps its agents: the manager still lists the worker" 'mgr agent list | grep -q worker'
doctor_run
check "after the loop stops, doctor: hub identities" row_ok "hub identities"
check "after the loop stops, doctor: remote web role" row_ok "remote web role"

say "lever-admin links the broker back to a project left without one"
# The controller token may unlink a broker (project.update) but never link one;
# scion's auto-provide linked it when the project was created.
BROKER=$(hubbody "/api/v1/projects/$PROJ/providers" | python3 -c 'import json,sys; print(json.load(sys.stdin)["providers"][0]["brokerId"])')
code=$(hub DELETE "/api/v1/projects/$PROJ/providers/$BROKER")
checkx "the project's broker unlinked ($code)" '[ "$code" = 200 ] || [ "$code" = 204 ]'
checkx "the project lists no broker" '! hubbody "/api/v1/projects/$PROJ/providers" | grep -q brokerId'
up 8
check "lever-admin linked the jail's runtime broker" grep -q "linking the jail's runtime broker .* as lever-admin" "$WORK/up.log"
checkx "the project lists the broker again" 'hubbody "/api/v1/projects/$PROJ/providers" | grep -q "$BROKER"'
check "the manager runs after the re-link" running

say "an agent points the hub's image registry and runtime at its own, and turns on gcloud injection"
# These hub routes take any loopback caller. On this scion the registry and
# runtime land in the database, which outranks settings.yaml and applies at the
# next dispatch. lever's agent images carry a registry host, so the registry
# write cannot redirect a start; each apply also sets all three back.
cat > "$WORK/call.py" <<'PY'
import json, os, sys, urllib.error, urllib.request
tok = open(os.path.expanduser("~/.scion/scion-token")).read().strip()
ep = os.environ.get("SCION_HUB_ENDPOINT") or os.environ.get("SCION_HUB_URL")
req = urllib.request.Request(ep + sys.argv[2], data=sys.argv[3].encode(), method=sys.argv[1],
                             headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
try:
    print(urllib.request.urlopen(req, timeout=10).status)
except urllib.error.HTTPError as e:
    print(e.code)
PY
limactl copy "$WORK/call.py" "$VM:/tmp/call.py"
X cp /tmp/call.py "lever--$NAME:/tmp/call.py"
# agentcall <method> <path> <json>: the hub's status for the manager's call.
# X joins its arguments into one shell line, so the JSON is quoted for it.
agentcall() { X exec "lever--$NAME" python3 /tmp/call.py "$1" "$2" "'$3'" 2>&1 | tail -1; }
# hubreg: image_registry in the hub database's endpoints section ("" with no row).
hubreg() {
	G python3 -c 'import json, os, sqlite3
c = sqlite3.connect("file:" + os.path.expanduser("~/.scion/hub.db") + "?mode=ro", uri=True)
r = c.execute("select value from hub_settings where section = ?", ("endpoints",)).fetchone()
print(json.loads(r[0]).get("image_registry", "") if r else "")'
}
hubrt() { hubbody /api/v1/system/runtime | python3 -c 'import json,sys; print(json.load(sys.stdin).get("configured", ""))'; }
gcloudset() { G sh -c 'grep -q "^auto_inject_gcloud_adc:" ~/.scion/settings.yaml'; }
checkx "the runtime is podman before the writes ($(hubrt))" '[ "$(hubrt)" = podman ]'
code=$(agentcall PUT /api/v1/system/registry '{"image_registry":"registry.invalid/agent-chosen"}')
checkx "the agent's registry write reaches the hub ($code)" '[ "$code" = 200 ]'
reg=$(hubreg 2>&1)
checkx "the hub's endpoints section holds the agent's registry ($reg)" '[ "$reg" = registry.invalid/agent-chosen ]'
mgr agent stop worker | tail -1 | sed 's/^/      | /'
checkx "the worker resumes under the agent's registry" 'mgr agent resume worker | grep -qE "running|resumed"'
checkx "the worker runs lever's image, not the agent's registry ($(cimage lever--worker))" '[ "$(cimage lever--worker)" = "$AGENT_IMAGE" ]'
code=$(agentcall PATCH /api/v1/system/workstation-settings '{"auto_inject_gcloud_adc":true}')
checkx "the agent's gcloud injection write reaches the hub ($code)" '[ "$code" = 200 ]'
check "settings.yaml carries auto_inject_gcloud_adc" gcloudset
code=$(agentcall PUT /api/v1/system/runtime '{"runtime":"docker"}')
checkx "the agent's runtime write reaches the hub ($code)" '[ "$code" = 200 ]'
checkx "the hub's runtime is the agent's ($(hubrt))" '[ "$(hubrt)" = docker ]'
doctor_run
checkx "doctor: hub container runtime fails on the agent's runtime" '! row_ok "hub container runtime"'
up 5
reg=$(hubreg 2>&1)
checkx "apply set the hub's registry back to lever.local ($reg)" '[ "$reg" = lever.local ]'
checkx "apply set the hub's runtime back to podman ($(hubrt))" '[ "$(hubrt)" = podman ]'
checkx "apply removed auto_inject_gcloud_adc" '! gcloudset'
check "the manager runs after the repair" running
mgr agent stop worker | tail -1 | sed 's/^/      | /'
checkx "the worker resumes after the runtime repair" 'mgr agent resume worker | grep -qE "running|resumed"'
doctor_run
check "doctor: hub container runtime" row_ok "hub container runtime"
check "doctor: manager image" row_ok "manager image"

say "lever stop, then lever up (agents resume)"
(cd "$INST" && timeout 900 "$LEVER" stop 2>&1 | tail -3)
up 15
check "the manager runs after the resume" running
checkx "the manager still lists the worker" 'mgr agent list | grep -q worker'
checkx "lever msg send after the resume" '(cd "$INST" && "$LEVER" msg send --to "$NAME" "stack e2e after resume" 2>&1) | grep -q Sent'
checkx "the broker link survived the restart (no second link)" '! grep -q "linking the jail.s runtime broker" "$WORK/up.log"'
checkx "the manager resumes the suspended worker" 'mgr agent resume worker | grep -qE "running|resumed"'
doctor_run
printf '%s\n' "$doctor_out" | grep -E '^(✗|!) ' | mask_secrets | sed 's/^/      | /'
check "doctor: remote web role bound after the sign-ins" row_ok "remote web role"
check "doctor: scion conduit and artifacts after the resume" row_ok "scion conduit and artifacts"
checkx "doctor: no failed row after the resume" '! printf "%s\n" "$doctor_out" | grep -q "^✗ "'

say "lever up applies a config change, with no lever apply"
# upna <log> [args...]: `lever up --no-attach`, which runs the whole plan.
upna() { local log=$1; shift; (cd "$INST" && timeout 1800 "$LEVER" up --no-attach "$@" > "$log" 2>&1); }
CONTACT2=contact2@example.com
ppid0=$(cat "$INST/.lever-state/remote.pid" 2>/dev/null)
write_config "$IMAGE_LINE" "    - { login: $CONTACT2, tier: contact, agents: [$NAME] }"
upna "$WORK/up-config.log"; rc=$?
tail -4 "$WORK/up-config.log" | mask_secrets | sed 's/^/      | /'
checkx "up with an edited config succeeds ($rc)" '[ "$rc" = 0 ]'
check "up says the config changed since the last bring-up" grep -q "the config changed since the last bring-up" "$WORK/up-config.log"
checkx "up invited the new contact" 'python3 -c "import json,sys; sys.exit(0 if json.load(open(sys.argv[1]))[\"users\"].get(sys.argv[2]) else 1)" "$INST/.lever-state/hub-identity.json" "$CONTACT2"'
checkx "up restarted the proxy on the new config" '[ "$ppid0" != "$(cat "$INST/.lever-state/remote.pid")" ]'
code=$(pcode "$CONTACT2" "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "the new contact signs in through the proxy ($code)" '[ "$code" = 200 ]'
write_config "$IMAGE_LINE"
upna "$WORK/up-config2.log"; rc=$?
checkx "up with the contact removed succeeds ($rc)" '[ "$rc" = 0 ]'
code=$(pcode "$CONTACT2" "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "the removed contact is refused ($code)" '[ "$code" = 403 ]'
check "the manager still runs" running

say "a warm lever up changes nothing"
hpid0=$(G sh -c 'tr -cd 0-9 < ~/.scion/server.pid')
mstart0=$(X inspect -f '{{.State.StartedAt}}' "lever--$NAME")
ppid0=$(cat "$INST/.lever-state/remote.pid" 2>/dev/null)
t0=$SECONDS
upna "$WORK/up-warm.log"; rc=$?
echo "      | warm up took $((SECONDS - t0))s"
checkx "a warm up succeeds ($rc)" '[ "$rc" = 0 ]'
check "it skips the warm-up a finished one covers" grep -q "already loaded and warmed" "$WORK/up-warm.log"
checkx "it says nothing changed" '! grep -q "changed since the last bring-up" "$WORK/up-warm.log"'
checkx "the hub was not restarted" '[ "$hpid0" = "$(G sh -c "tr -cd 0-9 < ~/.scion/server.pid")" ]'
checkx "the manager container was not restarted" '[ "$mstart0" = "$(X inspect -f "{{.State.StartedAt}}" "lever--$NAME")" ]'
checkx "the proxy was kept" '[ "$ppid0" = "$(cat "$INST/.lever-state/remote.pid")" ]'

say "lever up --wait waits out a held lifecycle lock"
python3 -c 'import fcntl,sys,time; f=open(sys.argv[1],"a"); fcntl.flock(f,fcntl.LOCK_EX); open(sys.argv[2],"w").close(); time.sleep(15)' \
	"$INST/.lever-state/conversation.lock" "$WORK/held-wait" &
holder=$!
for _ in $(seq 300); do [ -e "$WORK/held-wait" ] && break; sleep 0.1; done
if [ ! -e "$WORK/held-wait" ]; then
	# Without the lock held, the checks below would test nothing.
	bad "the test's lock holder took the lifecycle lock within 30s (the --wait checks did not run)"
	kill "$holder" 2>/dev/null
	wait "$holder" 2>/dev/null
else
	upna "$WORK/up-locked.log"; rc=$?
	checkx "up under a held lock exits 6 at once ($rc)" '[ "$rc" = 6 ]'
	upna "$WORK/up-wait.log" --wait 3m; rc=$?
	wait "$holder" 2>/dev/null
	checkx "up --wait runs once the holder lets go ($rc)" '[ "$rc" = 0 ]'
	check "up --wait said what it waited for" grep -q "waiting up to 3m0s" "$WORK/up-wait.log"
fi

say "a stale hub pid file naming a live process, then lever up"
# After a guest reboot server.pid names the old hub's pid, and the pid
# sequence repeats, so a new process can hold it. A decoy holds it here.
(cd "$INST" && timeout 900 "$LEVER" stop --keep-vm 2>&1 | tail -2)
checkx "the hub is down after stop --keep-vm" '! G curl -sf -m 5 -o /dev/null http://127.0.0.1:8080/healthz'
decoy=$(G sh -c 'nohup setsid sleep 1800 </dev/null >/dev/null 2>&1 & echo $! | tee ~/.scion/server.pid')
checkx "a decoy holds the pid server.pid names ($decoy)" 'G kill -0 "$decoy"'
upna "$WORK/up-decoy.log"; rc=$?
tail -4 "$WORK/up-decoy.log" | mask_secrets | sed 's/^/      | /'
checkx "up over the decoy succeeds ($rc)" '[ "$rc" = 0 ]'
hpid=$(G sh -c 'tr -cd 0-9 < ~/.scion/server.pid')
checkx "server.pid names a new hub, not the decoy ($hpid)" '[ -n "$hpid" ] && [ "$hpid" != "$decoy" ]'
check "the hub serves" G curl -sf -m 10 -o /dev/null http://127.0.0.1:8080/healthz
checkx "lever left the decoy alone" 'G kill -0 "$decoy"'
G kill "$decoy" 2>/dev/null
check "the manager runs after the up" running
say "a stale hub pid file naming a dead process, then lever up"
(cd "$INST" && timeout 900 "$LEVER" stop --keep-vm 2>&1 | tail -2)
dead=$(G sh -c 'sleep 0 & p=$!; wait $p; echo $p | tee ~/.scion/server.pid')
upna "$WORK/up-dead.log"; rc=$?
checkx "up over a dead pid succeeds ($rc)" '[ "$rc" = 0 ]'
hpid=$(G sh -c 'tr -cd 0-9 < ~/.scion/server.pid')
checkx "server.pid names a new hub ($hpid, not $dead)" '[ -n "$hpid" ] && [ "$hpid" != "$dead" ]'
check "the hub serves after it" G curl -sf -m 10 -o /dev/null http://127.0.0.1:8080/healthz
check "the manager runs after it" running

say "lever conversation new on the running manager"
# P <args...>: podman in the guest, each argument passed as is (X joins them).
P() { G sh -c 'XDG_RUNTIME_DIR=/run/user/$(id -u) exec podman "$@"' _ "$@"; }
# mgrrec <field>: the manager's hub record field (phase, or task from its
# applied config); "none" with no record.
mgrrec() {
	hubbody "/api/v1/agents?projectId=$PROJ" | python3 -c 'import json,sys
a = [x for x in json.load(sys.stdin).get("agents", []) if x.get("slug") == sys.argv[1]]
if not a: print("none"); sys.exit()
print(a[0].get("phase", "") if sys.argv[2] == "phase" else (a[0].get("appliedConfig") or {}).get("task", ""))' "$NAME" "$1"
}
# claude_up: a claude process runs in the manager's container.
# cmdlines: every process's command line in the manager's container, one per line.
cmdlines() { P exec "lever--$NAME" sh -c 'for f in /proc/[0-9]*/cmdline; do tr "\000" " " < "$f"; echo; done' 2>/dev/null; }
# claude_up: a claude process runs in the manager's container. Claude Code runs as
# claude, as node with claude-code's cli, or behind scion's shell wrapper.
# The list is read whole first: under pipefail, grep -q quitting at the first
# match (claude starts early) leaves limactl writing to a closed pipe, and its
# SIGPIPE failed the check with claude running.
claude_up() { local c; c=$(cmdlines) || :; grep -qE '(^|[/ ])claude( |$)|@anthropic-ai/claude-code/' <<<"$c"; }
# newest: the newest transcript in the manager's home (its claude session id).
newest() { P exec --user scion "lever--$NAME" sh -c 'ls -t /home/scion/.claude/projects/*/*.jsonl 2>/dev/null | grep -v stack-e2e-old | head -1' 2>/dev/null; }
P exec --user scion "lever--$NAME" sh -c 'mkdir -p /home/scion/.claude/projects/-workspace && echo stack-e2e-old > /home/scion/.claude/projects/-workspace/stack-e2e-old.jsonl'
before=$(newest)
echo "      | session before: ${before:-(no transcript)}"
# Held from outside lever: every change to the manager record is refused, and
# names nothing it changed.
python3 -c 'import fcntl,sys,time; f=open(sys.argv[1],"a"); fcntl.flock(f,fcntl.LOCK_EX); open(sys.argv[2],"w").close(); time.sleep(120)' \
	"$INST/.lever-state/conversation.lock" "$WORK/held" &
holder=$!
for _ in $(seq 50); do [ -e "$WORK/held" ] && break; sleep 0.1; done
(cd "$INST" && timeout 300 "$LEVER" conversation new > "$WORK/conv-locked.log" 2>&1); rc=$?
checkx "conversation new under a held lock exits 6 ($rc)" '[ "$rc" = 6 ]'
(cd "$INST" && timeout 300 "$LEVER" stop > "$WORK/stop-locked.log" 2>&1); rc=$?
checkx "lever stop under a held lock exits 6 ($rc)" '[ "$rc" = 6 ]'
kill "$holder" 2>/dev/null; wait "$holder" 2>/dev/null
check "the manager still runs after the refusals" running
MARK="stack-e2e-boot-$$"
echo "Manager for the stack e2e ($MARK). Do nothing unless asked." > "$INST/manager.md"
: > "$WORK/conv-samples"
(while [ ! -e "$WORK/conv-done" ]; do
	ph=$(mgrrec phase 2>/dev/null); cl=no; claude_up && cl=yes
	echo "${ph:-unknown} $cl" >> "$WORK/conv-samples"
done) &
sampler=$!
# The manager here never idles (its API key is fake, so every turn retries);
# the busy refusal itself is unit-tested.
# GOPROXY=off (T0305): `lever up` built scion and its web UI for this pin, so
# nothing here may ask the Go module proxy; a `go mod download` would fail.
(cd "$INST" && GOPROXY=off timeout 900 "$LEVER" conversation new --force > "$WORK/conv-new.log" 2>&1); conv_rc=$?
touch "$WORK/conv-done"; wait "$sampler"
tail -3 "$WORK/conv-new.log" | mask_secrets | sed 's/^/      | /'
echo "      | phases seen: $(awk '{print $1"/"$2}' "$WORK/conv-samples" | uniq | tr '\n' ' ')"
checkx "conversation new succeeds ($conv_rc)" '[ "$conv_rc" = 0 ]'
bid=$(sed -n 's/.*the old one is backup \([^ ]*\) (.*/\1/p' "$WORK/conv-new.log" | head -1)
checkx "it names a backup (${bid:-none})" '[ -n "$bid" ]'
checkx "the backup is listed" '(cd "$INST" && "$LEVER" conversation list 2>&1) | grep -q "^$bid"'
# Against an honest manager: the phase is the agent's own report to the hub.
checkx "the hub never read the manager stopped with claude running" '! grep -q "^stopped yes" "$WORK/conv-samples"'
check "the manager runs on its new conversation" running
checkx "the hub reads the manager running ($(mgrrec phase))" '[ "$(mgrrec phase)" = running ]'
# The hub reads running as soon as the container starts; claude follows.
for _ in $(seq 1 30); do claude_up && break; sleep 2; done
check "claude runs in the manager's container" claude_up
claude_up || { echo "      | processes in the manager's container:"; cmdlines | cut -c1-160 | sed 's/^/      |   /'; }
after=$(newest)
echo "      | session after: ${after:-(no transcript)}"
checkx "a new claude session" '[ -n "$after" ] && [ "$after" != "$before" ]'
checkx "the old transcript stays in the home" 'P exec --user scion "lever--$NAME" test -s /home/scion/.claude/projects/-workspace/stack-e2e-old.jsonl'
checkx "the new session got the current boot prompt" 'P exec "lever--$NAME" sh -c "cat /proc/[0-9]*/cmdline 2>/dev/null" | tr "\000" " " | grep -q "$MARK"'
checkx "the hub stores the current boot prompt as the record's task" 'mgrrec task | grep -q "$MARK"'
doctor_run
printf '%s\n' "$doctor_out" | grep -E '^(✗|!) ' | mask_secrets | sed 's/^/      | /'
check "doctor: manager agent after the new conversation" row_ok "manager agent"
checkx "doctor: no failed row after the new conversation" '! printf "%s\n" "$doctor_out" | grep -q "^✗ "'

say "teardown"
(cd "$INST" && "$LEVER" destroy 2>&1 | tail -1)
if limactl list --format '{{.Name}}' 2>/dev/null | grep -qx "$VM"; then bad "VM $VM still present after destroy"; else ok "VM torn down"; fi

end_phase
printf '\nphase times:\n%s' "$phase_times"
if [ "$fail" = 0 ]; then echo "stack e2e: PASS"; else echo "stack e2e: FAIL"; fi
exit "$fail"
