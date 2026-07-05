#!/usr/bin/env bash
# Build the generic lever-claude agent image from the local scion-claude base,
# tagged for the local image registry (scionlocal) so scion uses it with no pull.
#
# Prerequisite: the bin/ and scionhook/ build-context dirs must be staged first
# (cross-compiled binaries + the pre-start hook). `make lever-image` does that
# for you, then runs this script; run it standalone only if you staged them
# yourself.
set -euo pipefail

CTX="$(cd "$(dirname "$0")" && pwd)"
REGISTRY="${LEVER_IMAGE_REGISTRY:-scionlocal}"

docker image inspect scion-claude:latest >/dev/null 2>&1 || {
  cat >&2 <<'EOF'
scion-claude:latest not found in the local Docker store.

lever-claude builds FROM scion's stock claude image. Build it once from a scion
checkout: clone github.com/GoogleCloudPlatform/scion and run its image build
(see scion's image-build/ — `image-build/scripts/build-images.sh scion-claude`),
which leaves scion-claude:latest in your local store. Then re-run this.
EOF
  exit 1
}

for d in bin scionhook; do
  [ -d "${CTX}/${d}" ] || {
    echo "build context ${CTX}/${d} is missing — run \`make lever-image\` (it stages the binaries + hook)." >&2
    exit 1
  }
done

docker build -t lever-claude:latest -t "${REGISTRY}/lever-claude:latest" \
  -f "${CTX}/Dockerfile" "${CTX}"

echo "Built lever-claude:latest (+ ${REGISTRY}/lever-claude:latest)."
