---
title: "Operator directives"
nav_order: 5.75
---
# Operator directives: an authenticated channel to a running agent

Lever has strong **machine** authentication — mTLS between broker and agents, Ed25519 capability
tokens, a host-side controller PAT — but nothing that lets an agent verify a **human**. Every
human→agent instruction (`lever msg`, a relayed email, a file an agent reads) arrives as
unauthenticated text; the `sender: "user:…"` label on a message is a string the orchestration layer
stamps on, not a verified identity. In practice this produces exactly the right refusal for the
wrong reason: a well-hardened manager correctly declines an out-of-band instruction like "email the
owner" or "write this to disk" because it cannot tell the operator from an attacker who got text
onto the same channel. The hardening is correct; the operator simply had no authentic way in. Before
this feature, the only genuine operator action was `lever stop` → edit the boot prompt → restart,
which throws away session state.

Operator directives give the operator a channel an agent can treat as authoritative, without that
channel becoming a prompt-injection backdoor.

## The load-bearing principle

You cannot use the model's judgment as the security boundary for a feature whose entire purpose is
to *override* the model's judgment. If a directive were just authoritative-sounding prose the model
is persuaded to obey, it would recreate "authoritative-seeming text" as the attack surface —
exactly as forgeable and injectable as any other prompt content. So a directive is not prose: it is
a **signed, host-verified artifact**, delivered to the agent as an opaque pointer, and fetched over
the agent's own authenticated channel. The model never checks a signature — all cryptography is
host-side.

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

**Identity binding.** A directive targets `{cn, generation}`, not a recyclable agent slug. The
broker keeps a persistent per-CN generation counter, bumped on every genuine `/enrol` (so a new
occupant of a recycled slug can't inherit a predecessor's directives) and established at 1 on
`/renew` for agents that never re-enrol. Consume requires the caller's *live* CN and *current*
generation to match, so one agent can never consume another's directive, and directives don't
survive a slug being recycled to a different agent.

Directive state (active/consumed/revoked/expired, plus tombstones for replay defence, plus per-CN
generations) persists to `.lever-state/directives.json` with atomic writes, and consume/submit fail
closed on a persistence error rather than report a success that isn't durable.

This depends on **per-agent network-namespace isolation** (shipped in 0.7.0): before it, co-resident
agents shared a gateway loopback, and the "consume is bound to the calling agent's own mTLS
identity" property wasn't real. See [worker isolation](/security-model/worker-isolation/).

## Directive kinds

| Kind | Shape | Trust level |
|---|---|---|
| `tool_call` | Fully bound: `tool`, `op`, `args` all fixed at signing (`arg_binding: "exact"`, `uses: 1`). Injected text has nothing to steer — the arguments aren't there to steer. | Authenticated intent (see Phase-1 scope below — **not yet call-time enforced**). |
| `approval` | Permits a call the agent's *standing* grants already allow but policy flagged for operator sign-off. Never elevates beyond standing grants. Preferred shape. | Its distinguishing enforcement (an operator-approval gate at call time) is deferred to a later phase. |
| `instruction` | Free-text advisory guidance. No host enforcement at all. | Explicitly lower-trust — the bootstrap treats it as the operator's steer, never as authority that overrides a refusal of a sensitive/outbound action. The escape hatch; keep it rare. |

## Honest Phase-1 scope — read this before relying on directives

Phase 1 delivers authenticated, integrity-protected, replay-proof **delivery and verification** of
an operator-signed action bound to the target agent. It does **not** yet enforce the bound action at
tool-call time. On consume, the broker hands the agent a validated action descriptor; the agent then
makes the call through its ordinary capability path — still subject to every existing host-side
grant check (`rules.MayObtain` at mint, token verification at call). **A directive therefore grants
no new capability.** Its value is that the request is *provably from the operator*, which lets a
hardened agent act on operator intent it would otherwise refuse for lack of a verifiable requester —
for actions already within its standing grants.

For `tool_call` and `approval`, the *execution* boundary in Phase 1 is still model discipline
(bar-raising), not a host-enforced gate. Do not treat a Phase-1 `tool_call` directive as a
host-enforced capability grant — it isn't one yet. True call-time enforcement, and `approval`'s
distinguishing operator-approval gate, are deferred to a later phase with its own adversarial
review.

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

- **Recommended:** the key lives on the *operator's own machine*, or is hardware-backed
  (touch-to-sign). Host compromise then does not imply operator-key compromise, and signatures carry
  real non-repudiation.
- **Key on the broker host** is only the "operator's own machine" posture when that host genuinely
  *is* the operator's machine (e.g. a personal instance). Understand what that buys: effective trust
  is "can invoke the CLI / read the key on the host," not hardware-backed, and a compromised
  host-side tool subprocess could read it. Fine for a personal machine — document the posture rather
  than pretending otherwise.
- **Never forward an SSH agent to the broker host.** A forwarded agent is a signing oracle a
  compromised host can use to sign arbitrary directives.
- **Multi-key by default.** The `operator@<instance>` principal supports (and should have) **two or
  more keys** in `allowed_signers`, so a lost or rotated key never locks the operator out
  (break-glass). Use `allowed_signers` `valid-after`/`valid-before` to expire a key without a config
  edit. Revocation is live: the broker shells out to `ssh-keygen -Y verify` per call, so editing
  `allowed_signers` takes effect immediately, no broker restart needed.

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
24h). `lever directive send/list/revoke` all take an explicit `[CONFIG]` path argument — unlike
`doctor`/`msg`, which are cwd-based.

`lever doctor` reports an "operator directives" check: unconfigured is a pass (most instances never
touch this), and once configured it verifies `allowed_signers` has at least one key, `ssh-keygen` is
on `PATH`, and — if the broker is up — that the directive socket exists.

## What this does NOT do

- It does not grant any capability a directive's target didn't already hold — Phase 1 is delivery
  and verification, not call-time enforcement (see above).
- `instruction` directives carry no cryptographic weight over the *action itself* — only over the
  fact that the operator sent them. They never override a refusal of a sensitive or outbound action.
- It does not authenticate manager→worker messages. A directive that must reach a worker is signed
  for that worker directly; a manager's own messages stay manager-tier, never operator-tier —
  authority does not launder through the manager.
- It does not hide directive ids from a compromised hub — ids are treated as public. What it
  protects is the *content* (never transits the untrusted channel) and *single-use, target-bound
  consumption* of it.

For the full threat model — what's trusted, what's in scope as hostile, and how each attack is
defended — see [security model: operator directives](/security-model/operator-directives/). For
every `operator:` config key, see the [config reference](/reference/config/); for the full
`lever directive` command surface, see the [CLI reference](/reference/cli/).
