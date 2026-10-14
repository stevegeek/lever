#!/usr/bin/env bash
# Build the generic lever-claude agent image from the local scion-claude base
# (or, with LEVER_IMAGE_FLAVOR=slim, lever-claude-slim from lever's slim base),
# tagged under lever's agent image registry name (lever.local), which scion
# never rewrites to the hub's image registry. No registry by that name exists:
# lever loads the image into the jail, so scion runs it with no pull.
#
# The bin/ and scionhook/ build-context dirs must be staged first;
# `make lever-image` does that, then runs this script.
#
# Images are tagged by arch, never :latest, so a host that builds both arches
# never clobbers one with the other. lever resolves a tagless `manager.image`
# to the jail's arch tag at apply time (internal/config archImage).
#
# Env:
#   LEVER_IMAGE_REGISTRY  registry prefix of the output tag (default: lever.local).
#                         The default also tags scionlocal/<name>:<arch>, the
#                         name a lever.yaml from before lever.local still uses
#   LEVER_IMAGE_ARCH      arch the staged binaries were built for (default: arm64);
#                         selects the scion-claude:<arch> base and the output tag
#   LEVER_IMAGE_FORCE=1   overwrite an existing <registry>/lever-claude:<arch>
#   LEVER_IMAGE_VERSION   lever version the staged binaries were built from, baked
#                         as the lever_version label (`make lever-image` sets it;
#                         unset leaves the label empty and `lever doctor` skips
#                         its host-vs-image version check)
#   LEVER_JAIL_ABI        in-jail compatibility version of the staged binaries
#                         (internal/jailabi), baked as the lever_jail_abi label
#                         (`make lever-image` sets it; unset leaves it empty and
#                         `lever doctor` compares lever_version instead)
#   LEVER_IMAGE_FLAVOR    full (default): FROM scion-claude:<arch>, output
#                         lever-claude:<arch>. slim: first build lever's slim
#                         base (image/scion-slim, its bin/ staged by
#                         `make lever-image-slim`) as lever-scion-slim:<arch>,
#                         then the same lever layer FROM it, output
#                         lever-claude-slim:<arch>
#   SCION_VERSION         slim only: the scion source the staged sciontool and
#                         scion were built from, baked as the scion_version label
set -euo pipefail

CTX="$(cd "$(dirname "$0")" && pwd)"
REGISTRY="${LEVER_IMAGE_REGISTRY:-lever.local}"
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
FLAVOR="${LEVER_IMAGE_FLAVOR:-full}"
case "${FLAVOR}" in
  full)
    BASE="scion-claude:${ARCH}"
    NAME="lever-claude"
    ;;
  slim)
    BASE="lever-scion-slim:${ARCH}"
    NAME="lever-claude-slim"
    ;;
  *)
    echo "LEVER_IMAGE_FLAVOR must be 'full' or 'slim', got '${FLAVOR}'." >&2
    exit 1
    ;;
esac
TARGETS=("${REGISTRY}/${NAME}:${ARCH}")
if [ "${REGISTRY}" = lever.local ]; then TARGETS+=("scionlocal/${NAME}:${ARCH}"); fi
TAG_ARGS=()
for t in "${TARGETS[@]}"; do TAG_ARGS+=(-t "${t}"); done

for d in bin scionhook; do
  [ -d "${CTX}/${d}" ] || {
    echo "build context ${CTX}/${d} is missing — run \`make lever-image\` (it stages the binaries + hook)." >&2
    exit 1
  }
done

# This tag is also what a customized instance image uses: refuse to silently
# overwrite one with the generic image.
for t in "${TARGETS[@]}"; do
  if [ "${LEVER_IMAGE_FORCE:-}" != "1" ] && docker image inspect "${t}" >/dev/null 2>&1; then
    cat >&2 <<EOF
${t} already exists. Building would overwrite it with the generic image.
If that tag belongs to a customized instance image, DON'T — point manager.image at a
different name, or extend the generic image (FROM ${NAME}:${ARCH}). To overwrite anyway,
re-run with LEVER_IMAGE_FORCE=1.
EOF
    exit 1
  fi
done

if [ "${FLAVOR}" = slim ]; then
  SLIM_CTX="$(cd "${CTX}/../scion-slim" && pwd)"
  for b in sciontool scion; do
    [ -x "${SLIM_CTX}/bin/${b}" ] || {
      echo "${SLIM_CTX}/bin/${b} is missing — run \`make lever-image-slim\` (it stages sciontool and scion)." >&2
      exit 1
    }
    # The base's arch comes from --platform, so the arch guard below cannot
    # catch binaries staged by an earlier build for the other arch. Byte 18 of
    # an ELF header is e_machine: 62 x86-64, 183 aarch64.
    case "$(od -An -tu1 -j18 -N1 "${SLIM_CTX}/bin/${b}" | tr -d ' ')" in
      62) bin_arch=amd64 ;;
      183) bin_arch=arm64 ;;
      *) bin_arch=unknown ;;
    esac
    [ "${bin_arch}" = "${ARCH}" ] || {
      echo "${SLIM_CTX}/bin/${b} is ${bin_arch}, not ${ARCH} — re-run \`make lever-image-slim LEVER_IMAGE_ARCH=${ARCH}\`." >&2
      exit 1
    }
  done
  # --platform: the slim base starts from a multi-arch node image, so the
  # arch is chosen here rather than by an arch-tagged local base.
  docker build --platform "linux/${ARCH}" --build-arg "SCION_VERSION=${SCION_VERSION:-}" \
    -t "${BASE}" -f "${SLIM_CTX}/Dockerfile" "${SLIM_CTX}"
fi

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

PLATFORM_ARGS=()
[ "${FLAVOR}" = slim ] && PLATFORM_ARGS=(--platform "linux/${ARCH}")
docker build ${PLATFORM_ARGS[@]+"${PLATFORM_ARGS[@]}"} --build-arg "SCION_BASE=${BASE}" --build-arg "LEVER_VERSION=${LEVER_IMAGE_VERSION:-}" --build-arg "LEVER_JAIL_ABI=${LEVER_JAIL_ABI:-}" -t "${NAME}:${ARCH}" "${TAG_ARGS[@]}" -f "${CTX}/Dockerfile" "${CTX}"

echo "Built ${NAME}:${ARCH} (+ ${TARGETS[*]})."
