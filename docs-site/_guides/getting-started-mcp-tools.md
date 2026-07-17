---
title: "Give agents MCP tools"
nav_order: 2.3
permalink: /getting-started/mcp-tools/
---
Part of [getting started](/getting-started/). Steps keep their original numbering.

## 7. Give an agent an MCP server (the various ways)

Agents get real power from MCP (Model Context Protocol) servers — calendars, search, a database,
your own tools. But an MCP server runs on your host, and the jail's whole point is that a
prompt-injected agent *can't* reach your host freely. So "attach an MCP server" is really "poke a
controlled hole." lever gives you two ways to do it, with a clear trade-off.

### Approach A — ambient (`allow_ports` + `.mcp.json`)

The simplest path: run the server on a host loopback port, open that one port through the jail's
egress allowlist, and hand the agent an `.mcp.json` pointing at it.

```yaml
# lever.yaml
manager:
  allow_ports: [3200]         # open exactly this host-loopback port to the jail
```

```json
// workspace/.mcp.json  (inside the mounted tree, so the agent's harness reads it)
{ "mcpServers": { "mytool": { "type": "http", "url": "http://host.orb.internal:3200" } } }
```

The agent now reaches `mytool` directly. This is easy and fine for a **trusted, read-only** server,
but it is an **ambient** grant. Any agent in the jail can hit that port with no per-call check, and
the port is a standing hole in the egress allowlist for as long as it's listed. There is no
capability, no per-agent scoping, and no audit.

### Approach B — brokered (capability-gated, recommended)

Register the server as a **broker tool** instead. The capability broker (host-side, holds no agent
trust) fronts it over mTLS at `/mcp/<name>/`; an agent reaches it **only with a
capability token the broker mints**, bound to that agent's identity. No `allow_ports` hole, no
hand-authored `.mcp.json` — `lever-agent boot` (baked into the image) discovers registered tools via
the broker's `/tools` and runs `claude mcp add /mcp/<name>/` for each. You get per-agent scoping and
an audit trail, and closing the ambient hole means a compromised agent can't reach the port at all
except through a capability it was granted. (The full model — enrolment, tokens, delegation,
revocation — is described in [capabilities.md](/capabilities/).)

There are **two kinds** of broker tool:

**External tool** (`external: true`) — front a server that is *already running* (you start it; the
broker does **not** spawn it). This is the right choice for third-party or desktop-app servers —
e.g. anything driving macOS apps via AppleScript, where the OS ties Automation permission to your
login session and a broker-forked child would lose it. Bind it on host **loopback** (a literal
`127.0.0.1`/`[::1]`; the broker proxies host-side, so a non-loopback backend would let the agent
reach your LAN *through* the broker). Pick a capability grain:

```yaml
broker:
  tools:
    # fine: only the listed operations are callable; arguments can be pinned.
    - name: devonthink
      external: true
      backend: 127.0.0.1:3302
      operations:
        - {name: search}
      allowed_values: {database: [work, personal]}
    # coarse: one wildcard capability admits the server's WHOLE surface.
    - name: things3
      external: true
      gate: coarse
      backend: 127.0.0.1:3300
```

- `gate: fine` (the default) — enumerate `operations`; a token authorises one operation, and
  `allowed_values` pins arguments. Use for anything sensitive.
- `gate: coarse` — one wildcard grant (`op: "*"`) admits every call the server exposes. Simplest;
  wholesale trust. The broker audits the real operation either way.

**First-party (captool) tool** — a tool the broker **supervises** as a subprocess (you give it a
`command`), written with the captool SDK so it re-verifies the capability itself and the broker
forwards the token to it. Use this when you're *writing* the tool and want it capability-aware with
the tightest control:

```yaml
broker:
  tools:
    - name: db
      command: [lever-tool-db]     # the broker launches + supervises this
      backend: 127.0.0.1:3201       # the loopback address it listens on
      operations:
        - {name: read}
      allowed_values: {table: [users, orders]}
```

### Granting access — who may use which tool

A registered tool is inert until an agent is granted a capability for it. Grants are per-identity
and default-deny (the token model behind them — identities, minting, delegation, revocation — is
described in [capabilities.md](/capabilities/)):

```yaml
manager:
  obtain:
    - {tool: calendar, op: "*"}                     # the manager may use calendar itself
  delegate:
    - {tool: devonthink, op: search, to: [worker]}  # …and may hand worker this at dispatch
workers:
  - name: worker
    dir: workers/worker
    obtain:
      - {tool: db, op: read}                         # worker may use db.read — nothing else
```

- **`obtain`** — the agent can self-mint a capability for the listed `{tool, op}`.
- **`delegate`** — the manager can mint a token *bound to a named recipient* worker at dispatch time
  (an attenuated hand-off).
- Absence of a grant = no access, and a grant for one agent can't be replayed by another (the
  token is identity-bound). Gate grains (`fine` vs `coarse`, and why a `"*"` wildcard can never
  widen a `fine` tool) are covered in [capabilities.md](/capabilities/).

### Which should I use?

| | Ambient (A) | Brokered — external (B) | Brokered — first-party (B) |
|---|---|---|---|
| Best for | quick, trusted, read-only | an existing/third-party server (esp. desktop-app/AppleScript) | a tool you're writing |
| Broker spawns it? | no | no (you run it) | yes (supervised) |
| Per-agent scoping / audit | no | yes | yes |
| Egress hole | standing (`allow_ports`) | none | none |
| Capability required per call | no | yes (token stripped) | yes (token forwarded) |

Start with **A** to get moving; move a server to **B** when you want it scoped per agent, audited,
and off the ambient allowlist. See [config-reference.md](/reference/config/) for every key and
[security-model.md](/security-model/) for what the gate does and doesn't protect.
