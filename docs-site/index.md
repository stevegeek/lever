---
layout: home
title: Lever
hero:
  name: Lever
  text: Coding agents in a sealed jail.
  tagline: >-
    Lever runs Scion agents where the real API key never enters the container
    and the agent can reach nothing but a capability broker.
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
      The whole stack — Scion, the broker, the container runtime, every agent —
      runs inside one isolated machine with rootless podman and an egress
      allowlist. Host secrets and the LAN simply aren't reachable.
  - title: The key never lands in the container
    details: >-
      In api-key mode the broker holds the real model key and injects it
      host-side; agents carry only a short-lived, identity-bound capability
      token. A compromised agent leaks nothing reusable.
  - title: Per-agent capability gating
    details: >-
      Each agent reaches MCP tools through a broker that enforces, per verified
      identity, which tools and operations it may use — with request constraints
      pinned at mint time.
---

Lever is a thin **brain + interface** over [Scion](https://github.com/GoogleCloudPlatform/scion),
Google's container-based agent orchestrator. Scion runs the agents; Lever wraps
it in a **containment-and-credential boundary** so you can point autonomous
coding agents at real work without handing them your secrets or the open
internet.
