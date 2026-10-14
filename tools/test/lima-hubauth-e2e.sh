#!/usr/bin/env bash
# Live Lima e2e for the hub's proxy auth (lever-admin, lever-controller,
# contacts bound before first sign-in). It brings an instance up with an
# OLDER lever (the dev-auth window flow), renames the dev user to
# lever-admin's address from the manager (the R1 hazard), migrates it with
# this lever while the manager and a worker run, adds and removes a contact
# while they run, removes one while the manager loops that rename with a token
# due, forces a controller PAT rotation, checks what an agent can
# and cannot do, and finally brings a fresh instance up with this lever. The
# migration also moves the jail from OLD_SCION_VERSION to SCION_VERSION, as an
# upgrade from the older release does; after it, and on the fresh instance,
# the manager starts a worker and the experiment fence is checked.
#
# Needs: Lima >= 2.0, Go, an agent image archive (IMAGE_TAR: a docker archive
# holding $IMAGE:<arch>, e.g. from `podman save` of `make lever-image-slim`'s
# output), and the older lever to migrate from (OLD_LEVER, or OLD_REF: a git
# ref of this repo built into the work dir; default: c288d646, the 0.38.0
# release, the last before proxy auth).
#
# The config keeps the image name an older lever.yaml uses (scionlocal/...):
# after the migration this lever starts agents on its lever.local/ name, and
# doctor flags the manager the older lever created on the unqualified one.
#
# Env: IMAGE_TAR (required), IMAGE (default scionlocal/lever-claude-slim),
# OLD_LEVER | OLD_REF, LEVER (default ~/.local/bin/lever), SCION_VERSION
# (default: the release pin), OLD_SCION_VERSION (the older lever's pin,
# default e0f61f2d), NAME, KEEP=1.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NAME="${NAME:-hubauthe2e}"
VM="lever-${NAME}"
LEVER="${LEVER:-$HOME/.local/bin/lever}"
IMAGE="${IMAGE:-scionlocal/lever-claude-slim}"
: "${IMAGE_TAR:?set IMAGE_TAR to a docker archive holding $IMAGE:<arch>}"
SCION_VERSION="${SCION_VERSION:-$(sed -n 's/^const ReleasePin = "\(.*\)"/\1/p' "$REPO_ROOT/internal/provision/scionbin/release.go")}"
OLD_SCION_VERSION="${OLD_SCION_VERSION:-e0f61f2d}"
CUR_SCION="$OLD_SCION_VERSION"

ts() { date -u +%H:%M:%SZ; }
fail=0
say() { printf '\n%s === %s ===\n' "$(ts)" "$*"; }
ok() { printf '%s   PASS: %s\n' "$(ts)" "$*"; }
bad() { printf '%s   FAIL: %s\n' "$(ts)" "$*"; fail=1; }
check() { local what=$1; shift; if "$@"; then ok "$what"; else bad "$what"; fi; }
# checkx evaluates a shell condition in this shell (variables and functions).
checkx() { local what=$1; shift; if eval "$*"; then ok "$what"; else bad "$what"; fi; }

# A short work dir: the broker's operator socket lives under the state dir,
# and a UNIX socket path is limited to about 100 bytes.
WORK="$(mktemp -d /tmp/lh.XXXXXX)"
INST="$WORK/i"
cleanup() {
	rc=$?
	if [ "${KEEP:-0}" = 1 ]; then echo "KEEP=1: leaving $VM and $WORK"; return; fi
	pkill -f "lever broker serve.*$INST" 2>/dev/null
	pkill -f "lever remote serve.*$INST" 2>/dev/null
	limactl delete --force "$VM" >/dev/null 2>&1
	rm -rf "$WORK"
	exit $rc
}
trap cleanup EXIT
export PATH="$(cd "$REPO_ROOT" && go env GOROOT)/bin:$PATH" LEVER_PROGRESS=plain

port_busy() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
pick() { local p=$((21000 + RANDOM % 8000)); while port_busy $p; do p=$((p + 1)); done; echo $p; }
JP=$(pick); AP=$(pick); RP=$(pick)
while [ "$AP" = "$JP" ]; do AP=$(pick); done
while [ "$RP" = "$JP" ] || [ "$RP" = "$AP" ]; do RP=$(pick); done

say "build"
if [ -z "${OLD_LEVER:-}" ]; then
	OLD_REF="${OLD_REF:-c288d646fd21c706db6d3ed1979d46ebcef446b1}"
	mkdir -p "$WORK/old"
	(cd "$REPO_ROOT" && git archive "$OLD_REF") | tar -x -C "$WORK/old" || { echo "git archive $OLD_REF failed"; exit 1; }
	(cd "$WORK/old" && go build -o "$WORK/lever-old" ./cmd/lever) || { echo "building the old lever failed"; exit 1; }
	OLD_LEVER="$WORK/lever-old"
fi
echo "old: $("$OLD_LEVER" version 2>&1 | head -1)"
echo "new: $("$LEVER" version 2>&1 | head -1)"
case "$(uname -m)" in arm64|aarch64) ARCH=arm64 ;; *) ARCH=amd64 ;; esac
mkdir -p "$WORK/bin"
(cd "$REPO_ROOT" && GOOS=linux GOARCH=$ARCH CGO_ENABLED=0 go build -o "$WORK/bin/lever-agent" ./cmd/lever-agent) || exit 1
export LEVER_AGENT_BIN="$WORK/bin/lever-agent"

mkdir -p "$INST/workspace" "$INST/workers/worker" "$INST/workers/worker2"
touch "$INST/workspace/.gitkeep" "$INST/workers/worker/.gitkeep" "$INST/workers/worker2/.gitkeep"
echo "Manager for the hub auth e2e. Do nothing unless asked." > "$INST/manager.md"
printf 'sk-ant-FAKE-hubauth-%s' "$$" > "$INST/console-api-key"; chmod 600 "$INST/console-api-key"
ln -f "$IMAGE_TAR" "$INST/img.tar" 2>/dev/null || cp "$IMAGE_TAR" "$INST/img.tar"
write_config() { # $1: extra allowed_users lines
	cat > "$INST/lever.yaml" <<EOF
name: $NAME
scion:
  version: $CUR_SCION
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
  - name: worker2
    dir: workers/worker2
    obtain: []
broker:
  jail_port: $JP
  admin_port: $AP
  llm_auth: api-key
  api_key_file: console-api-key
remote:
  enabled: true
  base_url: "https://hubauth.example.invalid"
  port: $RP
  allowed_users:
    - operator@example.com
    - { login: contact1@example.com, tier: contact, agents: [$NAME] }
$1
EOF
}
G() { limactl shell --workdir / "$VM" -- "$@"; }
X() { G sh -c "XDG_RUNTIME_DIR=/run/user/\$(id -u) podman $*"; }
hubpid() { G sh -c 'tr -cd 0-9 < ~/.scion/server.pid'; }
# hubproxyenv: the running hub carries lever's proxy-mode env (read and
# matched in the guest; nothing else of its environment leaves the jail).
hubproxyenv() { G sh -c 'pid=$(tr -cd 0-9 < ~/.scion/server.pid); tr "\000" "\n" < /proc/$pid/environ | grep -qx SCION_SERVER_AUTH_MODE=proxy'; }
doctor_row() { (cd "$INST" && timeout 600 "$LEVER" doctor 2>&1) | grep -E "^(✓|✗|!) $1 " | head -1; }
row_ok() { local r; r=$(doctor_row "$1"); echo "      | $r" | cut -c1-400; [[ "$r" == ✓* ]]; }
proxy() { # $1 login, rest curl args: a request through the host proxy as that login
	local login=$1; shift
	curl -sS -m 30 -H "Host: 127.0.0.1:$RP" -H "Tailscale-User-Login: $login" "$@"
}
running() { X ps --format '{{.Names}}' | grep -qx "lever--$NAME"; }
mgrlist() { X exec "lever--$NAME" lever-manager agent list 2>&1 | grep -q worker; }
# hubcode <path>: a hub GET's status with the controller PAT (over stdin).
hubcode() { G sh -c 'read -r t; curl -sS -m 20 -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $t" "http://127.0.0.1:8080$1"' _ "$1" < "$INST/.lever-state/controller.pat"; }
# fenced <login> <curl args...>: a 403 from the proxy's own route fence.
fenced() {
	local login=$1 out; shift
	out=$(proxy "$login" -w '\n%{http_code}' "$@")
	[ "${out##*$'\n'}" = 403 ] && [[ "$out" == *"does not forward this route"* ]]
}
# fence_checks <label>: scion's conduit and artifacts are off at the hub, lever
# recorded the pin, and the proxy refuses them and an experiments write.
fence_checks() {
	local p code
	for p in /api/v1/conduit/grant-keys /api/v1/artifacts; do
		code=$(hubcode "$p")
		checkx "$1: hub answers $p with 404 ($code)" '[ "$code" = 404 ]'
	done
	check "$1: lever-admin pinned the experiments off" grep -q '"off": *\[' "$INST/.lever-state/hub-experiments.json"
	checkx "$1: proxy refuses conduit" 'fenced operator@example.com "http://127.0.0.1:$RP/api/v1/conduit/grant-keys"'
	checkx "$1: proxy refuses artifacts" 'fenced operator@example.com "http://127.0.0.1:$RP/api/v1/artifacts"'
	checkx "$1: proxy refuses an experiments write" \
		'fenced operator@example.com -X PUT -H "Content-Type: application/json" --data "{\"overrides\":{\"hub.conduit\":true}}" "http://127.0.0.1:$RP/api/v1/admin/experiments"'
	check "$1: doctor: scion conduit and artifacts" row_ok "scion conduit and artifacts"
}
# start_worker2 <label>: the manager starts a worker through the broker as the
# controller, on the pinned scion.
start_worker2() {
	X exec "lever--$NAME" lever-manager agent start worker2 --task probe2 2>&1 | tail -1 | sed 's/^/      | /'
	checkx "$1: the manager starts worker2" 'X exec "lever--$NAME" lever-manager agent list 2>&1 | grep -q worker2'
}

say "old lever: apply (dev-auth window flow, scion $OLD_SCION_VERSION)"
write_config ""
(cd "$INST" && "$OLD_LEVER" init >/dev/null 2>&1)
for i in 1 2; do
	(cd "$INST" && timeout 3600 "$OLD_LEVER" apply 2>&1 | tail -15) && break
done
check "old flow: manager runs" running

say "old lever: the operator signs in and a dev-auth window caps it"
# The older lever binds and caps a login only after its first sign-in, from a
# dev-auth window, which opens only with no container running. Its ceiling
# names user.list, which the older scion's hub-member carried and the current
# one reserves: no hub user holds it, so none may delete that ceiling.
code=$(proxy operator@example.com -o /dev/null -w '%{http_code}' "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "old flow: the operator signs in through the older proxy ($code)" '[ "$code" = 200 ]'
(cd "$INST" && timeout 1800 "$OLD_LEVER" stop </dev/null 2>&1 | tail -2); stop_rc=$?
checkx "old flow: lever stop ($stop_rc)" '[ "$stop_rc" = 0 ]'
(cd "$INST" && timeout 3600 "$OLD_LEVER" up --no-attach </dev/null 2>&1 | tail -5); up_rc=$?
checkx "old flow: lever up ($up_rc)" '[ "$up_rc" = 0 ]'
# rrfield <key> [<email>]: a remote-role.json map entry, or a list joined by commas.
rrfield() {
	python3 -c 'import json,sys
v = json.load(open(sys.argv[1])).get(sys.argv[2]) or ""
print(v.get(sys.argv[3], "") if isinstance(v, dict) else ",".join(v))' "$INST/.lever-state/remote-role.json" "$@" 2>/dev/null
}
ceil0=$(rrfield ceilings operator@example.com)
checkx "old flow: the dev user capped the operator ($ceil0)" '[ -n "$ceil0" ]'
checkx "old flow: that ceiling names user.list" '[[ ",$(rrfield ceiling_permissions)," == *,user.list,* ]]'
check "old flow: manager runs after the window" running
X exec "lever--$NAME" lever-manager agent start worker --task probe 2>&1 | tail -1
X exec "lever--$NAME" lever-manager msg send --to worker pre-migration 2>&1 | tail -1

say "R1 setup: the manager renames the dev user to lever-admin's address"
cat > "$WORK/r1.py" <<'PY'
import json, os, urllib.request
tok = open(os.path.expanduser("~/.scion/scion-token")).read().strip()
ep = os.environ.get("SCION_HUB_ENDPOINT") or os.environ.get("SCION_HUB_URL")
req = urllib.request.Request(ep + "/api/v1/system/identity", data=json.dumps({"email": os.environ["E2E_EMAIL"]}).encode(), method="PUT",
                             headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
print(urllib.request.urlopen(req, timeout=20).status)
PY
limactl copy "$WORK/r1.py" "$VM:/tmp/r1.py"
X cp /tmp/r1.py "lever--$NAME:/tmp/r1.py"
checkx "an agent can rename the dev user (the hazard R1 repairs)" '[ "$(X exec -e E2E_EMAIL=lever-admin@lever.local "lever--$NAME" python3 /tmp/r1.py)" = 200 ]'

say "migrate with this lever (scion $SCION_VERSION) while the manager and the worker run"
pid0=$(hubpid)
mgr0=$(X inspect -f '{{.State.StartedAt}}' "lever--$NAME")
CUR_SCION="$SCION_VERSION"
write_config ""
(cd "$INST" && "$LEVER" init >/dev/null 2>&1)
(cd "$INST" && timeout 3600 "$LEVER" apply 2>&1 | tee "$WORK/migrate.log" | tail -25)
migrate_rc=$?
checkx "the migration apply succeeded ($migrate_rc)" '[ "$migrate_rc" = 0 ]'
pid1=$(hubpid)
checkx "the hub restarted once with lever's auth env" '[ -n "$pid1" ] && [ "$pid0" != "$pid1" ] && hubproxyenv'
checkx "the manager container was not restarted" '[ "$mgr0" = "$(X inspect -f "{{.State.StartedAt}}" "lever--$NAME")" ]'
check "R1 was repaired" grep -q "dev user carries lever-admin's address" "$WORK/migrate.log"
check "the dev user was demoted" grep -q "demoting hub admin" "$WORK/migrate.log"
check "the record says migrated" grep -q '"mode": *"migrated"' "$INST/.lever-state/hub-identity.json"
check "doctor: hub authentication" row_ok "hub authentication"
check "doctor: hub identities" row_ok "hub identities"
check "doctor: remote web role" row_ok "remote web role"
check "doctor: remote access" row_ok "remote access"
check "the older ceiling was rewritten in place" grep -qF "access constraint lever-remote-ceiling:operator@example.com ($ceil0) has permissions" "$WORK/migrate.log"
checkx "the operator keeps that one ceiling ($(rrfield ceilings operator@example.com))" '[ "$(rrfield ceilings operator@example.com)" = "$ceil0" ]'
checkx "the ceiling no longer names user.list" '[[ ",$(rrfield ceiling_permissions)," != *,user.list,* ]]'
check "no relaxation-authority refusal" sh -c '! grep -q insufficient_constraint_relaxation_authority "$1"' _ "$WORK/migrate.log"
check "broker path as the member controller: agent list" mgrlist
X exec "lever--$NAME" lever-manager agent stop worker 2>&1 | tail -1
checkx "broker path: resume the worker" 'X exec "lever--$NAME" lever-manager agent resume worker 2>&1 | grep -qE "running|resumed"'
checkx "lever msg send after the migration" '(cd "$INST" && "$LEVER" msg send --to "$NAME" post-migration 2>&1) | grep -q Sent'
att=$(cd "$INST" && timeout 15 script -qfec "'$LEVER' attach" /dev/null 2>&1 | head -c 4000)
checkx "lever attach opens with the member controller's PAT" '[ ${#att} -gt 100 ] && ! printf "%s" "$att" | grep -qiE "authentication failed|forbidden|no access token"'
checkx "the migration needed no broker link (the old window linked it)" '! grep -q "linking the jail.s runtime broker" "$WORK/migrate.log"'
start_worker2 "migrated"
AGENT_NAME="${IMAGE#lever.local/}"; AGENT_NAME="${AGENT_NAME#scionlocal/}"
AGENT_IMAGE="lever.local/$AGENT_NAME:$ARCH"
w2img=$(X inspect -f '{{.ImageName}}' lever--worker2 2>&1)
checkx "migrated: worker2 runs the image under its lever.local name ($w2img)" '[ "$w2img" = "$AGENT_IMAGE" ]'
checkx "migrated: the jail holds $AGENT_IMAGE" 'X image inspect "$AGENT_IMAGE" >/dev/null 2>&1'
mrow=$(doctor_row "manager image")
echo "      | $mrow" | cut -c1-400
checkx "migrated: doctor warns that the manager runs an unqualified image name" '[[ "$mrow" == "! manager image"*unqualified* ]]'
fence_checks "migrated"

say "a contact added while agents run"
write_config "    - { login: contact2@example.com, tier: contact, agents: [$NAME] }"
(cd "$INST" && timeout 1800 "$LEVER" reload 2>&1 | tail -5)
c2=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["users"].get("contact2@example.com",""))' "$INST/.lever-state/hub-identity.json")
checkx "contact2 was invited and recorded before any sign-in" '[ -n "$c2" ]'
check "doctor: contact2 bound and capped before sign-in" row_ok "remote web role"
me=$(proxy contact2@example.com "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "contact2 signs in through the proxy as the recorded hub user" 'printf "%s" "$me" | grep -q "\"id\":\"$c2\""'
code=$(proxy contact2@example.com -o /dev/null -w '%{http_code}' "http://127.0.0.1:$RP/api/v1/admin/roles")
checkx "the contact fence refuses other routes ($code)" '[ "$code" = 403 ]'
code=$(proxy contact2@example.com -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' --data '{"name":"x","scopes":["agent:message"]}' "http://127.0.0.1:$RP/api/v1/auth/tokens")
checkx "the proxy refuses a token mint for the contact ($code)" '[ "$code" = 403 ]'
checkx "the manager kept running" '[ "$mgr0" = "$(X inspect -f "{{.State.StartedAt}}" "lever--$NAME")" ]'

say "the contact removed while agents run"
write_config ""
(cd "$INST" && timeout 1800 "$LEVER" reload 2>&1 | tail -3)
code=$(proxy contact2@example.com -o /dev/null -w '%{http_code}' "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "the removed contact is refused ($code)" '[ "$code" != 200 ]'
check "doctor: the removed contact's hub user is suspended" row_ok "remote web role"

say "a contact removed while an agent loops the controller rename, with a token due"
# The manager renames scion's dev user every few ms through the hub's identity
# route. A removal must still take effect: at the proxy before any step, and at
# the hub before any controller work. A token is due, so the apply needs the
# controller's address and should fail naming the cause; the stale proxy must
# refuse the removed contact all the same.
write_config "    - { login: contact3@example.com, tier: contact, agents: [$NAME] }"
(cd "$INST" && timeout 1800 "$LEVER" reload 2>&1 | tail -3)
code=$(proxy contact3@example.com -o /dev/null -w '%{http_code}' "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "contact3 signs in before the removal ($code)" '[ "$code" = 200 ]'
cat > "$WORK/loop.py" <<'PY'
import json, os, time, urllib.request
tok = open(os.path.expanduser("~/.scion/scion-token")).read().strip()
ep = os.environ.get("SCION_HUB_ENDPOINT") or os.environ.get("SCION_HUB_URL")
end = time.time() + float(os.environ.get("LOOP_SECS", "600"))
while time.time() < end:
    req = urllib.request.Request(ep + "/api/v1/system/identity", data=json.dumps({"email": "agent-chosen@example.org"}).encode(), method="PUT",
                                 headers={"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    try:
        urllib.request.urlopen(req, timeout=5)
    except Exception:
        pass
    time.sleep(0.002)
PY
limactl copy "$WORK/loop.py" "$VM:/tmp/loop.py"
X cp /tmp/loop.py "lever--$NAME:/tmp/loop.py"
X exec -d -e LOOP_SECS=600 "lever--$NAME" python3 /tmp/loop.py
sleep 3
# Other recorded scopes make the mint urgent: a token only in its renew
# window would be left for a later apply with a warning.
python3 - "$INST/.lever-state/controller.pat.json" <<'PY'
import json, sys
p = sys.argv[1]; d = json.load(open(p))
d["requested_scopes"] = ["agent:manage"]
json.dump(d, open(p, "w"))
PY
ppid0=$(cat "$INST/.lever-state/remote.pid" 2>/dev/null)
write_config ""
(cd "$INST" && timeout 1800 "$LEVER" reload > "$WORK/race.log" 2>&1); race_rc=$?
tail -4 "$WORK/race.log"
code=$(proxy contact3@example.com -o /dev/null -w '%{http_code}' "http://127.0.0.1:$RP/api/v1/auth/me")
checkx "the removed contact is refused under the rename loop ($code)" '[ "$code" = 403 ]'
check "doctor: the removed contact's hub user is suspended under the loop" row_ok "remote web role"
if [ "$race_rc" != 0 ]; then
	check "the failed apply names the rename and says removals took effect" grep -q "does not stay on the controller.*already took effect" "$WORK/race.log"
	checkx "the proxy was not restarted (the fence refused the contact)" '[ "$ppid0" = "$(cat "$INST/.lever-state/remote.pid")" ]'
	check "the stale proxy audited the refusal as deny-revoked" grep -q '"ts_login":"contact3@example.com".*"decision":"deny-revoked"' "$INST/.lever-state/remote-audit.jsonl"
else
	echo "      | note: the controller assertion won the race; the apply minted and restarted the proxy"
fi
X exec "lever--$NAME" pkill -f loop.py
sleep 2
(cd "$INST" && timeout 1800 "$LEVER" apply 2>&1 | tail -3)
check "after the loop stops, doctor: hub identities" row_ok "hub identities"

say "forced controller PAT rotation while agents run"
old_id=$(python3 -c 'import json; print(json.load(open("'"$INST"'/.lever-state/controller.pat.json"))["id"])')
python3 - "$INST/.lever-state/controller.pat.json" <<'PY'
import json, sys, datetime
p = sys.argv[1]; d = json.load(open(p))
d["expires_at"] = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=10)).strftime("%Y-%m-%dT%H:%M:%SZ")
json.dump(d, open(p, "w"))
PY
(cd "$INST" && timeout 1800 "$LEVER" apply 2>&1 | tee "$WORK/rotate.log" | tail -3)
new_id=$(python3 -c 'import json; print(json.load(open("'"$INST"'/.lever-state/controller.pat.json"))["id"])')
checkx "a new controller PAT was minted" '[ "$old_id" != "$new_id" ]'
check "doctor: the old token is gone (hub identities)" row_ok "hub identities"
check "the broker works on the new PAT" mgrlist

say "what an agent can and cannot do"
cat > "$WORK/agent.py" <<'PY'
import json, os, urllib.request, base64
tok = open(os.path.expanduser("~/.scion/scion-token")).read().strip()
ep = os.environ.get("SCION_HUB_ENDPOINT") or os.environ.get("SCION_HUB_URL")
def call(m, p, hdr, body=None):
    req = urllib.request.Request(ep + p, method=m, headers=hdr, data=json.dumps(body).encode() if body is not None else None)
    try:
        return urllib.request.urlopen(req, timeout=20).status
    except urllib.error.HTTPError as e:
        return e.code
    except Exception as e:
        return "error " + type(e).__name__
b = lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()
forged = b({"alg": "none", "typ": "JWT"}) + "." + b({"email": "lever-admin@lever.local", "exp": 9999999999}) + "."
print("forged", call("GET", "/api/v1/auth/me", {"X-Lever-Hub-Assertion": forged}))
print("mint", call("POST", "/api/v1/auth/tokens", {"Authorization": "Bearer " + tok, "Content-Type": "application/json"}, {"name": "x", "scopes": ["agent:message"]}))
print("rename", call("PUT", "/api/v1/system/identity", {"Authorization": "Bearer " + tok, "Content-Type": "application/json"}, {"email": "evil@example.com"}))
PY
limactl copy "$WORK/agent.py" "$VM:/tmp/agent.py"
X cp /tmp/agent.py "lever--$NAME:/tmp/agent.py"
out=$(X exec "lever--$NAME" python3 /tmp/agent.py)
echo "$out" | sed 's/^/      | /'
checkx "a forged assertion gets 401" 'printf "%s\n" "$out" | grep -qx "forged 401"'
checkx "an agent token cannot mint a PAT" 'printf "%s\n" "$out" | grep -qE "^mint 40[13]$"'
checkx "nothing listens on the old login or throwaway ports" '! G ss -ltn | grep -qE ":(8446|48080) "'
pid2=$(hubpid)
(cd "$INST" && timeout 1800 "$LEVER" apply 2>&1 | tee "$WORK/repin.log" | tail -3)
check "the next apply re-pins the controller's address (R2)" grep -q "controller's hub address is now" "$WORK/repin.log"
checkx "the agent's settings write caused no hub restart" '[ "$pid2" = "$(hubpid)" ]'
check "doctor: hub identities after R2" row_ok "hub identities"

say "a fresh instance with this lever"
(cd "$INST" && "$LEVER" destroy 2>&1 | tail -2)
# A first apply on a new VM can lose the image tag right after the load (a
# pre-existing flake of image_tar); the second apply finds the image loaded.
for i in 1 2; do
	(cd "$INST" && timeout 3600 "$LEVER" apply 2>&1 | tee -a "$WORK/fresh.log" | tail -15) && break
done
check "the record says fresh" grep -q '"mode": *"fresh"' "$INST/.lever-state/hub-identity.json"
check "no dev-token file and no dev user" row_ok "hub identities"
check "doctor: hub authentication (fresh)" row_ok "hub authentication"
check "doctor: remote web role (fresh)" row_ok "remote web role"
check "doctor: scion project registration (fresh)" row_ok "scion project registration"
check "fresh: manager runs" running
PROJ=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["project_id"])' "$INST/.lever-state/hub-identity.json")
checkx "fresh: the project lists the jail's runtime broker" 'G sh -c "read -r t; curl -sS -m 20 -H \"Authorization: Bearer \$t\" http://127.0.0.1:8080/api/v1/projects/$PROJ/providers" < "$INST/.lever-state/controller.pat" | grep -q brokerId'
check "fresh: no hub link refused the earlier instance's project marker" sh -c '! grep -q "project not found under the user access token" "$1"' _ "$WORK/fresh.log"
start_worker2 "fresh"
checkx "fresh: the manager runs $AGENT_IMAGE" '[ "$(X inspect -f "{{.ImageName}}" "lever--$NAME")" = "$AGENT_IMAGE" ]'
check "fresh: doctor: manager image" row_ok "manager image"
check "fresh: doctor: hub container runtime" row_ok "hub container runtime"
fence_checks "fresh"

say "teardown"
(cd "$INST" && "$LEVER" destroy 2>&1 | tail -1)
if [ "$fail" = 0 ]; then echo "hub auth e2e: PASS"; else echo "hub auth e2e: FAIL"; fi
exit "$fail"
