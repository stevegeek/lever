---
title: "Operator directives"
nav_order: 5.75
---
# Operator directives: an authenticated channel to a running agent

Agents authenticate machines (mTLS, capability tokens, the host-side PAT) but not humans. `lever
msg`, relayed email, and files arrive as unauthenticated text; the `sender: "user:…"` label on a
message is stamped by the orchestration layer, not verified. A hardened manager therefore refuses
out-of-band instructions from them. Operator directives are a signed, host-verified channel an agent
can treat as authoritative without becoming a prompt-injection path.

A directive is not prose: it is a **signed, host-verified artifact**, delivered to the agent as an
opaque pointer and fetched over the agent's own authenticated channel. The model never checks a
signature; all cryptography is host-side. The rationale and threat model are in
[security model §11](/security-model/operator-directives/).

## How it works, end to end

| Step | Actor | What happens |
|---|---|---|
| 1. Sign | operator, host-side | `lever directive send` builds a canonical JSON statement (target `{cn, generation}`, validity window, an `action`) and signs it with an SSH key, namespace `lever-operator-directive@lever.dev`. |
| 2. Submit | operator CLI → broker | The exact signed bytes go to the broker over a **0600 UNIX-domain socket** in the instance state dir — unreachable from inside the jail. Every admin op (send/list/revoke) is signed regardless; the socket is defence in depth, not the trust boundary. |
| 3. Verify + store | broker, host-side | The broker verifies the signature with `ssh-keygen -Y verify` against `allowed_signers`, parses the *exact* received bytes (no re-serialize, duplicate JSON keys rejected), validates instance/window/target, and stores the directive `active`. |
| 4. Deliver a pointer | broker → agent | The agent's inbox gets only a `directive_id` — never the action content. No directive content ever transits the message channel an attacker could also write to. |
| 5. Consume | agent, over its own mTLS | If the agent independently decides to act, it calls `directive_consume(id)` on the `lever-capability` MCP server (the same server that mints capability tokens — see [capabilities](/capabilities/)). |
| 6. Atomic CAS | broker | Returns the action **only if** the caller's mTLS-verified CN + current enrolment generation match the directive's target, it's active, and it's inside its time window — and flips it to `consumed` in the same step. Single use. |

Every failure mode — unknown id, wrong target, stale generation, already consumed, expired — returns
the same byte-identical opaque `{"error":"not found"}`. There is no oracle for *which* failure
occurred; a revoked caller gets `403`, a rate-limited one `429`. A read-only `directive_check(id)`
exists too, target-gated with the same opaque miss.

**Identity binding.** A directive targets `{cn, generation}`, not a recyclable agent slug; consume
requires the caller's live CN and current generation to match
([§11.4](/security-model/operator-directives/)).

Directive state (active/consumed/revoked/invalidated/expired, plus tombstones for replay defence,
plus per-CN generations) persists to `.lever-state/directives.json` with atomic writes, and
consume/submit fail closed on a persistence error rather than report a success that isn't durable.
`invalidated` is set on an active directive when its target CN re-enrols (generation bump).

This depends on per-agent network-namespace isolation (shipped in 0.7.0); see
[worker isolation §4.3](/security-model/worker-isolation/).

## Directive kinds

| Kind | Shape | Trust level |
|---|---|---|
| `tool_call` | Fully bound: `tool`, `op`, `args` all fixed at signing (`arg_binding: "exact"`, `uses: 1`). Injected text has nothing to steer — the arguments aren't there to steer. | Authenticated intent (**not call-time enforced**). |
| `approval` | Permits a call the agent's *standing* grants already allow but policy flagged for operator sign-off. Never elevates beyond standing grants. Preferred shape. | Its distinguishing enforcement (an operator-approval gate at call time) is deferred to a later phase. |
| `instruction` | Free-text advisory guidance. No host enforcement at all. | Explicitly lower-trust — the bootstrap treats it as the operator's steer, never as authority that overrides a refusal of a sensitive/outbound action. The escape hatch; keep it rare. |

## Scope

The mechanism delivers authenticated, replay-proof **delivery and verification** of an operator-signed
action bound to the target agent. It does **not** enforce the bound action at tool-call time: the
agent makes the call through its ordinary capability path, under the existing grant checks, so **a
directive grants no new capability**. For `tool_call` and `approval`, the execution boundary is model discipline, not a host-enforced gate. See
[§11.1](/security-model/operator-directives/).

## Setup

### The `operator:` config block

```yaml
operator:
  allowed_signers: operator_allowed_signers   # ssh-keygen allowed_signers file; keep it OUT of tree:
  signing_key: /abs/path/to/operator          # default private key `lever directive send` signs with
  directive_expiry: 10m                        # optional; default 10m
  directive_expiry_max: 24h                    # optional; default 24h (hard cap — a larger value is rejected)
```

The whole block is optional. With `allowed_signers` unset, the channel is simply **disabled** —
dormant, not a half-configured risk. `allowed_signers` is confined to the instance root (like
`manager.prompt_file`); `signing_key` is a host path and deliberately **not** confined — the signing
key must live *outside* the mounted tree, where a jailed agent can never read it. Put both at the
host-only instance root, outside `tree:`, and gitignore the private key.

### Generating a key

```bash
ssh-keygen -t ed25519 -f operator          # writes operator (private) + operator.pub
```

Add a line to the `allowed_signers` file naming the fixed principal `operator@<instance-name>`:

```
operator@myapp ssh-ed25519 AAAA...   # <type> <keydata> copied from operator.pub
```

Verify the wiring before you need it in anger:

```bash
lever directive selftest
# selftest OK: signing key verifies against the broker's allowed_signers
```

### Key posture

Keep the key on the operator's own machine or hardware-backed; never forward an SSH agent to the
broker host; put two or more keys under the `operator@<instance>` principal so a lost key never
locks you out. Editing `allowed_signers` takes effect immediately (the broker runs `ssh-keygen -Y
verify` per call). Detail: [§11.5](/security-model/operator-directives/).

## Using it

```bash
# advisory instruction — no host enforcement, keep it rare
lever directive send manager --instruction "Hold all outbound email until I'm back online at 6pm."

# fully-bound tool call — args fixed at signing time
lever directive send worker-a --action '{"kind":"tool_call","tool":"db","op":"read","args":{"table":"orders"},"arg_binding":"exact","uses":1}'

# list what's outstanding
lever directive list --state active

# revoke one before it's consumed
lever directive revoke <directive-id>
```

`send` prints the exact statement bytes it's about to sign, for operator review, before it sends
anything. `<agent>` is the manager (the app name) or a declared worker name; `--expires` defaults to
`operator.directive_expiry` and is hard-capped at `operator.directive_expiry_max` (itself capped at
24h). `--key PATH` overrides `operator.signing_key` (on send/list/revoke/selftest). `--not-before
RFC3339` delays validity (default: now). `--state` on `list` accepts
`active|consumed|revoked|invalidated|expired`. Every `directive` subcommand takes an optional
trailing `[CONFIG]` path and otherwise reads `./lever.yaml`, like the other `lever` commands.

`lever doctor` reports an "operator directives" check: unconfigured is a pass (most instances never
touch this), and once configured it verifies `allowed_signers` has at least one key, `ssh-keygen` is
on `PATH`, and — if the broker is up — that the directive socket exists.

## What this does NOT do

- It does not grant any capability the target did not already hold (see Scope).
- `instruction` directives authenticate only that the operator sent them; they never override a
  refusal of a sensitive or outbound action.
- It does not authenticate manager→worker messages. A directive for a worker is signed for that
  worker directly; authority does not launder through the manager.
- It does not hide directive ids from a compromised hub; ids are public. It protects the content
  and the single-use, target-bound consumption.

Threat model: [security model §11](/security-model/operator-directives/). Config keys:
[config reference](/reference/config/#operator). Commands: [CLI reference](/reference/cli/).
