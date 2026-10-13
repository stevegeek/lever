#!/usr/bin/env bash
# Build the generic lever-claude agent image from the local scion-claude base,
# tagged for the local image registry (scionlocal) so scion uses it with no pull.
#
# The bin/ and scionhook/ build-context dirs must be staged first;
# `make lever-image` does that, then runs this script.
#
# Images are tagged by arch, never :latest, so a host that builds both arches
# never clobbers one with the other. lever resolves a tagless `manager.image`
# to the jail's arch tag at apply time (internal/config archImage).
#
# Env:
#   LEVER_IMAGE_REGISTRY  registry prefix for the scionlocal tag (default: scionlocal)
#   LEVER_IMAGE_ARCH      arch the staged binaries were built for (default: arm64);
#                         selects the scion-claude:<arch> base and the output tag
#   LEVER_IMAGE_FORCE=1   overwrite an existing scionlocal/lever-claude:<arch>
#   LEVER_IMAGE_VERSION   lever version the staged binaries were built from, baked
#                         as the lever_version label (`make lever-image` sets it;
#                         unset leaves the label empty and `lever doctor` skips
#                         its host-vs-image version check)
#   LEVER_JAIL_ABI        in-jail compatibility version of the staged binaries
#                         (internal/jailabi), baked as the lever_jail_abi label
#                         (`make lever-image` sets it; unset leaves it empty and
#                         `lever doctor` compares lever_version instead)
set -euo pipefail

CTX="$(cd "$(dirname "$0")" && pwd)"
REGISTRY="${LEVER_IMAGE_REGISTRY:-scionlocal}"
ARCH="${LEVER_IMAGE_ARCH:-arm64}"
# Must be Go/docker arch naming — lever appends runtime.GOARCH (arm64/amd64) to a
# tagless image ref, so a base tagged :aarch64/:x86_64 would never be found.
case "${ARCH}" in
  arm64 | amd64) ;;
  *)
    echo "LEVER_IMAGE_ARCH must be 'arm64' or 'amd64' (Go/docker naming), got '${ARCH}'." >&2
    exit 1
    ;;
esac
BASE="scion-claude:${ARCH}"
TARGET="${REGISTRY}/lever-claude:${ARCH}"

docker image inspect "${BASE}" >/dev/null 2>&1 || {
  cat >&2 <<EOF
${BASE} not found in the local Docker store.

lever-claude builds FROM scion's stock claude image, tagged by arch. If you have an
untagged \`scion-claude:latest\`, tag it for its arch:
  docker tag scion-claude:latest ${BASE}   # only if that image really is ${ARCH}
Otherwise build it from a scion checkout: clone github.com/GoogleCloudPlatform/scion
and run \`image-build/scripts/build-images.sh --target harnesses\`, then tag the
result ${BASE}. See scion's image-build/ for details.
EOF
  exit 1
}

for d in bin scionhook; do
  [ -d "${CTX}/${d}" ] || {
    echo "build context ${CTX}/${d} is missing — run \`make lever-image\` (it stages the binaries + hook)." >&2
    exit 1
  }
done

# docker build does not check the ELF arch of the COPYed binaries, so a
# base/binary mismatch would only die at boot with `exec format error` on the
# pre-start hook. Verify the base's real architecture in case the tag lies.
BASE_ARCH="$(docker image inspect "${BASE}" --format '{{.Architecture}}' 2>/dev/null || true)"
if [ -n "${BASE_ARCH}" ] && [ "${BASE_ARCH}" != "${ARCH}" ]; then
  cat >&2 <<EOF
arch mismatch: ${BASE} is actually ${BASE_ARCH}, but binaries were staged for ${ARCH}.
The image would build but fail at boot with 'exec format error'. Fix the base tag, or build
for the base's arch: \`make lever-image LEVER_IMAGE_ARCH=${BASE_ARCH}\`.
EOF
  exit 1
fi

# This tag is also what a customized instance image uses: refuse to silently
# overwrite one with the generic image.
if [ "${LEVER_IMAGE_FORCE:-}" != "1" ] && docker image inspect "${TARGET}" >/dev/null 2>&1; then
  cat >&2 <<EOF
${TARGET} already exists. Building would overwrite it with the generic image.
If that tag belongs to a customized instance image, DON'T — point manager.image at a
different name, or extend the generic image (FROM lever-claude:${ARCH}). To overwrite anyway,
re-run with LEVER_IMAGE_FORCE=1.
EOF
  exit 1
fi

docker build --build-arg "SCION_BASE=${BASE}" --build-arg "LEVER_VERSION=${LEVER_IMAGE_VERSION:-}" --build-arg "LEVER_JAIL_ABI=${LEVER_JAIL_ABI:-}" -t "lever-claude:${ARCH}" -t "${TARGET}" -f "${CTX}/Dockerfile" "${CTX}"

echo "Built lever-claude:${ARCH} (+ ${TARGET})."
