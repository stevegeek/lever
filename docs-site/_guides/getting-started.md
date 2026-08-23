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
the real files. See the [security model](/security-model/) for what the jail does and does not
protect.

## Prerequisites

- **macOS on Apple Silicon** with [OrbStack](https://orbstack.dev) running (`backend: orbstack`,
  used by this walkthrough). A `lima` backend also exists for macOS and Linux; see
  [backends](/reference/backends/).
- **Go 1.26+** — to build the binaries, and on `PATH` at runtime: `lever apply`/`lever up`
  cross-compile the pinned Scion engine into the jail. A version-manager shim (asdf, mise) that is
  not initialised in the non-interactive sub-process fails with `resolve go toolchain (is go on
  PATH?): exit status 126`; put the real toolchain bin on `PATH` (see
  [troubleshooting](/operations/#troubleshooting-quick-table)).
- **The agent image** `scionlocal/lever-claude:<arch>` on your host Docker. `lever apply` loads it
  into the jail; it cannot be pulled from inside. Build it with `make lever-image`; see
  [step 1a](/getting-started/install/#1a-build-the-agent-image). Confirm with
  `docker images | grep scionlocal/lever-claude`.
- **A Claude OAuth token** in a file (mint with `claude setup-token`) for this subscription demo.
  Point `manager.credential_file` at it. Use a least-privilege token; in subscription mode it is
  projected into the agent containers ([security model §6](/security-model/credentials/)).

## Where to go next

- [config-reference.md](/reference/config/), every config key, defaults, conventions.
- [security-model.md](/security-model/), trust boundaries, the threat model, and the
  credential flow.
- `examples/two-agents-comms` and `examples/multi-project`, richer topologies.
