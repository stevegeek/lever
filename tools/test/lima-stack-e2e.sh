#!/usr/bin/env bash
# Live Lima e2e of the whole stack on the pinned scion: a real `lever up`
# with the agent image (lever-admin links the runtime broker and pins the
# experiments off), a manager and a worker, messages both ways, remote access
# for an operator and a contact, the experiment and broker fence, a contact
# removed while an agent loops the controller rename, an agent's image registry
# write undone by the next apply, `lever stop` and a resume, and doctor. `make test-lima-e2e` covers the jail and the broker
# without scion; this covers what a scion pin bump can break.
#
# Needs: Lima >= 2.0, Go, perl, python3. The agent image comes from IMAGE_TAR (a
# docker archive holding $IMAGE:<arch>) or, without it, is built inside the
# guest from the scion module at SCION_VERSION (the guest then needs to reach
# Debian, npm and Docker Hub). `go mod download` of that version must work on
# the host.
#
# Env: SCION_VERSION (default: the release pin), IMAGE_TAR, IMAGE (default
# scionlocal/lever-claude-slim), SAVE_IMAGE_TAR (copy the archive lever loaded
# there, e.g. for the hub auth e2e), LEVER (default ~/.local/bin/lever), NAME,
# KEEP=1 (leave the VM for inspection).
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NAME="${NAME:-stacke2e}"
VM="lever-${NAME}"
LEVER="${LEVER:-$HOME/.local/bin/lever}"
IMAGE="${IMAGE:-scionlocal/lever-claude-slim}"
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

# A short work dir: the broker's operator socket lives under the state dir,
# and a UNIX socket path is limited to about 100 bytes.
WORK="$(mktemp -d /tmp/ls.XXXXXX)"
INST="$WORK/i"
cleanup() {
	rc=$?
	if [ "${KEEP:-0}" = 1 ]; then echo "KEEP=1: leaving $VM and $WORK"; return; fi
	# By the instance path, not the binary's name: LEVER may name a binary not
	# called lever, and a daemon left behind keeps any lock this run holds.
	pkill -f "(broker|remote) serve .*$INST/" 2>/dev/null
	limactl delete --force "$VM" >/dev/null 2>&1
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

command -v limactl >/dev/null 2>&1 || { echo "limactl not found"; exit 1; }
[ -x "$LEVER" ] || { echo "$LEVER not found (make install)"; exit 1; }
case "$(uname -m)" in arm64|aarch64) ARCH=arm64 ;; *) ARCH=amd64 ;; esac
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
doctor_out=""
doctor_run() { doctor_out=$(cd "$INST" && timeout 900 "$LEVER" doctor 2>&1); }
doctor_row() { printf '%s\n' "$doctor_out" | grep -E "^(✓|✗|!|-) $1" | head -1; }
row_ok() { local r; r=$(doctor_row "$1"); echo "      | ${r:-(no row)}" | cut -c1-300; [[ "$r" == ✓* ]]; }
# up runs `lever apply`: `lever up` ends by attaching, which needs a terminal.
up() { (cd "$INST" && timeout 3600 "$LEVER" apply 2>&1 | tee "$WORK/up.log" | mask_secrets | tail -${1:-25}); }

say "clean slate"
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
	# podman names a local build localhost/<ref>; lever looks the archive up by
	# the docker form of manager.image.
	if G podman tag "$IMAGE:$ARCH" "docker.io/$IMAGE:$ARCH" &&
		G podman save --format docker-archive -o /tmp/stack-img.tar "docker.io/$IMAGE:$ARCH" >/dev/null 2>&1 &&
		limactl copy "$VM:/tmp/stack-img.tar" "$INST/img.tar" >/dev/null; then
		ok "agent image saved to an archive for lever to load"
	else
		bad "saving the agent image"; exit 1
	fi
	G rm -f /tmp/stack-img.tar
	IMAGE_LINE="  image_tar: img.tar"
	write_config "$IMAGE_LINE"
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
check "no dev token in the jail" G sh -c '! test -e ~/.scion/dev-token'

say "manager, worker and messages"
# X joins its arguments into one shell line, so no argument here holds a space.
mgr agent start worker --task probe | tail -2 | sed 's/^/      | /'
checkx "the manager lists the worker" 'mgr agent list | grep -q worker'
checkx "the manager messages the worker" 'X exec "lever--$NAME" lever-manager msg send --to worker stack-e2e-hello >/dev/null 2>&1'
checkx "the manager reads its inbox through the broker (notifications)" 'X exec "lever--$NAME" lever-manager msg list >/dev/null 2>&1'
checkx "lever msg send to the manager" '(cd "$INST" && "$LEVER" msg send --to "$NAME" "stack e2e host note" 2>&1) | grep -q Sent'
checkx "the controller PAT lists notifications (inbox:read)" '[ "$(hub GET /api/v1/notifications)" = 200 ]'

say "the experiment fence on the hub"
for p in /api/v1/conduit/grant-keys /api/v1/artifacts; do
	code=$(hub GET "$p")
	checkx "hub answers $p with 404 (experiment off): $code" '[ "$code" = 404 ]'
done

say "the experiment fence in the remote proxy"
code=$(pcode "$OPERATOR" "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "the operator has a live session through the proxy ($code)" '[ "$code" = 200 ]'
code=$(pcode "$CONTACT" "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "the contact has a live session through the proxy ($code)" '[ "$code" = 200 ]'
for login in "$OPERATOR" "$CONTACT"; do
	for p in /api/v1/conduit /api/v1/conduit/grant-keys /api/v1/artifacts /api/v1/artifacts/shared/x /api/v1/artifacts/view/x /api/v1/admin/conduit/grant-keys/rotate /auth/test-identity/redeem; do
		checkx "proxy refuses $p for $login" 'refused "$login" "http://127.0.0.1:$RP$p"'
	done
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

say "an agent points the hub's agent image registry at its own"
# The hub's registry route takes any loopback caller. On this scion it writes
# the database's endpoints section, which outranks settings.yaml and applies at
# the next dispatch, so only lever-admin's write in hub-identity undoes it.
cat > "$WORK/registry.py" <<'PY'
import json, os, urllib.error, urllib.request
tok = open(os.path.expanduser("~/.scion/scion-token")).read().strip()
ep = os.environ.get("SCION_HUB_ENDPOINT") or os.environ.get("SCION_HUB_URL")
req = urllib.request.Request(ep + "/api/v1/system/registry", data=json.dumps({"image_registry": "registry.invalid/agent-chosen"}).encode(), method="PUT",
                             headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
try:
    print(urllib.request.urlopen(req, timeout=10).status)
except urllib.error.HTTPError as e:
    print(e.code)
PY
limactl copy "$WORK/registry.py" "$VM:/tmp/registry.py"
X cp /tmp/registry.py "lever--$NAME:/tmp/registry.py"
# hubreg: image_registry in the hub database's endpoints section ("" with no row).
hubreg() {
	G python3 -c 'import json, os, sqlite3
c = sqlite3.connect("file:" + os.path.expanduser("~/.scion/hub.db") + "?mode=ro", uri=True)
r = c.execute("select value from hub_settings where section = ?", ("endpoints",)).fetchone()
print(json.loads(r[0]).get("image_registry", "") if r else "")'
}
code=$(X exec "lever--$NAME" python3 /tmp/registry.py 2>&1 | tail -1)
checkx "the agent's registry write reaches the hub ($code)" '[ "$code" = 200 ]'
reg=$(hubreg 2>&1)
checkx "the hub's endpoints section holds the agent's registry ($reg)" '[ "$reg" = registry.invalid/agent-chosen ]'
up 5
reg=$(hubreg 2>&1)
checkx "apply set the hub's registry back to scionlocal ($reg)" '[ "$reg" = scionlocal ]'
check "the manager runs after the registry repair" running

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

say "teardown"
(cd "$INST" && "$LEVER" destroy 2>&1 | tail -1)
if limactl list --format '{{.Name}}' 2>/dev/null | grep -qx "$VM"; then bad "VM $VM still present after destroy"; else ok "VM torn down"; fi

end_phase
printf '\nphase times:\n%s' "$phase_times"
if [ "$fail" = 0 ]; then echo "stack e2e: PASS"; else echo "stack e2e: FAIL"; fi
exit "$fail"
