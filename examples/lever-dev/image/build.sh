#!/usr/bin/env bash
# Build lever.local/lever-dev-claude:<arch> on top of lever-claude:<arch>, or on
# another base: LEVER_BASE=lever.local/lever-claude-slim:amd64 \
#   LEVER_DEV_TAG=lever.local/lever-dev-claude-slim:amd64 ./build.sh
# Pin the versions in the environment (see examples/lever-dev/README.md):
#   LIMA_VERSION=2.2.1 LIMA_SHA256=<from the release SHA256SUMS> PLAYWRIGHT_VERSION=<x.y.z> ./build.sh
set -euo pipefail
cd "$(dirname "$0")"
ARCH="${LEVER_IMAGE_ARCH:-$(go env GOARCH)}"
: "${LIMA_VERSION:?set LIMA_VERSION}" "${LIMA_SHA256:?set LIMA_SHA256}" "${PLAYWRIGHT_VERSION:?set PLAYWRIGHT_VERSION}"
[ "$ARCH" = amd64 ] || { echo "lever-dev image: only amd64 is supported (Lima x86_64 tarball)" >&2; exit 1; }
BASE="${LEVER_BASE:-lever-claude:${ARCH}}"
TAG="${LEVER_DEV_TAG:-lever.local/lever-dev-claude:${ARCH}}"
docker image inspect "$BASE" >/dev/null || { echo "missing $BASE: run make lever-image (or make lever-image-slim) in the host-only lever clone" >&2; exit 1; }
got=$(docker image inspect --format '{{.Architecture}}' "$BASE")
[ "$got" = "$ARCH" ] || { echo "$BASE is $got" >&2; exit 1; }
docker build \
  --build-arg LEVER_IMAGE_ARCH="$ARCH" --build-arg LEVER_BASE="$BASE" \
  --build-arg LIMA_VERSION="$LIMA_VERSION" --build-arg LIMA_SHA256="$LIMA_SHA256" \
  --build-arg PLAYWRIGHT_VERSION="$PLAYWRIGHT_VERSION" \
  -t "$TAG" .
# Smoke test: the toolchain is there (never run claude here). --entrypoint bash skips
# sciontool's init and the scion pre-start hook, which print errors outside scion.
docker run --rm --entrypoint bash "$TAG" -c 'go version && limactl --version && qemu-system-x86_64 --version | head -1 && test -d /usr/share/OVMF && gcc --version | head -1 && find "$PLAYWRIGHT_BROWSERS_PATH" -type f -name chrome | grep -q . && echo "playwright chromium: ok"'
