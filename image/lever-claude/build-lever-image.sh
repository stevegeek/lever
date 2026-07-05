#!/usr/bin/env bash
# Build the generic lever-claude agent image from the local scion-claude base,
# tagged for the local image registry (scionlocal) so scion uses it with no pull.
#
# Prerequisite: the bin/ and scionhook/ build-context dirs must be staged first
# (cross-compiled binaries + the pre-start hook). `make lever-image` does that
# for you, then runs this script; run it standalone only if you staged them
# yourself.
#
# Env:
#   LEVER_IMAGE_REGISTRY  registry prefix for the second tag (default: scionlocal)
#   LEVER_IMAGE_ARCH      arch the staged binaries were built for (default: arm64),
#                         checked against the scion-claude base's architecture
#   LEVER_IMAGE_FORCE=1   overwrite an existing scionlocal/lever-claude:latest
set -euo pipefail

CTX="$(cd "$(dirname "$0")" && pwd)"
REGISTRY="${LEVER_IMAGE_REGISTRY:-scionlocal}"
ARCH="${LEVER_IMAGE_ARCH:-arm64}"
TARGET="${REGISTRY}/lever-claude:latest"

docker image inspect scion-claude:latest >/dev/null 2>&1 || {
  cat >&2 <<'EOF'
scion-claude:latest not found in the local Docker store.

lever-claude builds FROM scion's stock claude image. Build it once from a scion
checkout: clone github.com/GoogleCloudPlatform/scion and run its image build —
`image-build/scripts/build-images.sh --target harnesses` (builds scion-claude
among the harness images; `--target all` builds the base chain from scratch if
you have nothing yet). See scion's image-build/ for details. Then re-run this.
EOF
  exit 1
}

for d in bin scionhook; do
  [ -d "${CTX}/${d}" ] || {
    echo "build context ${CTX}/${d} is missing — run \`make lever-image\` (it stages the binaries + hook)." >&2
    exit 1
  }
done

# Arch guard: docker build won't validate the ELF arch of the COPYed binaries, so
# a base/binary mismatch builds fine and then dies at container boot with a
# cryptic `exec format error` on the pre-start hook. Catch it up front.
BASE_ARCH="$(docker image inspect scion-claude:latest --format '{{.Architecture}}' 2>/dev/null || true)"
if [ -n "${BASE_ARCH}" ] && [ "${BASE_ARCH}" != "${ARCH}" ]; then
  cat >&2 <<EOF
arch mismatch: scion-claude:latest is ${BASE_ARCH}, but the binaries were staged for ${ARCH}.
The image would build but fail at boot with 'exec format error'. Rebuild the binaries for the
base arch: \`make lever-image LEVER_IMAGE_ARCH=${BASE_ARCH}\`.
EOF
  exit 1
fi

# Clobber guard: this tag is also what a real (possibly customized) instance
# image uses. Refuse to silently overwrite one — a Ruby-less generic image
# replacing a customized one boots a manager missing that instance's tooling.
if [ "${LEVER_IMAGE_FORCE:-}" != "1" ] && docker image inspect "${TARGET}" >/dev/null 2>&1; then
  cat >&2 <<EOF
${TARGET} already exists. Building would overwrite it with the generic image.
If that tag belongs to a customized instance image, DON'T — point manager.image at a
different tag, or extend the generic image (FROM lever-claude:latest). To overwrite anyway,
re-run with LEVER_IMAGE_FORCE=1.
EOF
  exit 1
fi

docker build -t lever-claude:latest -t "${TARGET}" -f "${CTX}/Dockerfile" "${CTX}"

echo "Built lever-claude:latest (+ ${TARGET})."
