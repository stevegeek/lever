# Contributing

Developer notes for working on lever itself. For using lever, start at the
[README](README.md) and [getting started](docs-site/_guides/getting-started.md).

## Repository layout

| Path | What |
|---|---|
| `cmd/lever` | Host control plane: provisioning and lifecycle (`up`, `apply`, `stop`, `doctor`, ...). |
| `cmd/lever-manager` | In-jail orchestration CLI the manager agent runs (`agent`, `msg`, `watch`). |
| `cmd/lever-agent` | In-jail capability helper. Baked into the agent image; the container pre-start hook runs it to enrol the agent's mTLS identity and serve the capability MCP tool. `cmd/lever-agent/scionhook/` holds that `pre-start` shell hook (copied into the image by `make lever-image`) and a test that pins its contract. |
| `cmd/lever-tool-db` | Reference first-party capability tool (optional). |
| `cmd/lever-tool-github` | Host-side broker tool: pushes an agent's git bundle to one `agent/*` branch with a short-lived GitHub App token; opt-in PR, merge and release operations (`-ops`). |
| `cmd/lever-tool-whisper` | Host-side broker tool: runs whisper.cpp's `whisper-server` for the chat page's dictation (over a Unix socket) and an optional agent `transcribe` operation. |
| `cmd/lever-tool-tts` | Host-side broker tool: runs sherpa-onnx's `sherpa-onnx-offline-tts` once per text for the chat page's read-aloud with server voices (over a Unix socket). |
| `captool/` | Public SDK for first-party capability tools (independent token verification + backstop). Imported by `examples/`; the only non-`internal` package. |
| `internal/` | Shared packages for all eight binaries (listed below). |
| `internal/agent` | In-jail lever-agent core: keypair, enrolment, capability MCP server, loopback gateway, token renewal. |
| `internal/agentledger` | Host record of each contact message the broker authorized an agent to send. |
| `internal/apply` | `lever apply`/`up` bring-up: the pure `Plan` and the `Run` executor. |
| `internal/backend` | The containment `Backend` contract. `types` (stdlib-only leaf: the data types the contract carries for its guest-side verbs), `common` (shared machinery for reach-the-guest backends), `guest` (the in-guest half of provisioning: argv-prefix transport, in-guest scripts, installing what `internal/provision` built, scion state and hub-login settings surgery), `lima`, `orbstack`, `registry` (name → constructor, jail argv, and the `Candidates` guarantee matrix), `backendtest` (shared fakes for backend tests). |
| `internal/buildstamp` | The build's provenance every lever binary reports: the commit (stamped with `-ldflags -X` by the Makefile and the release build, else Go's VCS stamp), dirty state and binary hash. Stdlib only. |
| `internal/bridge` | Poll-based scion-event → events-file bridge the manager watches. |
| `internal/broker` | The capability broker: enrol, request/delegate, MCP gateway, llm proxy, directives. `registry` (tool/operation registry and constraint mapping), `rules` (obtain/delegate policy), `brokertest` (test-only: a broker under test with its CA, server and mTLS clients). |
| `internal/brokerctl` | Host-side controller for the broker daemon: keys, `serve`, tool supervisor, stop. |
| `internal/chatfiles` | The chat page's file exchange (`remote.files`): where files live, allowed names, store/hash/copy. |
| `internal/chatledger` | Host record of the web chat posts the remote proxy forwarded (verified web chat). |
| `internal/cap` | Capability primitives: `ca` (instance CA, mTLS, rotation), `token` (Ed25519 capability tokens). |
| `internal/cli` | Shared by both binaries: the release `Version` constant and the `version` command. |
| `internal/cli/host` | Cobra commands for `lever` (host control plane). |
| `internal/cli/manager` | Cobra commands for `lever-manager` (runs inside the agent container). |
| `internal/cli/clitest` | Test-only helpers shared by `cli/host` and `cli/manager` tests. |
| `internal/daemon` | Pid-file and listener bookkeeping shared by the host-side daemons. |
| `internal/config` | `lever.yaml` schema, loading and validation. |
| `internal/egress` | Jail egress allowlist as iptables/ip6tables rules. |
| `internal/fileledger` | Host record of chat-page uploads and agent-shared files, with sha256 and size. |
| `internal/fsutil` | File helpers (`WriteFileAtomic`, tree-confined access, `InsideByIdentity`); stdlib only, plus `golang.org/x/sys/unix` for the identity check. |
| `internal/ghpush` | Host side of `lever-tool-github`: request checks, bundle import into a host mirror, the push, and the opt-in PR and release operations over the GitHub REST API. |
| `internal/hostledger` | Safe JSON-lines file handling shared by the host-side ledgers. |
| `internal/proc` | The single seam to external commands (`Runner`, `FakeRunner`). |
| `internal/httpjson` | JSON-over-HTTP client helper (`Post`, typed decode, the one error shape) used by every broker caller. |
| `internal/hubapi` | Minimal scion Hub REST client for what the scion CLI does not expose. |
| `internal/jail` | `JailRunner`: a `proc.Runner` that runs commands inside the jail. |
| `internal/mcp` | JSON-RPC 2.0 framing and the `tools/call` projection shared by the agent MCP server and `captool`; `MaxBodyBytes`. |
| `internal/opsig` | Operator-directive signature protocol. |
| `internal/provision` | Host-side build pipelines that produce a local artefact for the guest: `scionbin` (scion binary: prebuilt/source/pinned module, ELF arch check), `webassets` (scion's SPA via npm, cached per source digest; the node probe `lever doctor` shares), and the shared `GoBinary` resolver. |
| `internal/retry` | `retry.Until`: the one bounded poll loop every wait-for-ready path uses. |
| `internal/hubauth` | lever's hub assertion key and ES256 signer: the identities lever asserts to the hub (admin, controller, remote sign-ins) and the reserved addresses. |
| `internal/remoteproxy` | `lever remote serve`: the authenticating reverse proxy, the assertion sign-in, chat page, file exchange and web push. |
| `internal/scion` | Client for the scion CLI (bring-up, lifecycle, hub tokens) and every predicate over scion's output wording. `layout` (pure: scion's `~/.scion` paths, settings.yaml keys and YAML edit helpers). |
| `internal/servicehost` | `lever service-host`: the service host's config, its stdio bridge and upstream proxy behind pinned mutual TLS with a per-tool ceiling, and the pairing, transport and probe the broker and `lever doctor` use. |
| `internal/sentledger` | Host record of every message lever itself sends to an agent. |
| `internal/sessionrec` | Host record of when each agent's session last started fresh, and with which skill text. |
| `internal/skills` | Framework-authored SKILL.md files scaffolded into instances. |
| `internal/state` | The `.lever-state` directory layout and its file helpers: JSON state, 0600 secrets, pid files, the remote-proxy stamp. |
| `internal/termsafe` | Makes guest-supplied strings safe to print on the operator's terminal. |
| `internal/testutil` | Stdlib-only assertion helpers shared by test packages (`WantErrIs`, `WantErrContaining`). |
| `internal/voice` | Speech on the host. Speech to text: the pinned Whisper models and `lever voice fetch`, the `whisper-server` supervisor and adapter, the dictation socket and the agent operation of `lever-tool-whisper`. Text to speech: the pinned speech model archives (unpacked with a manifest of every file), the sherpa-onnx adapter, the per-text runner and the speech socket of `lever-tool-tts`. |
| `internal/webpush` | Web Push (RFC 8030/8291/8292) sender for the chat page's notifications, stdlib only. |
| `internal/wire` | Leaf package: agent⇄broker request/response types, route paths and bootstrap material. Imports no other lever package. |
| `image/lever-claude` | Build context for the generic agent image (`lever.local/lever-claude:<arch>`), and the lever layer of the slim one. |
| `image/scion-slim` | The slim image's base (`make lever-image-slim`): what scion needs in an agent container, in place of `scion-claude`. |
| `image/contract` | The agent-image contract check (`make image-contract IMAGE=...`) and its tests. |
| `examples/` | Runnable instances used by docs and tests. |
| `docs-site/` | Jekyll site for lever.to (`_guides`, `_reference`). |
| `tools/test` | Live end-to-end test scripts and the fake LLM upstream. |
| `docs/` | Design specs, plans, and audits. Historical records, not user docs. |

All binaries are built from one Go module (Go 1.26.9+, the `go` line of `go.mod`). The three in-jail binaries are cross-compiled
for `linux/<arch>` with `CGO_ENABLED=0` and copied to `/usr/local/bin` in the agent image.

scion itself is too large to embed (about 40 MB per arch compressed). The release build runs
`go run ./internal/provision/scionbin/genrelease -version <v>` first: it builds scion at
`scionbin.ReleasePin` for linux/amd64 and linux/arm64 with the same flags as the apply-time build
(plus `CGO_ENABLED=0`), writes the gzip-compressed binaries to `build/scion-release/`, which
goreleaser uploads as release assets, and writes their sha256 and sizes to
`internal/provision/scionbin/release/manifest.json`, which `go:embed` carries in `lever`. Such a
`lever` downloads the asset for a matching `scion.version` from its own release and installs it
only if it matches the embedded digests. Without that step (`make install`, `go build`) lever
embeds no manifest and builds scion as before. Do not run `genrelease` in a checkout you build
`lever` from for use: its manifest would name assets that are not on any release.

After the release assets are uploaded, goreleaser writes the Homebrew cask (`homebrew_casks` in
`.goreleaser.yaml`) and pushes it to `Casks/lever.rb` in
[stevegeek/homebrew-tap](https://github.com/stevegeek/homebrew-tap). The workflow's
`GITHUB_TOKEN` cannot write to another repository, so this needs a repository secret
`HOMEBREW_TAP_TOKEN` on the lever repo: a fine-grained personal access token limited to the
`homebrew-tap` repository, with only **Contents: read and write**. Anyone holding it can change
what `brew install` runs on users' machines, so keep it to that one repository and renew it before
it expires (a fine-grained token lasts at most a year). Without the secret, or for a prerelease tag
(`v1.2.3-rc1`), goreleaser skips the push and the release still succeeds. If the push fails after
the release is published, the job fails but the release stands; do not re-run the job (the assets
already exist). Fix the token and update `Casks/lever.rb` in the tap by hand: the new `version`
and, for each of the four archives, its `sha256` from the release's `checksums.txt`.

### Bumping the scion pin

Change `ReleasePin` in `internal/provision/scionbin/release.go` and the `version:` of every
`examples/*/lever.yaml` together (`TestExamplesPinTheReleaseScion` fails otherwise), then the pin
in the docs (`grep -rn <old pin> docs-site`), the Makefile's `SCION_VERSION` default and the
tests that name it (`grep -rn <old pin> .`). The next release ships the new pin prebuilt.

The agent image carries sciontool (and, in the slim image, scion) built from the pin, so a bump
needs a new image: bump `JailABI` (see below) so `lever doctor` asks for the rebuild. Run
`make test-lima-stack-e2e` on the new pin: `make test-lima-e2e` does not start scion. With
`SAVE_IMAGE_TAR=<file>` it keeps the image archive it built, for `make test-lima-hubauth-e2e
IMAGE_TAR=<file>`, which migrates an instance the older release brought up on its pin. With
`IMAGE_CACHE=<file>` it reuses the archive an earlier run saved there when the in-jail
fingerprint (`internal/jailabi/fingerprint.txt`), the pins, the arch and the image name are
unchanged, and otherwise builds the image and saves it there, so a rerun skips the in-guest build.

## Build

```bash
make install          # host `lever`, `lever-tool-github`, `lever-tool-whisper`, `lever-tool-tts` → $PREFIX (default ~/.local/bin)
make lever-image      # cross-compile in-jail binaries + docker build the agent image (LEVER_IMAGE_ARCH, default: host Go arch)
make lever-image-bins # cross-compile in-jail binaries into an instance's image build context only
make lever-image-slim # the slim agent image (SCION_VERSION or SCION_SRC: the scion the jail runs)
make image-contract   # check a built image (IMAGE=..., default the slim one) against what lever and scion need
```

Never build a binary into the tree: `go build ./cmd/x` writes `./x` into the current directory.
Build to a scratch path (`go build -o /tmp/x ./cmd/x`), or use `go vet` and `go test`. Stage files
by name, not with `git add -A` or `git add .`, and check `git diff --cached --stat` before each
commit.

Machine-specific paths (`LEVER_INSTANCE`, `LEVER_IMAGE_CTX`, `PREFIX`) go in an untracked
`local.mk`; see `local.mk.example`. `make lever-image` refuses to overwrite an existing image tag
unless `LEVER_IMAGE_FORCE=1`.

## Test

```bash
go test ./...          # unit and acceptance-fixture tests
make test-integration  # `-tags integration` tests; each skips unless its env/tooling is present
make test-apikey-e2e   # live api-key /llm path with a fake upstream; needs OrbStack + podman
make test-lima-e2e     # live lima backend gate; needs Lima >= 2.0
make test-lima-stack-e2e # live whole stack on the pinned scion (agent image, remote access); needs Lima >= 2.0
make test-lima-hubauth-e2e # live hub identities: migration from the older release, contacts while agents run; needs IMAGE_TAR
lever acceptance       # six-check live gate against a running instance
```

CI (`.github/workflows/ci.yml`) runs `gofmt -l .` (must be empty), `go vet ./...`, `go build ./...`
and `go test -race ./...`. A release is a `v*` tag; the release workflow refuses a tag that does not
match `const Version` in `internal/cli/root.go` or that is not on `main`.

### Live test plan for guest hardening and nested virt

The guest crash recovery, `guest_kernel` and the recovery after a hard VM kill (lever#179) are
unit tested only; these live checks remain. Run each on a disposable instance, one live e2e at a
time, and never on a VM whose agents you need.

1. **A fresh `nested_virt` VM.** After the first apply: the three files
   (`/etc/sysctl.d/99-lever-crash-recovery.conf`, `/etc/modprobe.d/lever-no-efi-pstore.conf`,
   `/etc/default/grub.d/99-lever-crash-recovery.cfg`), the four sysctls live, `efi_pstore` not
   loaded, apply's `jail: reboot pending:` lines, and `linux-generic-hwe-26.04` installed. Then
   `lever stop`, `lever up`: doctor's *guest crash recovery* and *guest kernel* rows are green and
   `/proc/cmdline` has `efi_pstore.pstore_disable=1`.
2. **Re-apply.** No `update-grub` and no apt run; record the time the hardening adds to apply.
3. **Re-apply under `egress: closed` without the HWE kernel** (remove the package first): measure
   the delay and check the warning says the egress is closed. Then make an install fail (open
   posture, mirrors unreachable) and check the stamp `.lever-state/hwe-install-failed.json`
   (0600), the next closed-posture apply's warning, and that it runs no apt.
4. **A production-like VM without `nested_virt`.** No kernel change; `update-grub` succeeds on the
   Lima cloud image.
5. **A guest panic.** `echo c > /proc/sysrq-trigger` in the guest, under qemu (and vz on a Mac if
   possible): the VM comes back or stops (record which, per driver). Then `lever up` logs
   "suspending and resuming", the conversation is intact, and the record is not refused.
6. **QEMU killed while the manager runs**, then `lever up`: the same checks as step 5.
7. **A crash-truncated transcript.** Truncate the last line of the manager's session JSONL, then
   resume: does `claude --continue` accept it?
8. **Doctor on OrbStack**: both rows say not applicable.
9. **Soak.** A nested VM under CPU and IO load for 24 h on the HWE kernel, watching the guest
   serial log (`~/.lima/<vm>/serial*.log`) for soft lockups.

Record results (no host names) in the operations guide's table of host/guest kernel pairs, and
update its "verified/unverified" notes.

### Refreshing the Lima guest image

`GuestImages` in `internal/backend/lima/template.go` pins the Ubuntu cloud image new Lima VMs boot:
one dated release directory (`releases/<codename>/release-YYYYMMDD/`, never the moving `release/`
alias) and one sha256 per architecture. A VM made from it starts at that serial's patch level, so
move the pin forward now and then:

1. Pick the newest `release-YYYYMMDD` directory under
   `https://cloud-images.ubuntu.com/releases/<codename>/`.
2. Check the signature of its `SHA256SUMS` (`SHA256SUMS.gpg`, the Ubuntu cloud image signing key
   `D2EB 4462 6FDD C30B 513D 5BB7 1A5D 6C4C 7DB8 7C81`) and copy the two `server-cloudimg`
   `amd64` and `arm64` `.img` digests from it. Never type a digest by hand.
3. Run `make test-lima-e2e` on the new pin.

Existing VMs are not affected: Lima reads the image only at create time.

### Bumping the jail ABI

`JailABI` in `internal/jailabi/jailabi.go` is the in-jail compatibility version. `make lever-image`
stamps it on the agent image as the `lever_jail_abi` label, and `lever doctor`'s `agent lever
version` row compares it with the host `lever`, so a release that changed no in-jail code needs no
image rebuild. The in-jail code is everything baked into the image: `cmd/lever-agent`,
`cmd/lever-manager`, `cmd/lever-tool-db`, the scion pre-start hook, `internal/agent`,
`internal/wire`, every package they import (`go list -deps`; external modules by version), the
image Dockerfile, and the Go version in `go.mod`. `internal/jailabi/fingerprint.txt` holds a hash
of those and lists what it covers. Go files are hashed as their token stream, so comments, blank
lines and spacing do not count, nor does the value of the release `Version` constant in
`internal/cli`; `//go:` directives (build constraints, `go:embed` and the like) do count. When `TestJailFingerprint` fails after your change:

1. add one to `JailABI`;
2. run `go test ./internal/jailabi -run TestJailFingerprint -update`;
3. commit `jailabi.go` and `fingerprint.txt` with the change.

The update refuses to record a new hash under the old number. Bump `JailABI` by hand, then run the
update, when the code is the same but its meaning on the wire changed. Two branches that each bump
it conflict in `fingerprint.txt`: after the merge, bump once more and run the update. Deleting
`fingerprint.txt` and running the update re-records it without a bump, so only do that when no
image has shipped with the current `JailABI`.

## Comments

A comment says what the code cannot: a non-obvious why, a constraint, a security assumption or
invariant, a gotcha, or a contract a caller must keep. Keep it short.

- Do not describe what the code does; name things so it reads on its own.
- Do not reference tickets or card numbers, issue or PR links, other projects, people, or private
  context.
- Do not write history ("it used to", "before this change", "renamed from"). Design reasoning and
  history go in the commit message, the PR text or the docs.
- Keep license headers, package doc comments, short doc comments on exported identifiers, tool
  directives (`//go:`, `//nolint`) and comments that state a security assumption.
- In tests, a comment may say what a non-obvious test proves.

## Release checklist

Before tagging, besides a green CI:

- `CHANGELOG.md` has the release's section, and `const Version` matches the tag.
- One live end-to-end run (`make test-lima-e2e`) on the release candidate.
- **A change to the agent's boot step** (`lever-agent boot`, the scion pre-start hook in
  `cmd/lever-agent/scionhook`, anything they read or write, or the scion pin) needs a live boot on a
  throwaway rootless instance, with the uid the hook ran as checked there (the container log's
  `sciontool init` lines and the owner of the files boot wrote), or the uid confirmed in the pinned
  scion source (`cmd/sciontool/commands/init.go`: `setupHostUser` runs before the pre-start hooks).
  Under rootless podman the hook runs as the agent user (uid 1000), not root. Two reviews once
  accepted "boot runs as root" without either check; a review that relies on the hook's uid must
  name which check it made.

## Docs

`docs-site/` builds with `cd docs-site && bundle exec jekyll build`. Docs describe the current
implementation only: no status notes, roadmap, or changelog content. Record changes in
`CHANGELOG.md`.
