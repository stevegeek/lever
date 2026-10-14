---
title: "Install & build the image"
nav_order: 2.1
parent: Getting started
permalink: /getting-started/install/
---
Part of [getting started](/getting-started/). Steps keep their original numbering.

## 1. Install the binaries

With [Homebrew](https://brew.sh) on macOS (Apple Silicon or Intel):

```sh
brew install stevegeek/tap/lever
```

This installs the release archive's binaries (below) into Homebrew's `bin` directory;
`brew upgrade lever` moves to a newer release. The cask also lists the Linux archives, but
Homebrew's support for casks on Linux is newer and not tested with lever; the release archive or a
source build is the supported route there. lever starts broker tools with the fixed `PATH`
`/usr/local/bin:/usr/bin:/bin`, not your shell's, so where Homebrew's `bin` is elsewhere (Apple
Silicon: `/opt/homebrew/bin`) name a tool by its absolute path in `lever.yaml`, for example `command: [/opt/homebrew/bin/lever-tool-github, ...]`.

A release archive from GitHub holds the same host binaries, ready to run. Its `lever` needs **no Go
toolchain** for the scion pin of its release (`b41cd6fe`, the pin in the examples): `lever apply`
downloads that release's prebuilt scion, checks it against the sha256 built into `lever`, and skips
the scion compile. Any other pin, a failed download, and `remote.enabled` (which builds scion's web
UI from source) still need Go. See [prebuilt scion](/reference/config/#prebuilt-scion).

To build from a checkout instead:

```sh
cd /path/to/lever
make all
```

This builds the host binaries into `~/.local/bin` (make sure that's on your `PATH`):

- **`lever`** (host control plane).
- **`lever-tool-github`**, **`lever-tool-fizzy`**, **`lever-tool-whisper`** and **`lever-tool-tts`**,
  optional host-side broker tools (see the [github tool](/github-tool/), the
  [fizzy tool](/fizzy-tool/), the [whisper tool](/whisper-tool/) and the [tts tool](/tts-tool/)).

The in-jail orchestration binary, **`lever-manager`**, isn't built here; it's baked into your agent
image. `make lever-image-bins` cross-compiles `lever-manager` (alongside `lever-agent` and
`lever-tool-db`) into your image build context (`LEVER_IMAGE_CTX` in the Makefile), and your
Dockerfile `COPY`s them to `/usr/local/bin`, so it's already on `PATH` inside the manager and worker
containers when they boot.

Verify: `lever version` (or `lever --version`). It prints the release plus the build provenance,
for example `0.31.0 (21ff051f651f)`: the short commit, with `-dirty` for an uncommitted tree. A
dirty build, or a build with no commit stamp (Go stamps none in a git worktree), also adds the
binary's own hash: `0.31.0 (bin 3f2a…)`. A `go install …@vX.Y.Z` build shows the module version.

## 1a. Build the agent image

`examples/hello-worker` (and every instance) runs the agent image
`lever.local/lever-claude:<arch>`. Build the generic one in a single command:

```sh
make lever-image
```

This cross-compiles the in-jail binaries into `image/lever-claude/`, syncs the pre-start hook, and
runs `docker build` to produce `lever.local/lever-claude:<arch>`, and `scionlocal/lever-claude:<arch>`
for a `lever.yaml` that still names it. No registry called `lever.local` exists: `lever apply` loads
the image into the jail, so scion runs it with no pull. The dotted name is what keeps scion from
swapping in the hub's image registry, which an agent can change (see [worker
isolation](/security-model/worker-isolation/)).

**Claude Code version:** the image bakes Claude Code at an explicit pin (`ARG
CLAUDE_CODE_VERSION` in the Dockerfile) and disables the in-container auto-updater — agents never
self-update; upgrades happen by rebuild. To bump: edit the ARG (or pass `--build-arg
CLAUDE_CODE_VERSION=X.Y.Z`), rebuild the image with `make lever-image LEVER_IMAGE_FORCE=1` (the
build script refuses to overwrite an existing `lever.local/lever-claude:<arch>` without it),
`lever apply`, and recreate the manager on the new image with `lever up --fresh` — a record keeps
the image it was created with, so `lever stop && lever up` resumes the old one. `--fresh` starts a
new conversation after backing up the old one; add `--keep-conversation` to keep it. Don't rely on the scion
base image's copy: it installs claude unpinned, so it's whatever was current when that base was
last rebuilt.

**tmux version:** scion runs each agent's harness in a tmux session, and the scion base installs
Debian trixie's tmux 3.5a, whose server can crash and take the agent with it (see [tmux server
crashes](/operations/#tmux-server-crashes)). The image replaces it with a pinned tmux from
trixie-backports (`ARG TMUX_VERSION` in the Dockerfile, recorded in the `tmux_version` label),
installed through apt, which checks the Debian archive signatures. When backports moves on, the old
version leaves the archive and the build fails at that step: bump the ARG to the version
`https://packages.debian.org/trixie-backports/tmux` shows and rebuild. An instance Dockerfile that
copies the binaries in itself (below) does not get this step; copy it.

**lever version:** the image tag names only the arch, so a rebuild from another lever checkout
replaces `lever.local/lever-claude:<arch>` for every instance on the host. `make lever-image` records
the lever version the in-jail binaries were built from in a second label, `lever_version` (the
release and short commit of that source, e.g. `0.28.1 (21ff051f651f)`; never a binary hash), and
the in-jail compatibility version in a third, `lever_jail_abi` (a number that changes only when the
code baked into the image changes). `lever doctor`'s `agent lever version` row compares the jail ABI
with the host `lever`: equal passes, and a different release is shown for information only; a
different ABI fails. The fix is to rebuild the image from the host lever's source, `lever apply`,
then `lever up --fresh`. An image without `lever_jail_abi` (built before that label existed) falls back to
comparing the release in `lever_version` and fails when it differs. An image with neither label
(built before the labels existed, or by running `build-lever-image.sh` without
`LEVER_IMAGE_VERSION`) reads as "not checked".

Cross-build for another arch with `LEVER_IMAGE_ARCH=amd64` (it builds `FROM scion-claude:amd64` and
tags the output `:amd64`).

**One prerequisite: scion's base image, arch-tagged.** lever-claude builds `FROM scion-claude:<arch>`,
scion's stock Claude harness image. Build it once from a scion checkout:

```sh
git clone https://github.com/GoogleCloudPlatform/scion
cd scion && git checkout b41cd6fe   # the same commit as scion.version in lever.yaml
image-build/scripts/build-images.sh --target harnesses
```

`--target harnesses` builds `scion-claude` (among the harness images); if you're starting from
nothing, `--target all` builds the whole base chain first. That leaves `scion-claude:latest` in your
local Docker store — **tag it for its arch** so `make lever-image` finds it:

```sh
docker tag scion-claude:latest scion-claude:arm64   # or :amd64, matching the image's real arch
```

(The build script reports this if the arch-tagged base is missing.) See scion's `image-build/`
for detail; scion owns this step.

**The slim image (opt-in).** `scion-claude` brings chromium, gcloud, gcsfuse, Go, kubectl,
golangci-lint and bun, and a lever image built on it is about 5 GB. `make lever-image-slim` builds
`lever.local/lever-claude-slim:<arch>` instead, with no scion image build at all: a Debian trixie
base (`node:24-trixie-slim`) with only what scion and lever need in an agent container (sciontool
and scion, the `scion` user, python3 for scion's Claude provisioner, git, zsh, tzdata, procps and
nano), then the same lever layer as the full image (Claude Code at its pin, installed once, the
pinned tmux, lever's binaries, the boot hook and `/etc/claude-code`). It should come to about 1 GB;
`make image-contract` prints the built size.

```sh
make lever-image-slim SCION_VERSION=b41cd6fe   # your lever.yaml's scion.version
```

sciontool and scion are cross-compiled on the host from the scion source the jail runs: set
`SCION_VERSION` to your `scion.version`, or `SCION_SRC=/path/to/scion` to the checkout your
`scion.source` names (the default is the pin the shipped examples use). The slim base records it
in a `scion_version` label. Then point `manager.image` (and any worker `image:`) at
`lever.local/lever-claude-slim`, `lever apply`, and recreate the agents on it with `lever up --fresh
--keep-conversation`. The full image stays the default and nothing changes for an instance that
does not opt in.

What the slim image leaves out, on purpose: `sudo` (the agent has no root in its container, so it
cannot `apt-get install` at run time), `gh` (lever's agents reach GitHub through the
[github tool](/github-tool/)), chromium and the Playwright and Chrome DevTools tools, gcloud, gcsfuse, Go, kubectl,
golangci-lint, bun, oh-my-zsh and scion's in-container firewall script (lever enforces egress in
the jail). It also lacks the everyday tools scion's base adds, which neither scion's runtime nor
lever uses but agents often do: curl, wget, jq, ripgrep, less, make, g++, vim, rsync, unzip,
openssh-client and the network utilities. An agent that needs a toolchain gets it from an instance Dockerfile `FROM
lever.local/lever-claude-slim:<arch>`, as below.

**Checking an image: `make image-contract`.** `image/contract/check-image.sh` checks a built image
against what lever and scion rely on: the entrypoint (`sciontool init --`), the `scion` user (uid
and gid 1000, its home and shell), the labels doctor reads, sciontool, scion, the pinned tmux,
python3, git, the terminfo and timezone data, lever's binaries for the right arch, the boot hook,
`/etc/claude-code`, Claude Code at the labelled version (read from its `package.json`; it never
runs `claude`), lever's environment variables and the commands lever's probes use. It then starts
a tmux session shaped like scion's and checks that `lever-agent claude-ready` writes the marker
lever's manager gate waits for. It runs the image with no network and removes the container.

```sh
make image-contract                                    # lever.local/lever-claude-slim:<arch>
make image-contract IMAGE=lever.local/lever-claude:arm64
LEVER_IMAGE_MAX_MB=1600 make image-contract            # also fail an image above 1.6 GB
```

It works on either flavor, and on an instance image built from one (`CONTAINER_CLI=podman` for
podman). Run it after every image build; it is the check that the slim image still has everything
the full one gives lever.

**Extending the image for your instance.** The generic image is minimal: scion's harness plus
lever's binaries and boot hook. If your agents need a language toolchain or a project CLI, write an
instance Dockerfile `FROM lever.local/lever-claude:<arch>` that adds it, and point `manager.image` (and any
worker `image:`) at your tag. If your added layer does root-level work
under `/home/scion`, end it by re-running `RUN chown -R scion:scion /home/scion` and `USER scion`.
The jail runs rootless podman, where a root-owned home is unwritable by the agent and silently
breaks its boot hook. Keep instance-specific tooling in that layer, not in the framework image.
An image built `FROM lever.local/lever-claude:<arch>` inherits its labels (`claude_code_version`,
`lever_version`, `lever_jail_abi`), so doctor reads them from your tag with no extra step; rebuild
it after each `make lever-image`. An instance Dockerfile that copies the binaries in itself (`make
lever-image-bins`) sets the labels itself: `LABEL lever_version="..."` with the value `make -s
lever-image-version` prints, and `LABEL lever_jail_abi="..."` with the value `make -s
lever-image-jail-abi` prints.
