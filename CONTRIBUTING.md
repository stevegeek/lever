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
| `captool/` | Public SDK for first-party capability tools (independent token verification + backstop). Imported by `examples/`; the only non-`internal` package. |
| `internal/` | Shared packages for all four binaries (listed below). |
| `internal/agent` | In-jail lever-agent core: keypair, enrolment, capability MCP server, loopback gateway, token renewal. |
| `internal/apply` | `lever apply`/`up` bring-up: the pure `Plan` and the `Run` executor. |
| `internal/backend` | The containment `Backend` contract and the `Candidates` guarantee matrix. `common` (shared machinery for reach-the-guest backends), `guest` (in-guest provisioning: scion checkout/binary, hub login, web assets), `guest/loginfwd` (the guest-side login forwarder binary), `lima`, `orbstack`, `registry` (name → constructor, jail argv). |
| `internal/bridge` | Poll-based scion-event → events-file bridge the manager watches. |
| `internal/broker` | The capability broker: enrol, request/delegate, MCP gateway, llm proxy, directives. `registry` (tool/operation registry and constraint mapping), `rules` (obtain/delegate policy). |
| `internal/brokerctl` | Host-side controller for the broker daemon: state dir, keys, `serve`, tool supervisor, health. |
| `internal/cap` | Capability primitives: `ca` (instance CA, mTLS, rotation), `token` (Ed25519 capability tokens). |
| `internal/cli` | Cobra commands for `lever` and `lever-manager`. |
| `internal/config` | `lever.yaml` schema, loading and validation. |
| `internal/egress` | Jail egress allowlist as iptables/ip6tables rules. |
| `internal/exec` | The single seam to external commands (`Runner`, `FakeRunner`). |
| `internal/hubapi` | Minimal scion Hub REST client for what the scion CLI does not expose. |
| `internal/jail` | `JailRunner`: an `exec.Runner` that runs commands inside the jail. |
| `internal/opsig` | Operator-directive signature protocol. |
| `internal/remoteproxy` | `lever remote serve`: the authenticating reverse proxy and local OIDC provider. |
| `internal/scion` | Client for the scion CLI (bring-up, lifecycle, hub tokens). |
| `internal/skills` | Framework-authored SKILL.md files scaffolded into instances. |
| `internal/wire` | Agent⇄broker wire types (bootstrap material). |
| `image/lever-claude` | Build context for the generic agent image (`scionlocal/lever-claude:<arch>`). |
| `examples/` | Runnable instances used by docs and tests. |
| `docs-site/` | Jekyll site for lever.to (`_guides`, `_reference`). |
| `tools/test` | Live end-to-end test scripts and the fake LLM upstream. |
| `docs/` | Design specs, plans, and audits. Historical records, not user docs. |

All binaries are built from one Go module (Go 1.26+). The three in-jail binaries are cross-compiled
for `linux/<arch>` with `CGO_ENABLED=0` and copied to `/usr/local/bin` in the agent image.

## Build

```bash
make install          # host `lever` → $PREFIX (default ~/.local/bin)
make lever-image      # cross-compile in-jail binaries + docker build the agent image (LEVER_IMAGE_ARCH=arm64)
make lever-image-bins # cross-compile in-jail binaries into an instance's image build context only
```

Machine-specific paths (`LEVER_INSTANCE`, `LEVER_IMAGE_CTX`, `PREFIX`) go in an untracked
`local.mk`; see `local.mk.example`. `make lever-image` refuses to overwrite an existing image tag
unless `LEVER_IMAGE_FORCE=1`.

## Test

```bash
go test ./...          # unit and acceptance-fixture tests
make test-apikey-e2e   # live api-key /llm path with a fake upstream; needs OrbStack + podman
make test-lima-e2e     # live lima backend gate; needs Lima >= 2.0
lever acceptance       # six-check live gate against a running instance
```

## Docs

`docs-site/` builds with `cd docs-site && bundle exec jekyll build`. Docs describe the current
implementation only: no status notes, roadmap, or changelog content. Record changes in
`CHANGELOG.md`.
