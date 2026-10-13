#!/usr/bin/env bash
# Check a built lever agent image against what lever and scion need in it: the
# image config (entrypoint, user, labels), then in-image.sh in a throwaway
# container of the image. Runs against either flavor (lever-claude, built on
# scion-claude, or lever-claude-slim), so it is also the parity check between
# them. `make image-contract IMAGE=...` runs it.
#
# Usage: check-image.sh <image-ref>
#
# Env:
#   CONTAINER_CLI       docker (default) or podman
#   LEVER_IMAGE_ARCH    the arch the image must be (default: the :arm64/:amd64
#                       tag, else the image's own architecture)
#   LEVER_JAIL_ABI      when set, the lever_jail_abi label must equal it
#                       (`make -s lever-image-jail-abi` prints this source's)
#   LEVER_IMAGE_MAX_MB  when set, fail an image larger than this many MB
#   GIT_MIN_VERSION     git floor (default 2.47.0, the floor of the scion
#                       commit the shipped examples pin)
#
# The container gets no network and is removed afterwards. Claude Code is
# never run: its version comes from its package.json.
set -euo pipefail

image="${1:?usage: check-image.sh <image-ref>}"
cli="${CONTAINER_CLI:-docker}"
here="$(cd "$(dirname "$0")" && pwd)"
failures=0

ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}

inspect() { "$cli" image inspect --format "$1" "$image"; }

if ! inspect '{{.Id}}' >/dev/null 2>&1; then
  echo "image ${image} not found by ${cli}" >&2
  exit 2
fi

echo "--- image config (${image})"
image_arch="$(inspect '{{.Architecture}}')"
arch="${LEVER_IMAGE_ARCH:-}"
if [ -z "$arch" ]; then
  case "$image" in
    *:arm64) arch=arm64 ;;
    *:amd64) arch=amd64 ;;
    *) arch="$image_arch" ;;
  esac
fi
case "$arch" in
  arm64 | amd64) ;;
  *)
    echo "unsupported arch '${arch}' (want arm64 or amd64)" >&2
    exit 2
    ;;
esac
if [ "$image_arch" = "$arch" ]; then ok "architecture ${arch}"; else fail "architecture is ${image_arch}, want ${arch}"; fi

entrypoint="$(inspect '{{json .Config.Entrypoint}}')"
if [ "$entrypoint" = '["sciontool","init","--"]' ]; then
  ok "entrypoint sciontool init --"
else
  fail "entrypoint is ${entrypoint}, want [\"sciontool\",\"init\",\"--\"]: scion starts the agent through it"
fi

user="$(inspect '{{.Config.User}}')"
if [ "$user" = scion ]; then ok "user scion"; else fail "image user is '${user}', want scion"; fi

label() { inspect "{{index .Config.Labels \"$1\"}}" 2>/dev/null | sed 's/^<no value>$//'; }
claude_label="$(label claude_code_version)"
tmux_label="$(label tmux_version)"
abi_label="$(label lever_jail_abi)"
version_label="$(label lever_version)"
scion_label="$(label scion_version)"
if [ -n "$claude_label" ]; then ok "claude_code_version ${claude_label}"; else fail "no claude_code_version label (lever doctor reads it)"; fi
if [ -n "$tmux_label" ]; then ok "tmux_version ${tmux_label}"; else fail "no tmux_version label"; fi
if [ -z "$abi_label" ]; then
  fail "no lever_jail_abi label: lever doctor cannot tell whether the image matches the host lever"
elif [ -n "${LEVER_JAIL_ABI:-}" ] && [ "$abi_label" != "$LEVER_JAIL_ABI" ]; then
  fail "lever_jail_abi is ${abi_label}, this lever source is ${LEVER_JAIL_ABI}"
else
  ok "lever_jail_abi ${abi_label}"
fi
printf 'note  lever_version %s\n' "${version_label:-(unset)}"
[ -z "$scion_label" ] || printf 'note  scion_version %s\n' "$scion_label"

size="$(inspect '{{.Size}}')"
size_mb=$((size / 1000000))
printf 'note  size %s MB\n' "$size_mb"
if [ -n "${LEVER_IMAGE_MAX_MB:-}" ]; then
  if [ "$size_mb" -le "$LEVER_IMAGE_MAX_MB" ]; then ok "size within ${LEVER_IMAGE_MAX_MB} MB"; else fail "size ${size_mb} MB exceeds ${LEVER_IMAGE_MAX_MB} MB"; fi
fi

echo "--- in the image"
in_rc=0
"$cli" run --rm -i --network none --entrypoint /bin/sh "$image" -s -- \
  "$arch" "$claude_label" "$tmux_label" "${GIT_MIN_VERSION:-2.47.0}" <"${here}/in-image.sh" || in_rc=$?

if [ "$failures" -ne 0 ] || [ "$in_rc" -ne 0 ]; then
  echo "contract: ${image} FAILS (${failures} config failure(s); in-image exit ${in_rc})"
  exit 1
fi
echo "contract: ${image} passes"
