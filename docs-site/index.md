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

Lever runs **multiagent fleets of coding agents under strong containment** — on
your laptop or a cloud instance. It wraps
[Scion](https://github.com/GoogleCloudPlatform/scion), Google's container-based
agent orchestrator, in a **containment-and-credential boundary**: Scion and every
agent it runs (via rootless podman) live inside one isolated VM with no host
filesystem access, no ambient authority, and a locked-down egress allowlist. A
**capability broker stays on the host**, outside the jail, and mediates everything
that crosses it — the agents' credentials, their tool calls, and Scion's own hub
calls — so your real model key never lands in a container, and you can close egress
to the broker alone when you want nothing else reachable.

A fleet is a **manager** agent with visibility over the whole project tree and
broker-granted authority to orchestrate the others, plus **worker** agents each
confined to a subtree with a narrower set of tool grants. Point them at real work
without handing them your secrets.

**Platforms today:** macOS on Apple Silicon with [OrbStack](https://orbstack.dev)
is the validated path. A Lima backend (targeting Linux and non-OrbStack macOS) is
built and passing its end-to-end suite, with live Linux validation in progress —
treat Linux support as *being proven*, not proven. Building from source needs Go
1.26+; there are no binary releases yet.
