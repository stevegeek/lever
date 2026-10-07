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
| `cmd/lever-tool-github` | Host-side broker tool: pushes an agent's git bundle to one `agent/*` branch with a short-lived GitHub App token. |
| `cmd/lever-tool-fizzy` | Host-side broker tool: a fixed set of operations on one Fizzy board, through the `fizzy` CLI. |
| `captool/` | Public SDK for first-party capability tools (independent token verification + backstop). Imported by `examples/`; the only non-`internal` package. |
| `internal/` | Shared packages for all six binaries (listed below). |
| `internal/agent` | In-jail lever-agent core: keypair, enrolment, capability MCP server, loopback gateway, token renewal. |
| `internal/agentledger` | Host record of each contact message the broker authorized an agent to send. |
| `internal/apply` | `lever apply`/`up` bring-up: the pure `Plan` and the `Run` executor. |
| `internal/backend` | The containment `Backend` contract. `types` (stdlib-only leaf: the data types the contract carries for its guest-side verbs), `common` (shared machinery for reach-the-guest backends), `guest` (the in-guest half of provisioning: argv-prefix transport, in-guest scripts, installing what `internal/provision` built, scion state and hub-login settings surgery), `lima`, `orbstack`, `registry` (name → constructor, jail argv, and the `Candidates` guarantee matrix), `backendtest` (shared fakes for backend tests). |
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
| `internal/fizzytool` | Host side of `lever-tool-fizzy`: board-confined operations through the `fizzy` CLI. |
| `internal/fsutil` | Stdlib-only file helpers (`WriteFileAtomic`). |
| `internal/ghpush` | Host side of `lever-tool-github`: request checks, bundle import into a host mirror, the push. |
| `internal/hostledger` | Safe JSON-lines file handling shared by the host-side ledgers. |
| `internal/proc` | The single seam to external commands (`Runner`, `FakeRunner`). |
| `internal/httpjson` | JSON-over-HTTP client helper (`Post`, typed decode, the one error shape) used by every broker caller. |
| `internal/hubapi` | Minimal scion Hub REST client for what the scion CLI does not expose. |
| `internal/jail` | `JailRunner`: a `proc.Runner` that runs commands inside the jail. |
| `internal/mcp` | JSON-RPC 2.0 framing and the `tools/call` projection shared by the agent MCP server and `captool`; `MaxBodyBytes`. |
| `internal/opsig` | Operator-directive signature protocol. |
| `internal/provision` | Host-side build pipelines that produce a local artefact for the guest: `scionbin` (scion binary: prebuilt/source/pinned module, ELF arch check), `webassets` (scion's SPA via npm, cached per source digest; the node probe `lever doctor` shares), `loginfwd` (the remote-access login forwarder — see below), and the shared `GoBinary` resolver. |
| `internal/retry` | `retry.Until`: the one bounded poll loop every wait-for-ready path uses. |
| `internal/remoteproxy` | `lever remote serve`: the authenticating reverse proxy, local OIDC provider, chat page, file exchange and web push. |
| `internal/scion` | Client for the scion CLI (bring-up, lifecycle, hub tokens) and every predicate over scion's output wording. `layout` (pure: scion's `~/.scion` paths, settings.yaml keys, the oidc_login block and YAML edit helpers). |
| `internal/sentledger` | Host record of every message lever itself sends to an agent. |
| `internal/sessionrec` | Host record of when each agent's session last started fresh, and with which skill text. |
| `internal/skills` | Framework-authored SKILL.md files scaffolded into instances. |
| `internal/state` | The `.lever-state` directory layout and its file helpers: JSON state, 0600 secrets, pid files, the remote-proxy stamp. |
| `internal/termsafe` | Makes guest-supplied strings safe to print on the operator's terminal. |
| `internal/testutil` | Stdlib-only assertion helpers shared by test packages (`WantErrIs`, `WantErrContaining`). |
| `internal/webpush` | Web Push (RFC 8030/8291/8292) sender for the chat page's notifications, stdlib only. |
| `internal/wire` | Leaf package: agent⇄broker request/response types, route paths and bootstrap material. Imports no other lever package. |
| `image/lever-claude` | Build context for the generic agent image (`scionlocal/lever-claude:<arch>`). |
| `examples/` | Runnable instances used by docs and tests. |
| `docs-site/` | Jekyll site for lever.to (`_guides`, `_reference`). |
| `tools/test` | Live end-to-end test scripts and the fake LLM upstream. |
| `docs/` | Design specs, plans, and audits. Historical records, not user docs. |

All binaries are built from one Go module (Go 1.26+). The three in-jail binaries are cross-compiled
for `linux/<arch>` with `CGO_ENABLED=0` and copied to `/usr/local/bin` in the agent image.

One more in-jail program is NOT built that way: `lever-login-forward`, the remote-access login
forwarder (`internal/provision/loginfwd/main`). It runs in the jail VM itself, not in an agent
container, so the agent image cannot carry it. `make install` (through `make loginfwd-prebuilt`) and the
release build run `go run ./internal/provision/loginfwd/genprebuilt` first, which
cross-compiles it for linux/amd64 and linux/arm64 into `internal/provision/loginfwd/prebuilt/`,
where `go:embed` carries it in `lever`; such a `lever` needs no Go toolchain for remote access. A
`lever` built without that step (`go build`, `go install`) falls back to cross-compiling the
embedded source on the operator host during `lever apply` of an instance with `remote.enabled`,
which needs a Go toolchain.

## Build

```bash
make install          # host `lever`, `lever-tool-github`, `lever-tool-fizzy` → $PREFIX (default ~/.local/bin)
make lever-image      # cross-compile in-jail binaries + docker build the agent image (LEVER_IMAGE_ARCH, default: host Go arch)
make lever-image-bins # cross-compile in-jail binaries into an instance's image build context only
```

Machine-specific paths (`LEVER_INSTANCE`, `LEVER_IMAGE_CTX`, `PREFIX`) go in an untracked
`local.mk`; see `local.mk.example`. `make lever-image` refuses to overwrite an existing image tag
unless `LEVER_IMAGE_FORCE=1`.

## Test

```bash
go test ./...          # unit and acceptance-fixture tests
make test-integration  # `-tags integration` tests; each skips unless its env/tooling is present
make test-apikey-e2e   # live api-key /llm path with a fake upstream; needs OrbStack + podman
make test-lima-e2e     # live lima backend gate; needs Lima >= 2.0
lever acceptance       # six-check live gate against a running instance
```

CI (`.github/workflows/ci.yml`) runs `gofmt -l .` (must be empty), `go vet ./...`, `go build ./...`
and `go test -race ./...`. A release is a `v*` tag; the release workflow refuses a tag that does not
match `const Version` in `internal/cli/root.go` or that is not on `main`.

## Docs

`docs-site/` builds with `cd docs-site && bundle exec jekyll build`. Docs describe the current
implementation only: no status notes, roadmap, or changelog content. Record changes in
`CHANGELOG.md`.
