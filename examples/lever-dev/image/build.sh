#!/usr/bin/env bash
# Build scionlocal/lever-dev-claude:<arch> on top of lever-claude:<arch>.
# Pin the versions in the environment (see examples/lever-dev/README.md):
#   LIMA_VERSION=2.2.1 LIMA_SHA256=<from the release SHA256SUMS> PLAYWRIGHT_VERSION=<x.y.z> ./build.sh
set -euo pipefail
cd "$(dirname "$0")"
ARCH="${LEVER_IMAGE_ARCH:-$(go env GOARCH)}"
: "${LIMA_VERSION:?set LIMA_VERSION}" "${LIMA_SHA256:?set LIMA_SHA256}" "${PLAYWRIGHT_VERSION:?set PLAYWRIGHT_VERSION}"
[ "$ARCH" = amd64 ] || { echo "lever-dev image: only amd64 is supported (Lima x86_64 tarball)" >&2; exit 1; }
docker image inspect "lever-claude:${ARCH}" >/dev/null || { echo "missing lever-claude:${ARCH}: run make lever-image in the host-only lever clone" >&2; exit 1; }
got=$(docker image inspect --format '{{.Architecture}}' "lever-claude:${ARCH}")
[ "$got" = "$ARCH" ] || { echo "lever-claude:${ARCH} is $got" >&2; exit 1; }
docker build \
  --build-arg LEVER_IMAGE_ARCH="$ARCH" --build-arg LEVER_BASE="lever-claude:${ARCH}" \
  --build-arg LIMA_VERSION="$LIMA_VERSION" --build-arg LIMA_SHA256="$LIMA_SHA256" \
  --build-arg PLAYWRIGHT_VERSION="$PLAYWRIGHT_VERSION" \
  -t "scionlocal/lever-dev-claude:${ARCH}" .
# Smoke test: the toolchain is there (never run claude here). --entrypoint bash skips
# sciontool's init and the scion pre-start hook, which print errors outside scion.
docker run --rm --entrypoint bash "scionlocal/lever-dev-claude:${ARCH}" -c 'go version && limactl --version && qemu-system-x86_64 --version | head -1 && test -d /usr/share/OVMF'
