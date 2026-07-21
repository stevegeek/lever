---
title: "Operator directives"
nav_order: 5.7
parent: Security model
permalink: /security-model/operator-directives/
---
Part of the [security model](/security-model/). Sections keep their original § numbers.

## 11. Operator directives: an authenticated human channel

Every other section of this page treats the operator's own instructions as out of scope for
attack — §5 protects the *config* the operator wrote, §6 protects the *credentials* the operator
provisioned, but nothing so far gives a running agent a way to verify that a piece of text claiming
operator authority actually came from the operator. In practice every human→agent instruction
(`lever msg`, a relayed email, a file dropped in the tree) arrives as unauthenticated text; the
`sender: "user:…"` label is a string the orchestration layer stamps on, not a verified identity. A
correctly hardened agent that treats "email the owner" or "run this" as untrusted content when it
appears mid-conversation is *right* to refuse it — but that leaves the operator with no genuine way
in short of `lever stop` → edit the boot prompt → restart, which destroys session state.

**Operator directives are a channel an agent can treat as authoritative without that becoming a
prompt-injection backdoor.** The load-bearing design constraint: you cannot use the model's own
judgment as the security boundary for a feature whose entire purpose is to override that judgment.
Authoritative-*sounding* prose is exactly what a prompt injection also produces, so the override
cannot be persuasive text the model is talked into trusting. It has to be a **signed, host-verified
artifact**, delivered as a pointer, fetched by the agent over its own already-authenticated channel.
The model never checks a signature; all cryptography is host-side.

### 11.1 Three layers, and the honest limit of each

| Layer | Where | What it does |
|---|---|---|
| 1. Delivery gate | Broker, host-side | A forged, unsigned, or altered directive never reaches an agent's inbox. This is a real cryptographic property over the signed artifact. |
| 2. Agent-side live verification | Agent, over its own mTLS channel to the broker | Stops low-effort in-context spoofing by binding consumption to the caller's verified identity. Depends on per-agent network-namespace isolation ([§4.3](/security-model/worker-isolation/), shipped 0.7.0) — without it, co-resident agents shared a gateway loopback and this binding would not be trustworthy. |
| 3. Host-enforced action binding | Broker, at tool-call time | The intended end state: the broker itself enforces the specific bound action when the tool is called. **Not implemented in Phase 1.** |

> **Honest Phase-1 scope.** Phase 1 delivers authenticated, integrity-protected, replay-proof
> **delivery and verification** of an operator-signed action bound to a specific target agent. It
> does **not** yet enforce that action at tool-call time. On consume, the broker returns the
> validated action descriptor to the agent, which then makes the call through its ordinary
> capability path — still subject to every existing host-side grant check (`rules.MayObtain` at
> mint, `token.Verify` at call, [§6.2](/security-model/credentials/)). **A directive therefore grants no new capability.**
> Its value is that the request is *provably from the operator*, which lets a hardened agent act on
> operator intent it would otherwise refuse for lack of a verifiable requester — for actions already
> within its standing grants. For the `tool_call` and `approval` kinds, the *execution* boundary in
> Phase 1 is still model discipline (bar-raising, not host-enforced); true call-time enforcement, and
> `approval`'s distinguishing operator-approval gate, is deferred to a later phase with its own
> adversarial review. Do not describe Phase-1 `tool_call` as "host-enforced capability" — it isn't yet.

Three directive kinds exist: `tool_call` (a fully-bound call — tool, op, and args all fixed at
signing, so injected text has no argument left to steer, subject to the Phase-1 note above),
`approval` (permits a call the agent's *standing* grants already allow but policy flagged for
operator sign-off; never elevates beyond standing grants; its distinguishing enforcement is the
Phase-2 piece), and `instruction` (free-text advisory guidance with **no host enforcement** —
explicitly the lowest-trust, escape-hatch kind; the bootstrap treats it as the operator's steer,
never as authority that overrides a refusal of a sensitive or outbound action).

### 11.2 Trust model

**Trusted:** the host-side broker, the host CLI, the host filesystem, and — since per-agent netns
landed in 0.7.0 — each agent's own mTLS channel to the broker.

**Treated as hostile:** everything inside the jail — any agent (manager or worker), the Scion hub,
anything readable from a container. Attackers in this model can inject arbitrary text into any
agent's context and can observe delivered notifications and directive ids; **ids are treated as
public**, not secret.

**Out of scope:** host compromise (as throughout this security model), and the model being
*persuaded* to act within the host's already-permitted actions — precisely why Layer 3 (§11.1)
matters, and why Phase 1 alone does not defend against it.

### 11.3 Mechanism, in brief

The operator signs a canonical JSON statement with an SSH key (`ssh-keygen -Y sign`, a fixed
namespace) naming the target agent, a time window, and an action. `lever directive send` submits
the exact bytes and signature over a 0600 unix-domain admin socket in the instance state
dir — genuinely unreachable from inside the jail; every admin operation is operator-signed
regardless, the socket is defence in depth, not the trust boundary. The broker verifies the
signature against an `allowed_signers` file (fixed principal, fixed namespace, only exit 0
accepted) over the exact received bytes, parses those same bytes (rejecting duplicate JSON keys, no
re-serialize step to smuggle a mismatch through), validates the time window and instance name, and
stores the directive active in persistent host-side state. It then delivers a **pointer only** — a
directive id and nothing else — to the target agent's ordinary message channel; no action content
ever transits that untrusted channel. If (and only if) the agent independently decides to act, it
calls the `directive_consume(id)` tool over its own mTLS connection to the broker, which performs an
atomic compare-and-swap: the action is returned, and the directive flipped to consumed, only if the
caller's verified identity and current generation (§11.4) match the
directive's target and it is unconsumed and in its time window. Every failure mode — unknown id,
wrong target, stale generation, already consumed, expired — returns a byte-identical opaque
`{"error":"not found"}`; there is no existence, target, or state oracle.

### 11.4 Identity binding: `{cn, generation}`

A directive binds to the target agent's mTLS **CN plus enrolment generation**, not to a recyclable
slug name. The broker keeps a persistent per-CN generation counter:

- **Bumped** on every genuine `/enrol` — a new occupant of a recycled agent slug — which
  invalidates the predecessor's still-active directives.
- **Established** (set to 1 if unset) on `/renew`, because an agent that restarts with a persisted
  certificate, or whose certificate predates this feature, refreshes via `/renew` and never re-hits
  `/enrol`. Renew never *bumps* the counter — that would invalidate an agent's own directives on
  every routine 12h leaf refresh ([§6.3](/security-model/credentials/)).

Consume requires the caller's *live* CN and current generation to match the directive's
`target_agent`, so one agent cannot consume a directive addressed to another, and a recycled slug
cannot inherit a predecessor's pending directives. This gate is only as trustworthy as the mTLS
identity behind it, which is why it depends on the per-agent network namespace work in
[§4.3](/security-model/worker-isolation/): before that shipped, co-resident agents shared one
gateway loopback and a compromised sibling could have presented as another agent's identity.

### 11.5 Key posture (honest)

- **Default, recommended:** the signing key lives on the operator's own machine, or is
  hardware-backed (touch-to-sign). Host compromise then does not imply operator-key compromise, and
  signatures carry real non-repudiation.
- **Key stored on the broker host** is equivalent to "the operator's own machine" *only when that
  host genuinely is the operator's own machine* (e.g. a single-user workstation running the
  instance). Understand its effective trust honestly: it reduces to "whoever can invoke the CLI or
  read the key file on that host," it is not hardware-backed, and a compromised host-side tool
  subprocess could read it. Reasonable on a personal workstation; document the posture wherever it's
  deployed rather than assuming the stronger default.
- **SSH agent forwarding to the broker host is forbidden.** A forwarded agent is a signing oracle: a
  compromised host could use it to sign arbitrary directives without ever extracting the key
  material.
- **Live revocation and break-glass.** Revoking a key is an edit to the `allowed_signers` file — the
  broker shells out to `ssh-keygen -Y verify` per call, so an edit takes effect immediately, no
  restart. `allowed_signers` `valid-after`/`valid-before` can also time-box a key without an edit.
  Because a single lost or rotated key must never lock the operator out, **multiple keys under the
  one operator principal (≥2) is the recommended default**, and `lever directive selftest`
  round-trips a self-signed test directive to catch an `allowed_signers` misconfiguration before it
  matters.

### 11.6 Threat model: attack, defence, and honest residual

| Attack | Defence / mechanism |
|---|---|
| Forged directive placed in an agent's inbox | Layer 1 — never verifies, never stored, never delivered (§11.1). |
| Cross-agent consume (agent A consumes a directive addressed to agent B) | mTLS CN + generation gate at consume; depends on 0.7.0 per-agent netns (§11.4, [§4.3](/security-model/worker-isolation/)). |
| In-context spoof — injected text claiming to be a "verified directive" | Structured, bound actions plus the "only a `directive_consume` you emitted yourself, this turn" rule — enforced for bound actions (`tool_call`/`approval`), only bar-raising for `instruction`. |
| Continuation / boundary confusion (attacker text appended after a real consume) | Bound actions carry no free text and the agent acts only on the *returned* action, never on surrounding text. |
| Token or id swap | The action acted on comes only from the consume response — nothing lives outside the signed statement for an attacker to substitute. |
| Replay of an observed directive id | Single-use atomic compare-and-swap plus persistent tombstones plus expiry. |
| Replay after a broker restart | Directive state, tombstones, and per-CN generations persist to disk with atomic writes, loaded before the broker starts. |
| Cross-agent redirection via slug recycling | `{cn, generation}` binding; re-enrolment bumps the generation and invalidates the predecessor's directives (§11.4). |
| Cross-instance replay | The signed statement carries `instance`; a directive for one instance does not verify against another's `allowed_signers`/state. |
| Manager→worker authority laundering | Authority does not launder — a directive that must reach a worker is signed for that worker directly; a manager's own message is manager-tier, never operator-tier. |
| A compromised hub reads a directive id off the wire | Accepted: ids are treated as public. Nothing acted-on ever transits the hub — only the pointer does. |
| Notification flood (spam ids at an agent) | Per-agent rate limiting; the bootstrap carve-out treats a flood of ids as inert, not a work queue. |
| `directive_check` used as an existence/target oracle | Target-gated, same opaque not-found response as consume. |
| Stolen operator signing key | Live revocation via `allowed_signers` edit, short default expiry (10m, hard-capped 24h), ≥2 keys recommended, hardware touch-to-sign for the strongest posture (§11.5). |
| `allowed_signers` misconfiguration / operator lockout | `lever directive selftest` plus the multi-key recommendation (§11.5). |

**Honest residuals — stated, not hidden:**

- Phase-1 `tool_call`/`approval` execution is model discipline, not host-enforced; Layer 3 (§11.1)
  is the pending piece that closes this.
- The "this turn, you emitted it" property that the bootstrap carve-out relies on depends on the
  harness rendering tool results distinguishably from message text — bar-raising, not a proof.
- A compromised hub sits on the delivery path for the pointer and can block a notification or
  reorder its timing around a genuine consume; `lever directive list` surfaces a
  notified-but-never-consumed directive so this is at least detectable, not silent.
- Metadata — a directive's existence, its target, and roughly when it was consumed — can leak to a
  compromised hub. Accepted, given ids are already treated as public; no directive *content* leaks
  this way.

### 11.7 Where this fits

Operator directives sit on top of, and depend on, containment already covered elsewhere on this
page: the [jail](/security-model/jail/) (§2) for the environment agents run in, and the
[per-agent network namespace](/security-model/worker-isolation/) (§4.3) specifically, without which
the mTLS identity that consume binds to would not be trustworthy. The capability broker and its
grant checks ([§6](/security-model/credentials/)) are what a consumed action's *execution* still
goes through in Phase 1 — a directive authenticates the requester, it does not widen what the
requester may do. For CLI usage, the `operator:` config block, and worked examples, see the
[operator directives](/operator-directives/) feature guide.
