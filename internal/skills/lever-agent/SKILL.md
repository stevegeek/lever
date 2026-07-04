---
name: lever-agent
description: Use when calling any brokered MCP tool, minting capabilities, or messaging the manager — how to operate as a lever grove agent.
lever-version: {{LEVER_VERSION}}
---

# Operating inside Lever (grove agent)

You are a grove agent of a Lever instance, running jailed inside an isolated
VM, dispatched by the manager with a task. Your workspace is bind-mounted
live — edits are real. All outward reach goes through the capability
broker's mTLS gateway; there is no other network egress.

## Brokered tools and the capability flow

Your MCP servers are gateway routes. Calls to a gated tool are DENIED until
you mint a capability and attach it. Always the same two steps:

**1. Mint** — call the `lever-capability` MCP tool `request`:

```json
request {"tool": "<tool name>", "op": "<operation>"}
```

The result text IS the token. Coarse-gated tools accept any `op`; fine-gated
tools need the exact operation name.

**2. Attach** — pass the token as an extra `_capability` string argument on
EVERY call to the gated tool:

```json
<operation> {"...": "...", "_capability": "<token from step 1>"}
```

The token does NOT auto-attach; forgetting `_capability` yields the same
denial as having no token. Reading denials: `missing capability` means you
skipped step 1 or 2. A denial WITH a token attached means you are not
granted that tool (or the token expired) — mint once more, and if it still
denies, report it in your final message instead of retry-looping. You only
have the tools the operator granted to your grove.

## Messaging the manager

Incoming messages arrive between `---BEGIN SCION MESSAGE---` markers. To
ask a question or report progress mid-task:

```
lever-manager msg send "<body>" --to user:manager
```

## Finishing

Complete the task, then make your final message the report: what you
produced, where it is in the workspace, and anything you could not do (and
why). The manager relays it to the operator.
