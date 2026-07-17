---
title: "Install & build the image"
nav_order: 2.1
parent: Getting started
permalink: /getting-started/install/
---
Part of [getting started](/getting-started/). Steps keep their original numbering.

## 1. Install the binaries

```sh
cd /path/to/lever_to
make all
```

This builds the host binary:

- **`lever`** (host control plane) → `~/.local/bin/lever` (make sure that's on your `PATH`).

The in-jail orchestration binary, **`lever-manager`**, isn't built here; it's baked into your agent
image. `make lever-image-bins` cross-compiles `lever-manager` (alongside `lever-agent` and
`lever-tool-db`) into your image build context (`LEVER_IMAGE_CTX` in the Makefile), and your
Dockerfile `COPY`s them to `/usr/local/bin`, so it's already on `PATH` inside the manager and worker
containers when they boot.

Verify: `lever version`.

## 1a. Build the agent image

`examples/hello-worker` (and every instance) runs the agent image
`scionlocal/lever-claude:<arch>`. Build the generic one in a single command:

```sh
make lever-image
```

This cross-compiles the in-jail binaries into `image/lever-claude/`, syncs the pre-start hook, and
runs `docker build` to produce `scionlocal/lever-claude:<arch>` (the local `scionlocal/` registry
prefix is what scion loads without a pull).

**Claude Code version:** the image bakes Claude Code at an explicit pin (`ARG
CLAUDE_CODE_VERSION` in the Dockerfile) and disables the in-container auto-updater — agents never
self-update; upgrades happen by rebuild. To bump: edit the ARG (or pass `--build-arg
CLAUDE_CODE_VERSION=X.Y.Z`), rebuild the image, `lever apply`, and power-cycle the manager
(`lever stop && lever up` — the conversation is preserved on OrbStack). Don't rely on the scion
base image's copy: it installs claude unpinned, so it's whatever was current when that base was
last rebuilt.

Cross-build for another arch with `LEVER_IMAGE_ARCH=amd64` (it builds `FROM scion-claude:amd64` and
tags the output `:amd64`).

**One prerequisite: scion's base image, arch-tagged.** lever-claude builds `FROM scion-claude:<arch>`,
scion's stock Claude harness image. Build it once from a scion checkout:

```sh
git clone https://github.com/GoogleCloudPlatform/scion
cd scion && image-build/scripts/build-images.sh --target harnesses
```

`--target harnesses` builds `scion-claude` (among the harness images); if you're starting from
nothing, `--target all` builds the whole base chain first. That leaves `scion-claude:latest` in your
local Docker store — **tag it for its arch** so `make lever-image` finds it:

```sh
docker tag scion-claude:latest scion-claude:arm64   # or :amd64, matching the image's real arch
```

(The build script tells you exactly this if the arch-tagged base is missing.) See scion's
`image-build/` for the full story — scion owns this step.

**Extending the image for your instance.** The generic image is deliberately minimal — scion's
harness plus lever's binaries and boot hook, nothing else. If your agents need a language toolchain
or a project CLI, write a small instance Dockerfile `FROM lever-claude:<arch>` that adds it, and point
`manager.image` (and any worker `image:`) at your tag. If your added layer does root-level work
under `/home/scion`, end it by re-running `RUN chown -R scion:scion /home/scion` and `USER scion`.
The jail runs rootless podman, where a root-owned home is unwritable by the agent and silently
breaks its boot hook. Keep instance-specific tooling in that layer, not in the framework image.
