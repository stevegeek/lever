# lever-dev agent image: lever-claude + the toolchain to build and live-test
# lever inside the jail (Go, QEMU, Lima, Playwright Chromium). Build with
# build.sh; keep this directory OUTSIDE the instance tree.
ARG LEVER_IMAGE_ARCH=amd64
ARG LEVER_BASE=lever-claude:${LEVER_IMAGE_ARCH}
ARG GO_VERSION=1.26.4
FROM --platform=linux/${LEVER_IMAGE_ARCH} golang:${GO_VERSION}-bookworm AS go

FROM ${LEVER_BASE}
ARG GO_VERSION
ARG LIMA_VERSION
ARG LIMA_SHA256
ARG PLAYWRIGHT_VERSION
USER root
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:/home/scion/go/bin:$PATH
RUN apt-get update \
 && apt-get install -y --no-install-recommends qemu-system-x86 qemu-utils ovmf make git file curl procps openssh-client ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN test -n "$LIMA_VERSION" && test -n "$LIMA_SHA256" \
 && curl -fsSLo /tmp/lima.tgz "https://github.com/lima-vm/lima/releases/download/v${LIMA_VERSION}/lima-${LIMA_VERSION}-Linux-x86_64.tar.gz" \
 && echo "${LIMA_SHA256}  /tmp/lima.tgz" | sha256sum -c - \
 && tar -xzf /tmp/lima.tgz -C /usr/local && rm /tmp/lima.tgz
RUN test -n "$PLAYWRIGHT_VERSION" && npx -y "playwright@${PLAYWRIGHT_VERSION}" install-deps chromium
# Lima's sockets must live on container-local disk, not the 9p tree mount.
ENV LIMA_HOME=/home/scion/.lima
LABEL go_version=${GO_VERSION} lima_version=${LIMA_VERSION} playwright_version=${PLAYWRIGHT_VERSION}
RUN chown -R scion:scion /home/scion
USER scion
RUN npx -y "playwright@${PLAYWRIGHT_VERSION}" install chromium
