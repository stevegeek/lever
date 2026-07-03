---
title: "Example: a personal AI assistant"
nav_order: 3
---

# Example: a personal AI assistant

A good fit for Lever is a **personal assistant made of several long-running
agents**: distinct personas, each trusted with a different slice of your data
and tools. You talk to a **manager** and it dispatches work to the personas.
Lever lets you give one persona access to your notes and another only web search,
without ever putting a real credential in any container, and without trusting the
agents to behave.

This page builds a small, generic version of that. Nothing here reflects a real
setup; the tools (`notes`, `web`) are illustrative.

## The shape

```
manager  ── dispatches ──▶  archivist   (reads & writes your notes)
                            researcher  (web search only, no notes)
```

- **`archivist`** is a trusted persona: it may read *and* write the `notes` tool.
- **`researcher`** is less trusted (it reads untrusted web content, a prime
  prompt-injection vector), so it gets **only** `web` search. It cannot touch
  notes at all.
- Both run in **api-key mode** (the default): the real model key lives host-side
  in the broker, and `egress: closed` seals the jail. Neither container ever holds
  a usable credential, and neither can reach anything but the broker.

## The config

```yaml
name: assistant
backend: orbstack
tree: workspace
egress: closed                            # seal the jail: agents reach only the broker
scion:
  version: 666333f9                       # pin a scion commit; fetched + cross-compiled into the jail
manager:
  image: scionlocal/lever-claude:latest
  prompt_file: manager.md

broker:
  llm_auth: api-key                       # no real key in any container (the default)
  api_key_file: ~/.secrets/anthropic-key  # 0600; read host-side by the /llm proxy only
  tools:
    - name: notes
      command: [lever-tool-notes]         # the supervised subprocess the broker proxies to
      backend: 127.0.0.1:3201             # loopback address it listens on (injected as -backend)
      operations:
        - { name: read }
        - { name: write }
      allowed_values:
        folder: [journal, reference]      # a notes capability may only be pinned to these
    - name: web
      command: [lever-tool-web]
      backend: 127.0.0.1:3202
      operations:
        - { name: search }

groves:
  - name: archivist                       # trusted: read + write notes
    dir: groves/archivist
    obtain:
      - { tool: notes, op: read }
      - { tool: notes, op: write }
  - name: researcher                      # less trusted: web search only
    dir: groves/researcher
    obtain:
      - { tool: web, op: search }
```

Every field here is in the [configuration reference](/reference/config/). The
two things doing the security work are **`broker`** (the credential boundary) and
the per-grove **`obtain`** grants (the capability boundary).

Each `tools` entry is a host-side subprocess the broker supervises: `command`
launches it and `backend` is the loopback address it listens on (injected as
`-backend`). The broker proxies gated calls to that address, so the tool's real
backend never enters a container.

## What happens at run time

Bring it up and talk to the manager (see [getting started](/getting-started/)):

```sh
lever up
```

Inside the manager session you dispatch a persona:

```sh
lever-manager agent start archivist \
  --task "Summarise this week's journal entries into reference/weekly.md"
```

Behind that one command, the boundary is enforced at three points:

1. **Identity.** On boot, `archivist` generates a keypair *inside its container*
   and enrols with the broker over mTLS. The private key never leaves the
   container; the broker knows this agent by its certificate CN.
2. **Capability mint.** When the agent wants to write a note, it asks the broker
   for a `notes/write` capability. The broker checks `archivist`'s `obtain`
   policy (allowed), validates any requested constraint against `allowed_values`
   (`folder` must be `journal` or `reference`), and mints a short-lived token
   **bound to `archivist`'s identity** with that constraint pinned in.
3. **Gated call.** The agent calls the `notes` tool through the broker's MCP
   gateway at `/mcp/notes/`, presenting the token. The gateway verifies the
   signature, that the caller *is* the bound agent, and that the request matches
   the pinned constraint, then forwards it. The real key for the model, and the
   real backend for the tool, stay on the host side.

Now consider `researcher`, fed a malicious web page that tells it to read your
private notes:

```sh
lever-manager agent start researcher \
  --task "Research X and report back"
```

- It has no `notes` grant, so the broker **refuses to mint** any `notes`
  capability for it (`/request` → 403). 
- Even if it somehow obtained a token, every token is **identity-bound**, a
  capability minted for `archivist` is rejected when presented by `researcher`.
- It cannot reach `api.anthropic.com`, your host, or the LAN directly, egress is
  closed, so its only outbound path is the broker, and the broker only does what
  its capabilities allow.

The blast radius of a compromised `researcher` is: web search. That's it.

## Delegation (optional)

Sometimes the manager wants a persona to do something on its behalf, narrowed to
a specific scope. Grant the manager a `delegate` right:

```yaml
manager:
  delegate:
    - { tool: notes, op: read, to: [researcher] }
```

Now the manager can ask the broker to mint a `notes/read` token **bound to
`researcher`** and narrowed (e.g. `folder: reference`), a one-off, strictly
weaker capability the broker enforces. Delegation always goes through the broker
(it is the sole minter), so a delegated token can only ever be *narrower* than
what policy allows, never wider.

## Why this needs Lever

Scion would run these three agents and message between them, but it would inject
the real model key into every container in cleartext, leave egress open, and gate
tools only with a coarse shared token. The per-persona boundary, the key never
touching a container, and the closed egress are what Lever adds. See the
[security model](/security-model/) for how each is enforced.
