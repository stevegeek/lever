---
title: Capabilities
nav_order: 6
---
# Capabilities: how agents get authority

Lever's inner security layer is a **capability model**: agents hold no ambient authority — no API
keys in the environment, no open ports to host services. Everything an agent may do outside its
own workspace is represented by a short-lived, identity-bound **capability token** minted by the
host-side **broker**, and every use of one is checked, audited, and revocable.

This page describes the model end to end. For the *why* (threat model, what containment alone
can't buy), see [security-model.md](/security-model/) §6; for the *config keys*, see the
[config reference](/reference/config/); for the hands-on walkthrough, see
[getting started §7](/getting-started/#7-give-an-agent-an-mcp-server-the-various-ways).

## Identities: enrolment

Every agent container proves who it is before it can ask for anything:

1. At first boot, `lever-agent` (baked into the image, run by the pre-start hook) generates a
   keypair **inside the container** — the private key never leaves it.
2. It enrols with the broker using a **one-shot bootstrap ticket** the host staged for exactly
   this agent (single-latch: spent on first use, re-armed only by the host).
3. The broker issues an mTLS client certificate whose **CN is the agent's identity** — `manager`
   for the manager, the grove name for a grove.

From then on, every request the agent makes to the broker — minting, tool calls through the
gateway, messaging — is authenticated by that certificate. There is nothing to steal that
works anywhere else: the key is container-local and the identity is pinned.

## Tokens: minting

A capability token is a short-lived signed structure (Ed25519, verified offline by the gateway
and re-verifiable by first-party tools) naming: the **tool**, the **operation**, the agent it is
**bound to**, optional **constraints**, and the issue **epoch**. It is not a bearer secret that
works forever or for anyone — it expires quickly and only works over the mTLS session of the
agent it names.

Agents mint through the **`lever-capability`** MCP tool (available in every agent container):

- `request {tool, op}` — mint bound to self. The broker's **obtain policy** — the `obtain:` lists
  you wrote in `lever.yaml` — decides whether *this* agent may have *this* capability. The result
  text is the token.
- `delegate {tool, op, to}` — mint bound to *another* agent, to hand off. Gated by the
  delegator's `delegate:` list; a delegated token is strictly narrower than what the delegator
  holds, and extra `key=value` arguments become constraints baked into the token.

Two **gate grains** exist per tool (`gate:` in the tool's config entry):

- `coarse` — one capability covers the whole tool. Mint requests are coerced to `op: "*"`; any
  operation passes. Right for personal external servers where per-verb control adds nothing.
- `fine` (default) — the token names one operation, and `allowed_values` constraints can pin
  parameters at mint time (e.g. a `db` capability valid only for `table: users`). The gateway
  chooses the required operation **server-side**, so a coarse `"*"` token can never widen a
  fine-gated tool.

## Using a token: the gateway

Brokered tools are MCP servers behind the broker's mTLS gateway at `/mcp/<name>/`. To call a
gated operation, the agent passes the token as an extra **`_capability`** string argument on the
tool call (the gateway advertises this argument in every tool schema). The gateway then:

1. authenticates the caller's certificate,
2. verifies the token — signature, expiry, epoch, tool/op match, **bound-to matches the caller
   CN**, constraints satisfied,
3. **strips `_capability`** and proxies the call — external servers never see tokens; first-party
   tools receive the token plus a forge-proof caller header so they can independently re-verify,
4. writes a `broker.decision` line (allow or deny, with the reason) to `.lever-state/broker.log`.

A denial reads `missing capability` (no token attached — mint one and attach it) or names the
policy reason (not granted, expired, wrong binding). Tokens are short-lived by design: agents
re-mint on expiry rather than holding long-lived credentials.

## The LLM as a capability (api-key mode)

With `llm_auth: api-key`, even the agent's own model access is a capability: the agent holds only
a `capability(llm)` token (auto-granted as `obtain: [{tool: llm, op: generate}]`), carried as its
`ANTHROPIC_AUTH_TOKEN`. The broker's `/llm` endpoint verifies and strips it, injects the real
Console key **host-side**, and streams to the fixed upstream — the real key appears in zero
container bytes, and jail egress to the public internet closes. A renewal sidecar re-mints the
token before expiry. (`subscription` mode trades this for simplicity: the OAuth token is projected
to agents directly.)

## Revocation

Three independent handles, all host-side:

- **Expiry** — the default: tokens die on their own within minutes.
- **`lever revoke <agent>`** — one agent's tokens stop verifying immediately (persisted, survives
  broker restarts).
- **`lever broker bump-epoch`** — raise the epoch floor and every outstanding token dies at once.

## Teaching agents the flow

Agents don't know any of this innately. `lever init` scaffolds **operator skills** into your
instance tree — SKILL.md files that teach the manager (and each grove) the mint-then-attach flow,
how to read denials, and when to stop and ask the operator. Run it once per instance and re-run
after upgrades; see [getting started §4](/getting-started/#4-scaffold-the-operator-skills-lever-init).
