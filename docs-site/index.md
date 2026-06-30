---
layout: home
title: Lever
hero:
  name: Lever
  text: Coding agents in a sealed jail.
  tagline: >-
    Lever runs Scion agents so the real model key never enters the container.
    One switch seals the agent off to reach nothing but a capability broker.
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
      The whole stack, Scion, the broker, the container runtime, every agent,
      runs inside one isolated machine with rootless podman and an egress
      allowlist. Host secrets and the LAN simply aren't reachable.
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

Lever wraps [Scion](https://github.com/GoogleCloudPlatform/scion), Google's
container-based agent orchestrator, in a **containment-and-credential boundary**.
Scion runs the agents; Lever keeps your real model key out of every container and
seals the jail off from your host and LAN, so you can point autonomous coding
agents at real work without handing them your secrets. Close egress to the broker
alone when you want nothing else reachable.
