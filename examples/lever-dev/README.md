# lever-dev: develop Lever inside a Lever jail

This runbook is for an x86_64 Linux host with KVM (the package commands are for Arch Linux; use your distribution's equivalents). The jail is a Lima VM with nested KVM.
The manager develops Lever in `jail-src/`. It pushes through the `github` host tool and uses the Lever board through the `fizzy` host tool.

Rules for the host:

- Never `cd` into `~/lever-dev/jail-src` on the host. Agent-written git config and hooks can run there.
- Build host binaries only from the clones in `~/lever-release`.

## 1. Host packages

1. `sudo pacman -S --needed qemu-base git go nodejs npm docker`
2. Install Lima from the release tarball:
   ```
   VER=2.2.1   # the Lima version you pin
   cd "$(mktemp -d)"
   curl -fsSLO https://github.com/lima-vm/lima/releases/download/v$VER/lima-$VER-Linux-x86_64.tar.gz
   curl -fsSLO https://github.com/lima-vm/lima/releases/download/v$VER/SHA256SUMS
   sha256sum -c --ignore-missing SHA256SUMS
   sudo tar -xzf lima-$VER-Linux-x86_64.tar.gz -C /usr/local
   ```
3. Confirm nested virtualization: `cat /sys/module/kvm_amd/parameters/nested` (AMD) or `cat /sys/module/kvm_intel/parameters/nested` (Intel) must print `1` or `Y`.

## 2. Host-only build clones

1. `mkdir -p ~/lever-release && cd ~/lever-release`
2. `git clone --branch vX.Y.Z https://github.com/stevegeek/lever`
3. `git clone https://github.com/GoogleCloudPlatform/scion && git -C scion checkout <pin>`
4. `cd ~/lever-release/scion && image-build/scripts/build-images.sh --target harnesses`
5. `cd ~/lever-release/lever && make install`
6. `make lever-image` (the host arch is amd64 by default)
7. Never build from `~/lever-dev/jail-src`.

## 3. Instance root

1. `mkdir -p ~/lever-dev/{secrets,jail-src} && chmod 700 ~/lever-dev/secrets`
2. Copy `lever.yaml`, `instructions.md` and `image/` from `~/lever-release/lever/examples/lever-dev/` to `~/lever-dev/`.
3. Fill the placeholders in `~/lever-dev/lever.yaml`: `YOU`, `OWNER`, `APP_ID`, `INSTALLATION_ID`, `ACCOUNT_ID`, `BOARD_ID`, `YOUR-HOST.YOUR-TAILNET`, the email. Keep `scion.version` equal to the pin of the installed release.
4. `git clone https://github.com/OWNER/lever ~/lever-dev/jail-src/lever` (your fork)
5. `git clone https://github.com/GoogleCloudPlatform/scion ~/lever-dev/jail-src/scion`

## 4. Hazard: git inside jail-src

The agents write to `jail-src/`. Git run on the host in that tree can execute agent-written config or hooks.

1. Never `cd` into `~/lever-dev/jail-src` on the host.
2. Turn off the shell-prompt git status for that path. Exclude the path in the zsh or bash prompt.
3. Do not open the folder in an IDE. In VS Code, set `git.ignoredRepositories` for it.
4. Deny-list the path for the work agents on the host.

## 5. Credentials

1. Run `claude setup-token`. Save the token to `~/lever-dev/secrets/claude-oauth-token` and run `chmod 600` on it.
2. Create a GitHub App: Settings, Developer settings, GitHub Apps, New.
   - No webhook.
   - Repository permissions: Contents: Read and write. Nothing else.
   - Install it on your fork (`OWNER/lever`) only.
3. Note the App ID and the installation ID. Put them in `lever.yaml`.
4. Generate a private key. Save it as `~/lever-dev/secrets/github-app.pem` and run `chmod 600` on it.
5. Install the fizzy CLI from the official release (`https://github.com/basecamp/fizzy-cli/releases`). Verify its checksum. Put it in `/usr/local/bin/fizzy`. Install fizzy 4.x: the tool refuses other major versions.
6. Create a new Fizzy personal access token only for lever-dev: app.fizzy.do, Profile, API, Personal access tokens. Save it as `~/lever-dev/secrets/fizzy-token` and run `chmod 600` on it.
7. Put your Fizzy account id and board id in `lever.yaml` (`ACCOUNT_ID`, `BOARD_ID`).
8. Do not keep a `~/.fizzy.yaml` on the host: the tool refuses to start when one exists above its state dir.

## 6. Rulesets

1. Repo Settings, Rules, New branch ruleset "main":
   - Target `main`.
   - Require a pull request.
   - Block force pushes.
   - Restrict deletions.
   - Bypass: Repository admin.
2. New tag ruleset "release tags":
   - Target `v*`.
   - Restrict creations, updates and deletions.
   - Bypass: Repository admin.
3. Repo Settings, Actions, General, Workflow permissions: select "Read repository contents and
   packages permissions". CI runs on `agent/**` pushes, so the `GITHUB_TOKEN` must be read-only.

The tool keeps one mirror per repo under the `-state` directory. The mirror grows, because the tool
sets `gc.auto=0`. Run `git --git-dir=<mirror> gc` now and then, or delete the mirror directory. The
tool creates it again on the next push.

## 7. Image

1. Read `LIMA_SHA256` for the x86_64 tarball from the release `SHA256SUMS`.
2. Pick the Playwright version: `npm view playwright version`.
3. `cd ~/lever-dev/image && LIMA_VERSION=... LIMA_SHA256=... PLAYWRIGHT_VERSION=... ./build.sh`

## 8. Bring-up

1. `cd ~/lever-dev && lever apply && lever up && lever doctor`
2. All rows must be green. The `nested virt` row must be green.

## 9. Chat page

1. Run `tailscale serve` for port 8445 as in the remote-access guide.
2. Open `base_url` from another device on the tailnet.
