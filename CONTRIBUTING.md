# Contributing

Developer notes for working on lever itself. For using lever, start at the
[README](README.md) and [getting started](docs-site/_guides/getting-started.md).

## Repository layout

| Path | What |
|---|---|
| `cmd/lever` | Host control plane: provisioning and lifecycle (`up`, `apply`, `stop`, `doctor`, ...). |
| `cmd/lever-manager` | In-jail orchestration CLI the manager agent runs (`agent`, `msg`, `watch`). |
| `cmd/lever-agent` | In-jail capability helper. Baked into the agent image; the container pre-start hook (`cmd/lever-agent/scionhook/pre-start`) runs it to enrol the agent's mTLS identity and serve the capability MCP tool. |
| `cmd/lever-tool-db` | Reference first-party capability tool (optional). |
| `internal/` | Shared packages for all four binaries: `apply` (plan/run), `backend` (orbstack, lima), `broker` (capability broker), `cap` (token), `cli`, `config`, `egress`, `jail`, `scion`, `remoteproxy`, `opsig` (operator directives), and others. |
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
