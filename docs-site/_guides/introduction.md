---
title: Introduction
nav_order: 1
---

# What Lever is

Lever wraps [Scion](https://github.com/GoogleCloudPlatform/scion), Google's
experimental container-based orchestrator for LLM coding agents, in a security
boundary. Scion is the engine: it creates per-agent containers, manages their
lifecycle, and relays typed messages. Lever is everything you wrap around that
engine to run it safely on your own machine:

- a **jail** that contains the whole stack,
- a **capability broker** that keeps real credentials out of every container,
- and a small **operator surface** (`lever.yaml`, `lever apply`, `lever up`).

## The problem it solves

An autonomous agent that reads untrusted content (web pages, dependencies,
issue text, tool output) can be steered into running arbitrary code. The
moment that agent also holds your model API key and has open internet access,
a single prompt injection can exfiltrate the key and impersonate you.

Scion contains agents in containers, but it does not contain *itself*: it runs
on your host, and its general secret model injects credentials into the
container in cleartext. Lever closes that gap.

## What Lever adds over Scion

Lever's value is the boundary it draws *around* Scion — things Scion presupposes
but doesn't build:

| | Scion alone | With Lever |
|---|---|---|
| **Containment** | runs on your host | the whole stack runs in an OrbStack isolated machine; agents run in rootless podman |
| **Network egress** | open by default | LAN and non-allowlisted host ports dropped; public internet open by default, or `egress: closed` to seal the jail to the broker alone |
| **Model credential** | injected into every container in cleartext | held host-side, the broker injects it, agents never see it (api-key, the default). The `subscription` opt-in hands the agent the OAuth token instead |
| **Tool access** | a coarse, shared token | per-agent, capability-gated MCP calls with pinned constraints |

Orchestration itself (starting and attaching agents, the directory-project
model, messaging) is **Scion**, driven through a thin `lever` wrapper.

## Where to go next

- **[Getting started](/getting-started/)**, install, configure, and run your
  first jailed agent.
- **[Security model](/security-model/)**, the threat model and exactly what the
  jail and broker do (and don't) guarantee.
- **[Configuration reference](/reference/config/)**, every `lever.yaml` field.
