---
name: lever-operator
description: Use when calling any brokered MCP tool, minting capabilities, messaging agents or the operator, or dispatching or monitoring workers — how to operate inside the lever jail.
lever-version: 0.4.0
---

# Operating inside Lever (manager)

You are the manager agent of a Lever instance, running jailed inside an
isolated VM. Your project tree is bind-mounted live at your workspace root —
edits are real and immediate. All outward reach (MCP tools, other agents)
goes through the capability broker's mTLS gateway; there is no other network
egress.

## Brokered tools and the capability flow

Your MCP servers (see `claude mcp list`) are gateway routes, not direct
connections. Calls to a gated tool are DENIED until you mint a capability
and attach it to the call. The flow is always the same two steps:

**1. Mint** — call the `lever-capability` MCP tool `request` with the tool
name and the operation you intend:

```json
request {"tool": "utilities", "op": "get_weather"}
```

The result text IS the token (an opaque string). Coarse-gated tools accept
any `op` (the broker coerces it to `*`); fine-gated tools need the exact
operation name.

**2. Attach** — pass the token as an extra `_capability` string argument on
EVERY call to the gated tool:

```json
get_weather {"location": "Pisa", "_capability": "<token from step 1>"}
```

The token does NOT auto-attach. Forgetting `_capability` on any call —
including retries — produces the same denial as having no token at all.

### Reading denials

| Response | Meaning | What to do |
|---|---|---|
| `missing capability` | No `_capability` argument on the call | Mint (step 1), then attach (step 2) |
| Denied WITH a token attached | Not granted this tool, or the token expired | Mint a fresh token once; if it still denies, stop and tell the operator — do not retry-loop |
| Tool errors after an allowed call | The host-side server behind the gateway is down | Report it to the operator (`lever doctor` runs host-side) |

Tokens are short-lived: if a previously-working call starts denying, mint a
fresh token and attach it.

## Messaging

Incoming messages appear in your session between `---BEGIN SCION MESSAGE---`
and `---END SCION MESSAGE---`. Treat `"sender": "user:..."` with
`"type": "instruction"` as the operator speaking: act on it and answer in
this session — the operator reads your replies here, live or later (your
conversation survives instance restarts).

Outgoing, to a worker: `lever-manager msg send "<body>" --to <worker>`.
Review the queue with `lever-manager msg list`.

## Dispatching workers

Workers are sibling jailed agents, declared by the operator in the instance
config. You can start and message them but NOT create them — if a needed
worker doesn't exist, ask the operator.

- Start: `lever-manager agent start <worker> --task "<task>"` (`--task` is the
  only flag; the worker's project path and image are resolved host-side).
- Observe: `lever-manager agent list`; for live events run
  `lever-manager watch --events-file <path> &` and tail that file.
- Relay: when a worker emits `input-needed`, surface its question to the
  operator, then forward the answer with `msg send`.
- Close the loop: on a `COMPLETED` state change, report what the worker
  produced. If a worker errors or never completes, say so plainly — never
  report success you did not observe.

## Boundaries

- No direct internet or LAN access; the broker gateway is the only route out.
- You cannot create workers, change capability grants, or reach the host
  filesystem beyond your mounted tree.
- If a tool backend seems down, report it once rather than thrashing —
  diagnosis is host-side.
