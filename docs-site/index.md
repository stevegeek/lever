---
layout: home
title: Lever
hero:
  name: Lever
  text: Autonomous AI agents in a sealed jail.
  image:
    src: /assets/logo.png
    alt: Lever
  tagline: >-
    Lever seals your agents inside a jail with no path to your host, your
    secrets, or your network. A host-side broker then grants and gates every
    capability they get: which tools, which operations, which credentials.
  actions:
    - theme: brand
      text: Get started
      link: /getting-started/
    - theme: alt
      text: Why Lever
      link: /introduction/
features:
  - title: Containment, not trust
    details: >-
      Scion, the container runtime, and every agent run inside one isolated
      machine with rootless podman and an egress allowlist. Host secrets and the
      LAN simply aren't reachable; the broker stays host-side, outside the jail.
  - title: The key never lands in the container
    details: >-
      By default the broker holds the real model key and injects it host-side;
      agents carry only a scoped, identity-bound, revocable capability token. A
      compromised agent leaks nothing reusable.
  - title: Per-agent capability gating
    details: >-
      Each agent reaches MCP tools through a broker that enforces, per verified
      identity, which tools and operations it may use, with request constraints
      pinned at mint time.
---

Lever runs fleets of coding agents under containment. It wraps
[Scion](https://github.com/GoogleCloudPlatform/scion), Google's container-based
agent orchestrator, in a containment-and-credential boundary: Scion and every
agent it runs (via rootless podman) live inside one isolated VM with no host
filesystem access, no ambient authority, and an egress allowlist. A
**capability broker** stays on the host, outside the jail, and mediates everything
that crosses it (the agents' credentials, their tool calls, and Scion's hub
calls), so the real model key never lands in a container; `egress: closed` seals the
jail to the broker alone.

A fleet is a **manager** agent with the whole project tree as its workspace and
broker-granted authority to orchestrate the others, plus **worker** agents each
confined to a subtree with narrower tool grants.

**Platforms:** macOS on Apple Silicon with [OrbStack](https://orbstack.dev), or
Lima (macOS `vz`, Linux QEMU/KVM; Lima >= 2.0.0, checked at bring-up). The
Linux/Lima path is validated end-to-end. Known Lima gaps: `lever stop` -> `up`
comes back with a fresh manager conversation
([#3](https://github.com/stevegeek/lever/issues/3)); `remote:` requires the
`orbstack` backend (rejected at config load otherwise).

Prebuilt `lever` binaries ship per release (darwin/linux, amd64/arm64). A Go
1.26+ toolchain is required at runtime with `scion.version`/`scion.source` (Scion
is compiled at `lever apply`); `scion.binary` needs none. The agent image is built
locally with Docker. See [install](/getting-started/install/).
