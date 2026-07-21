---
title: Capabilities
nav_order: 6
---
# Capabilities: how agents get authority

Lever's inner security layer is a **capability model**: agents hold no ambient authority — no API
keys in the environment, no open ports to host services. Everything an agent may do outside its
own workspace is represented by a revocable, identity-bound **capability token** minted by the
host-side **broker**, and every use of one is checked, audited, and revocable.

This page describes the model end to end. For the *why* (threat model, what containment alone
can't buy), see [security-model §6](/security-model/credentials/); for the *config keys*, see the
[config reference](/reference/config/); for the hands-on walkthrough, see
[getting started §7](/getting-started/mcp-tools/).

## Identities: enrolment

Every agent container proves who it is before it can ask for anything:

1. At first boot, `lever-agent` (baked into the image, run by the pre-start hook) generates a
   keypair **inside the container** — the private key never leaves it.
2. It enrols with the broker using a **one-shot bootstrap ticket** the host staged for exactly
   this agent (single-latch: spent on first use, re-armed only by the host).
3. The broker issues an mTLS client certificate whose **CN is the agent's identity** — `manager`
   for the manager, the worker name for a worker.

From then on, every request the agent makes to the broker — minting, brokered tool calls,
messaging — is authenticated by that certificate. There is nothing to steal that
works anywhere else: the key is container-local and the identity is pinned.

## Tokens: minting

A capability token is a signed structure (Ed25519, verified offline by the broker and
re-verifiable by first-party tools) naming: the **tool**, the **operation**, the agent it is
**bound to**, optional **constraints**, and the issue **epoch**. It is not a bearer secret that
works for anyone: it only works over the mTLS session of the agent it names, so leaked token
text is useless without that container's private key. Its lifetime (`broker.grant_ttl`, default
`24h`) is a backstop, not the real control — every call re-checks the epoch and revocation
state, so revocation cuts access immediately regardless of TTL.

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
  parameters at mint time (e.g. a `db` capability valid only for `table: users`). The broker
  chooses the required operation **server-side**, so a coarse `"*"` token can never widen a
  fine-gated tool.

## Using a token: calling a brokered tool

Brokered tools are MCP servers the broker fronts over mTLS at `/mcp/<name>/`. To call a
gated operation, the agent passes the token as an extra **`_capability`** string argument on the
tool call (the broker advertises this argument in every tool schema). The broker then:

1. authenticates the caller's certificate,
2. verifies the token — signature, expiry, epoch, tool/op match, **bound-to matches the caller
   CN**, constraints satisfied,
3. **strips `_capability`** and proxies the call — external servers never see tokens; first-party
   tools receive the token plus a forge-proof caller header so they can independently re-verify,
4. writes a `broker.decision` line (allow or deny, with the reason) to `.lever-state/broker.log`.

A denial reads `missing capability` (no token attached — mint one and attach it) or names the
policy reason (not granted, expired, wrong binding). Agents re-mint on an expiry-shaped denial;
tokens are cheap, and the grant policy — not the token — is what persists.

**The token deliberately rides through the LLM.** Because `_capability` is a tool-call argument,
the token text passes through the model's context window, transcripts, and logs. This is safe
because tokens are **CN-bound**: the broker only accepts one over the mTLS session of the agent it
names. Token text that leaks through a transcript, a log file, or a prompt-injection exfiltration
is inert without that container's private key, which never leaves the container. A leaked token
reveals metadata (which tool/op an agent was granted), not authority.

## Operator directives are not a capability grant

Lever separately has [operator directives](/operator-directives/): SSH-signed instructions a
human operator sends to one target agent. It's easy to conflate the two mechanisms, so to be
precise — a directive authenticates *who is asking*, it does not widen *what the agent may do*.
On consume, the broker returns a validated action descriptor; the agent still requests any
capability it needs and calls brokered tools through the exact mint-then-call path described
above, subject to the same `obtain:`/`delegate:` policy and epoch/revocation checks. A directive
grants no new capability.

## The LLM as a capability (api-key mode)

With `llm_auth: api-key`, even the agent's own model access is a capability: the agent holds only
a `capability(llm)` token (auto-granted as `obtain: [{tool: llm, op: generate}]`), carried as its
`ANTHROPIC_AUTH_TOKEN`. The broker's `/llm` endpoint verifies and strips it, injects the real
Console key **host-side**, and streams to the fixed upstream — the real key appears in zero
container bytes, and jail egress to the public internet closes. A renewal sidecar re-mints the
token before expiry. (`subscription` mode trades this for simplicity: the OAuth token is projected
to agents directly.)

By default that fixed upstream is `https://api.anthropic.com`; set `broker.llm_upstream` to point
`/llm` at an LLM proxy that speaks the Anthropic Messages API instead (logging, caching,
a compliance boundary, etc). The security properties are unchanged: the broker still injects the
real Console key host-side and strips the capability token before forwarding. `llm_upstream` is a
host-side config value the agent can never influence, and the jail still only ever talks to the
broker.

### Choosing `llm_auth`: subscription vs api-key

The shipped examples default to `subscription` because it's the friction-free personal setup.
**The instance must be uniform** (mixing modes across manager/workers is rejected at config load):

| | `subscription` | `api-key` |
|---|---|---|
| Credential in containers | your Claude OAuth token, projected to **every** agent | none — only a revocable `capability(llm)` token |
| If an agent is compromised | the OAuth token is exposed (mint it least-privilege, rotate it) | nothing reusable leaks; revoke the agent |
| Jail egress | must stay open to the public internet for `api.anthropic.com` | closes to the broker alone |
| Cost model | your Claude subscription (agents share your session limits) | Console API billing, pay per token, per-agent attribution via the broker |
| Right for | one operator's personal/dev instance | anything you'd call a deployment, or many agents |

## Revocation

Three independent handles, all host-side:

- **Expiry** — the backstop: tokens die on their own after `broker.grant_ttl` (default `24h` —
  session-scale on purpose, because the per-call revocation check below is the real cut).
- **`lever revoke <agent>`** — cuts that agent off immediately (persisted, survives broker
  restarts). Enforcement is by **caller identity at use time**, on every path a revoked agent could
  act through *or observe from*: tool calls (brokered tools + `/llm` proxy), minting *and*
  delegating (it can't hand a fresh token to a still-valid agent), messaging (send + inbox list), worker
  dispatch/teardown/list and enrolment tickets (for the manager), tool-catalog listing, and cert
  renewal — renew is refused, so the agent's existing cert simply expires and revocation is
  terminal. A revoked agent gains nothing by re-minting, re-messaging, reconnecting, or enumerating.
  (Removing a worker from the config is *not* revocation — see the
  [operations guide](/operations/#changing-config-on-a-running-instance) for cutting off a removed worker.)
- **`lever broker bump-epoch`** — raise the epoch floor and every outstanding token dies at once.

## Teaching agents the flow

Agents don't know any of this innately. `lever init` scaffolds **operator skills** into your
instance tree — SKILL.md files that teach the manager (and each worker) the mint-then-attach flow,
how to read denials, and when to stop and ask the operator. Run it once per instance and re-run
after upgrades; see [getting started §4](/getting-started/first-run/#4-scaffold-the-operator-skills-lever-init).
