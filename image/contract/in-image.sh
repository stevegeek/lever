# The in-image half of lever's agent-image contract. check-image.sh pipes this
# file into `sh -s` in a throwaway container of the image (default user, no
# network), so it runs as the user scion starts the agent as and sees what an
# agent sees. POSIX sh: the image's /bin/sh is dash.
#
# Args: <arch: amd64|arm64> <claude_code_version label> <tmux_version label> <git floor>
#
# It never runs claude: Claude Code's version is read from its package.json.
#
# Each check names who needs the thing. The lever side is mirrored by
# image/contract/contract_test.go, which fails when a path or user lever's
# code relies on is missing here.

arch=$1 claude_label=$2 tmux_label=$3 git_min=$4
failures=0

ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}
note() { printf 'note  %s\n' "$*"; }

ver_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n 1)" = "$2" ]; }

elf_arch() {
  case "$(od -An -tu1 -j18 -N1 "$1" 2>/dev/null | tr -d ' ')" in
    62) echo amd64 ;;
    183) echo arm64 ;;
    *) echo unknown ;;
  esac
}

need_cmd() { # need_cmd <name> <who needs it>
  if p=$(command -v "$1" 2>/dev/null); then
    ok "$1 ($p)"
  else
    fail "$1 not on PATH; needed by $2"
  fi
}

need_elf() { # need_elf <path> <who needs it>
  if [ ! -f "$1" ] || [ ! -x "$1" ]; then
    fail "$1 missing or not executable; needed by $2"
  elif [ "$(elf_arch "$1")" != "$arch" ]; then
    fail "$1 is $(elf_arch "$1"), the image is $arch; it would fail with exec format error"
  else
    ok "$1 ($arch)"
  fi
}

owner() { stat -c %u:%g "$1" 2>/dev/null; }

echo "--- user (scion keep-id mapping, every lever probe: podman exec --user scion)"
if [ "$(id -un)" = scion ] && [ "$(id -u):$(id -g)" = 1000:1000 ]; then
  ok "runs as scion, uid 1000, gid 1000"
else
  fail "runs as $(id -un) ($(id -u):$(id -g)), want scion (1000:1000): image USER and scion's keep-id mapping"
fi
pw=$(getent passwd scion)
pw_home=$(printf '%s' "$pw" | cut -d: -f6)
pw_shell=$(printf '%s' "$pw" | cut -d: -f7)
if [ "$pw_home" = /home/scion ]; then ok "scion's home is /home/scion"; else fail "scion's home is '$pw_home', want /home/scion"; fi
if [ -n "$pw_shell" ] && [ -x "$pw_shell" ]; then ok "scion's login shell $pw_shell exists"; else fail "scion's login shell '$pw_shell' is not executable (tmux's shell window)"; fi
if [ "${HOME:-}" = /home/scion ]; then ok "HOME=/home/scion"; else fail "HOME is '${HOME:-}', want /home/scion"; fi
if [ "$(owner /home/scion)" = 1000:1000 ] && [ -w /home/scion ]; then
  ok "/home/scion is scion's and writable (sciontool pre-flight, the boot hook under rootless podman)"
else
  fail "/home/scion is $(owner /home/scion), not scion's and writable: the boot hook fails under rootless podman"
fi
if [ -d /home/scion/.scion ]; then ok "/home/scion/.scion exists"; else fail "/home/scion/.scion missing (scion base layout)"; fi
if [ -d /workspace ] && [ "$(owner /workspace)" = 1000:1000 ]; then ok "/workspace is scion's"; else fail "/workspace missing or not scion's (the agent's work tree mount point)"; fi
if [ -d /tmp ] && [ -w /tmp ] && [ -k /tmp ]; then ok "/tmp is writable and sticky (the claude-ready marker)"; else fail "/tmp is not a writable sticky dir: the claude-ready marker cannot be written"; fi

echo "--- scion runtime (sciontool init, the claude harness provisioner)"
need_elf "$(command -v sciontool 2>/dev/null || echo /usr/local/bin/sciontool)" "scion: the image entrypoint, sciontool init"
if sciontool version >/dev/null 2>&1; then ok "sciontool version: $(sciontool version 2>/dev/null | head -n 1)"; else fail "sciontool version failed"; fi
need_elf "$(command -v scion 2>/dev/null || echo /usr/local/bin/scion)" "agents: scion message"
if scion version >/dev/null 2>&1; then ok "scion version: $(scion version 2>/dev/null | head -n 1)"; else fail "scion version failed"; fi
need_cmd tmux "scion: the harness runs in a tmux session; lever's probes list its panes"
tmux_want="tmux ${tmux_label%%-*}"
if [ -z "$tmux_label" ]; then
  fail "no tmux_version label: the lever layer (image/lever-claude/Dockerfile) did not install the pinned tmux"
elif [ "$(tmux -V 2>/dev/null)" = "$tmux_want" ]; then
  ok "$tmux_want matches the tmux_version label"
else
  fail "tmux -V is '$(tmux -V 2>/dev/null)', the tmux_version label says '$tmux_want'"
fi
need_cmd python3 "scion: the claude harness provisioner (provision.py)"
if python3 -c 'import argparse, json, os, subprocess, sys, tempfile, typing' 2>/dev/null; then ok "python3 runs with the stdlib modules provision.py imports"; else fail "python3 cannot import the stdlib modules provision.py needs"; fi
for c in usermod groupmod sed; do need_cmd "$c" "sciontool init: remapping the scion user"; done
need_cmd bash "scion: the image's default command"
need_cmd sh "scion: sh -c around tmux and the harness; lever's probe scripts"
need_cmd git "the agent's work in /workspace, sciontool's workspace setup"
git_have=$(git --version 2>/dev/null | awk '{print $3}')
if [ -n "$git_have" ] && ver_ge "$git_have" "$git_min"; then ok "git $git_have >= $git_min"; else fail "git '${git_have:-none}' < $git_min (the floor of the scion commit checked against)"; fi
if git --version 2>&1 >/dev/null | grep -q .; then fail "git writes to stderr on --version"; else ok "git writes nothing to stderr"; fi
need_cmd pgrep "sciontool doctor"
found_ti=
for d in /usr/share/terminfo /lib/terminfo /usr/lib/terminfo /etc/terminfo; do
  [ -e "$d/x/xterm-256color" ] && found_ti=$d
done
if [ -n "$found_ti" ]; then ok "xterm-256color terminfo ($found_ti) for scion's .tmux.conf"; else fail "no xterm-256color terminfo: scion's .tmux.conf sets it as default-terminal"; fi
if [ -f /usr/share/zoneinfo/UTC ]; then ok "tzdata (/usr/share/zoneinfo)"; else fail "no /usr/share/zoneinfo: a configured TZ silently becomes UTC"; fi
if [ -s /etc/ssl/certs/ca-certificates.crt ]; then ok "CA bundle"; else fail "no /etc/ssl/certs/ca-certificates.crt: TLS from git and other tools fails"; fi

echo "--- lever (the in-jail binaries, the boot hook, Claude Code's settings)"
for b in lever-agent lever-manager lever-tool-db; do
  need_elf "/usr/local/bin/$b" "lever"
done
hook=/etc/scion/hooks/pre-start.d/10-lever-agent-boot
if [ -f "$hook" ] && [ -x "$hook" ] && [ "$(owner "$hook")" = 0:0 ] && [ ! -w "$hook" ]; then
  ok "$hook (root's, executable, not writable by the agent)"
else
  fail "$hook missing, not executable, or not root's: the agent never enrols"
fi
if grep -q 'lever-agent boot' "$hook" 2>/dev/null; then ok "the hook runs lever-agent boot"; else fail "the hook does not run lever-agent boot"; fi
if [ -d /etc/claude-code ] && [ "$(owner /etc/claude-code)" = 1000:1000 ] && [ -w /etc/claude-code ]; then
  ok "/etc/claude-code is scion's (boot writes managed-settings.json as scion)"
else
  fail "/etc/claude-code missing or not writable by scion: boot cannot write the managed settings"
fi
need_cmd node "Claude Code's npm install and node-based MCP servers"
need_cmd claude "scion: the harness command"
claude_bin=$(command -v claude 2>/dev/null)
claude_real=$(readlink -f "$claude_bin" 2>/dev/null)
claude_pkg=
d=$(dirname "$claude_real" 2>/dev/null)
while [ -n "$d" ] && [ "$d" != / ]; do
  if [ -f "$d/package.json" ] && grep -q '"name": *"@anthropic-ai/claude-code"' "$d/package.json"; then
    claude_pkg=$d/package.json
    break
  fi
  d=$(dirname "$d")
done
if [ -z "$claude_pkg" ]; then
  fail "cannot find Claude Code's package.json from $claude_bin"
else
  claude_have=$(sed -n 's/^ *"version": *"\([^"]*\)".*/\1/p' "$claude_pkg" | head -n 1)
  if [ -n "$claude_label" ] && [ "$claude_have" = "$claude_label" ]; then
    ok "Claude Code $claude_have matches the claude_code_version label"
  else
    fail "Claude Code is '$claude_have', the claude_code_version label says '$claude_label'"
  fi
  if [ "$(owner "$claude_real")" = 0:0 ] && [ ! -w "$claude_real" ]; then
    ok "Claude Code's binary is root's (no self-update)"
  else
    fail "Claude Code's binary $claude_real is writable by the agent or not root's"
  fi
fi
claude_n=$(for d in $(printf '%s' "$PATH" | tr ':' ' '); do [ -x "$d/claude" ] && readlink -f "$d/claude"; done | sort -u | wc -l)
if [ "$claude_n" -eq 1 ]; then ok "one Claude Code on PATH"; else note "$claude_n distinct claude binaries on PATH; the first one wins"; fi
expect_env() { # expect_env <name> <value>: compares without printing the value
  eval "v=\${$1-}"
  if [ "$v" = "$2" ]; then ok "$1 as lever sets it"; else fail "$1 is not what image/lever-claude/Dockerfile sets"; fi
}
expect_env CLAUDE_CODE_CLIENT_CERT /home/scion/.lever-id/agent.crt
expect_env CLAUDE_CODE_CLIENT_KEY /home/scion/.lever-id/agent.key
expect_env NODE_EXTRA_CA_CERTS /home/scion/.lever-id/ca.crt
expect_env DISABLE_AUTOUPDATER 1
expect_env LC_ALL C.UTF-8
for c in head tr cut date readlink grep sleep wc stat od; do need_cmd "$c" "lever's probe scripts (internal/jail)"; done
if sleep 0.1 2>/dev/null; then ok "sleep takes fractions (the claude-ready probe polls every 0.2 s)"; else fail "sleep 0.1 fails: the claude-ready probe cannot poll"; fi

echo "--- end to end: the claude-ready hook in a scion-shaped tmux session"
sock=lever-contract-$$
marker=/tmp/lever-claude-ready
rm -f "$marker"
if tmux -L "$sock" -f /dev/null new-session -d -s scion -n agent 'lever-agent claude-ready; sleep 30' 2>/dev/null; then
  pane=$(tmux -L "$sock" list-panes -t scion:agent -F '#{pane_id}' 2>/dev/null | head -n 1)
  i=0
  while [ ! -s "$marker" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
  done
  # The run identity exactly as lever's manager gate computes it in the
  # container (readyScript, internal/jail/claudeready.go), so the marker
  # lever-agent wrote must equal it byte for byte.
  read -r boot </proc/sys/kernel/random/boot_id
  st=$(head -c 1024 /proc/1/stat)
  st=${st##*)}
  set -f
  set -- $st
  eval "st=\${20}"
  set +f
  got=$(head -c 256 "$marker" 2>/dev/null)
  if [ -n "$pane" ] && [ "$got" = "v1 $boot $st $pane" ]; then
    ok "lever-agent claude-ready wrote the marker lever's gate expects for pane $pane"
  else
    fail "the claude-ready marker is '$got', lever's gate expects 'v1 $boot $st $pane'"
  fi
  tmux -L "$sock" kill-server 2>/dev/null
else
  fail "tmux cannot start a detached session as scion"
fi
rm -f "$marker"

echo "--- $failures failure(s)"
[ "$failures" -eq 0 ]
