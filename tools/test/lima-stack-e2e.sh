#!/usr/bin/env bash
# Live Lima e2e of the whole stack on the pinned scion: a real `lever up`
# with the agent image, a manager and a worker, messages both ways, remote
# access for an operator and a contact, the experiment fence, `lever stop` and
# a resume, and doctor. `make test-lima-e2e` covers the jail and the broker
# without scion; this covers what a scion pin bump can break.
#
# Needs: Lima >= 2.0, Go, perl, python3. The agent image comes from IMAGE_TAR (a
# docker archive holding $IMAGE:<arch>) or, without it, is built inside the
# guest from the scion module at SCION_VERSION (the guest then needs to reach
# Debian, npm and Docker Hub). `go mod download` of that version must work on
# the host.
#
# Env: SCION_VERSION (default: the release pin), IMAGE_TAR, IMAGE (default
# scionlocal/lever-claude-slim), LEVER (default ~/.local/bin/lever), NAME,
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
JP=$(pick); AP=$(pick); RP=$(pick); LP=$(pick)
while [ "$AP" = "$JP" ]; do AP=$(pick); done
while [ "$RP" = "$JP" ] || [ "$RP" = "$AP" ]; do RP=$(pick); done
while [ "$LP" = "$JP" ] || [ "$LP" = "$AP" ] || [ "$LP" = "$RP" ]; do LP=$(pick); done

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
# write_config <image_tar line>: lever loads the image from a docker archive,
# since the host may have no docker.
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
  login_port: $LP
  allowed_users:
    - $OPERATOR
    - { login: $CONTACT, tier: contact, agents: [$NAME] }
EOF
}
if [ -n "${IMAGE_TAR:-}" ]; then
	ln -f "$IMAGE_TAR" "$INST/img.tar" 2>/dev/null || cp "$IMAGE_TAR" "$INST/img.tar"
	write_config "  image_tar: img.tar"
else
	write_config ""
fi

G() { limactl shell --workdir / "$VM" -- "$@"; }
X() { G sh -c "XDG_RUNTIME_DIR=/run/user/\$(id -u) podman $*"; }
running() { X ps --format '{{.Names}}' | grep -qx "lever--$NAME"; }
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
up() { (cd "$INST" && timeout 3600 "$LEVER" up 2>&1 | tee "$WORK/up.log" | mask_secrets | tail -${1:-25}); }

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
	write_config "  image_tar: img.tar"
fi

say "lever up on scion $SCION_VERSION"
up 80
if running; then ok "the manager runs"; else bad "the manager runs"; exit 1; fi
checkx "the jail's scion is the pin" 'G scion version 2>&1 | grep -q "$SCION_VERSION"'
G scion version 2>&1 | head -3 | sed 's/^/      | /'
rec="$INST/.lever-state/controller.pat.json"
checkx "the controller PAT has a recorded expiry" 'python3 -c "import json,sys,datetime as d; e=json.load(open(sys.argv[1])).get(\"expires_at\",\"\"); t=d.datetime.fromisoformat(e.replace(\"Z\",\"+00:00\")); n=d.datetime.now(d.timezone.utc); sys.exit(0 if d.timedelta(days=300)<t-n<d.timedelta(days=366) else 1)" "$rec"'
checkx "the controller PAT carries inbox:read" 'python3 -c "import json,sys; sys.exit(0 if \"inbox:read\" in json.load(open(sys.argv[1]))[\"requested_scopes\"] else 1)" "$rec"'

say "manager, worker and messages"
mgr agent start worker --task "stack e2e probe" | tail -2 | sed 's/^/      | /'
checkx "the manager lists the worker" 'mgr agent list | grep -q worker'
checkx "the manager messages the worker" 'X exec "lever--$NAME" lever-manager msg send --to worker "stack e2e hello" >/dev/null 2>&1'
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
for login in "$OPERATOR" "$CONTACT"; do
	for p in /api/v1/conduit /api/v1/conduit/grant-keys /api/v1/artifacts /api/v1/artifacts/shared/x /api/v1/artifacts/view/x /api/v1/admin/conduit/grant-keys/rotate /auth/test-identity/redeem; do
		checkx "proxy refuses $p for $login" 'refused "$login" "http://127.0.0.1:$RP$p"'
	done
	checkx "proxy refuses an experiments write for $login" \
		'refused "$login" -X PUT -H "Content-Type: application/json" --data "{\"overrides\":{\"hub.conduit\":true}}" "http://127.0.0.1:$RP/api/v1/admin/experiments"'
done

say "doctor"
doctor_run
printf '%s\n' "$doctor_out" | grep -E '^(✗|!) ' | mask_secrets | sed 's/^/      | /'
check "doctor: scion experiments" row_ok "scion conduit and artifacts"
check "doctor: remote access" row_ok "remote access"
check "doctor: remote web role" row_ok "remote web role"
check "doctor: dev user identity" row_ok "dev user identity"
checkx "doctor: no failed row" '! printf "%s\n" "$doctor_out" | grep -q "^✗ "'

say "lever stop, then lever up (agents resume)"
(cd "$INST" && timeout 900 "$LEVER" stop 2>&1 | tail -3)
up 15
check "the manager runs after the resume" running
checkx "the manager still lists the worker" 'mgr agent list | grep -q worker'
checkx "lever msg send after the resume" '(cd "$INST" && "$LEVER" msg send --to "$NAME" "stack e2e after resume" 2>&1) | grep -q Sent'
checkx "a re-apply opens no dev-auth window" '! grep -q "dev-auth" "$WORK/up.log"'

say "teardown"
(cd "$INST" && "$LEVER" destroy 2>&1 | tail -1)
if limactl list --format '{{.Name}}' 2>/dev/null | grep -qx "$VM"; then bad "VM $VM still present after destroy"; else ok "VM torn down"; fi

end_phase
printf '\nphase times:\n%s' "$phase_times"
if [ "$fail" = 0 ]; then echo "stack e2e: PASS"; else echo "stack e2e: FAIL"; fi
exit "$fail"
