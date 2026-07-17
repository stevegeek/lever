---
title: Getting started
nav_order: 2
---
# Getting started

This walks you from nothing to a running lever application: a **manager** agent that dispatches
work to a **worker** (an agent scoped to its own subdirectory), all inside a jail that contains the
whole stack. The bundled
[`examples/hello-worker`](https://github.com/stevegeek/lever/tree/main/examples/hello-worker) is the
worked example.

The walkthrough is split across three pages; step numbers are continuous:

| Steps | Page |
|---|---|
| 1, 1a — build `lever` and the agent image | [Install & build the image](/getting-started/install/) |
| 2–6, 8 — configure, bring up, dispatch a worker, lifecycle | [First run](/getting-started/first-run/) |
| 7 — give agents MCP servers (ambient vs brokered) | [Give agents MCP tools](/getting-started/mcp-tools/) |

## What you'll end up with

```
your machine
└── OrbStack isolated machine  "lever-hello-worker"   (the jail)
    ├── rootless podman + scion hub (loopback)
    ├── manager container        ← edits your tree in place, dispatches workers
    └── worker container         ← runs the dispatched task
```

The jail is the security boundary. Your project tree is bind-mounted **in place**, so agents edit
the real files; there's no copy or sync. See [security-model.md](/security-model/) for what
the jail does and doesn't protect.

## Prerequisites

- **macOS on Apple Silicon** with [OrbStack](https://orbstack.dev) running. (OrbStack is the
  validated backend today.)
- **Go 1.26+** — both to build the binaries and on `PATH` at runtime: `lever apply`/`lever up`
  cross-compile the pinned Scion engine into the jail, so they shell out to `go`. If you manage Go
  with a version manager (asdf, mise), make sure the **real toolchain** is resolvable, not just a
  shim — a shim that isn't initialised in the non-interactive sub-process fails with
  `resolve go toolchain (is go on PATH?): exit status 126`. The fix is to put the actual toolchain
  bin on `PATH`, e.g. `export PATH="$HOME/.asdf/installs/golang/1.26.4/go/bin:$PATH"` (adjust the
  version); `go version` should print from that path.
- **The agent image** `scionlocal/lever-claude:<arch>` on your host Docker (images are tagged by
  architecture — `:arm64` / `:amd64` — so one host can cross-build both without clobbering; a tagless
  `manager.image` in the config auto-resolves to the jail's arch). `lever apply` loads it into the
  jail; it can't be pulled from inside (egress is locked down). If you don't have it yet, build it —
  one command once you have scion's base image:

      make lever-image                    # builds for your host arch (default arm64)
      # make lever-image LEVER_IMAGE_ARCH=amd64   # cross-build for an amd64 server

  This cross-compiles the in-jail binaries and builds `scionlocal/lever-claude:<arch>` FROM scion's
  stock `scion-claude:<arch>`. See
  [building the agent image](/getting-started/install/#1a-build-the-agent-image) for the
  scion-base prerequisite and how instances extend the image. Confirm with
  `docker images | grep scionlocal/lever-claude`.
- **A Claude OAuth token** in a file (mint with `claude setup-token`) for this subscription demo.
  Point `manager.credential_file` at it. Use a least-privilege token; in subscription mode it is
  projected into the agent containers (see the security doc).

## Where to go next

- [config-reference.md](/reference/config/), every config key, defaults, conventions.
- [security-model.md](/security-model/), trust boundaries, the threat model, and the
  credential flow.
- `examples/two-agents-comms` and `examples/multi-project`, richer topologies.
