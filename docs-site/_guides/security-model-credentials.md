---
title: "Credentials & capabilities"
nav_order: 5.4
parent: Security model
permalink: /security-model/credentials/
---
Part of the [security model](/security-model/). Sections keep their original § numbers.

## 6. Credential blast radius, and the path to capabilities

> **STATUS: BUILT.** The capability broker described below as "target" now ships and is
> enforced, realised with a **typed Ed25519-signed capability token** (`internal/cap/token`), not
> macaroons. (It was first built on biscuits/Datalog; that was simplified to a typed signed token after
> an audit found the only biscuit-specific feature, offline, holder-side attenuation, went unused,
> because every capability is minted online by the broker, which is always on the verification path.)
> The "Finding" and "Target design" text is kept as the threat narrative; **§6.1 + §6.2 describe the
> built, enforced state.** Where this section says *macaroon*, read *signed capability token*, with one
> correction: narrowing happens at **mint** (the broker bakes constraints into the signed token), not by
> offline holder-side attenuation.

**Finding.** The manager sets the upstream credential (`CLAUDE_CODE_OAUTH_TOKEN`) as a Hub secret;
scion **resolves and injects it into every agent container's environment** at start (user/owner
scope, single jail = single tenant). So **every worker holds the real, long-lived OAuth token** in
`$CLAUDE_CODE_OAUTH_TOKEN` (or a token file in its home). This is a **subscription-mode** exposure
only (§6.1). Combined with [§8](/security-model/compromise/) (open internet egress for the model API), a single prompt-injected
worker can read the token and exfiltrate it, **impersonating the operator's account beyond the
jail.** ([§4](/security-model/worker-isolation/)'s dev-auth-off/controller-PAT hardening closes the *separate* risk of a compromised
agent driving the hub itself; it does not touch this ambient-credential exposure, which is a
property of subscription mode's design, not of hub authority.) The token is *ambient, shared, and
long-lived*, the worst combination, and the highest-value secret in the system.

This is the strongest argument for replacing **pushing keys to agents** with **agents exchanging
identity for narrow capabilities.**

The fix, **now built**, is a host-side capability broker that holds the raw credential and never
projects it into a container: agents present their mTLS identity and exchange it for a short-TTL,
CN-bound capability scoped to exactly what their policy allows. The broker rides the existing egress
fence (reachable only via the allowlisted host alias) and is the sole minter, so a worker's token is
strictly weaker than the manager's and a delegated token is an online mint, never an offline hand-off.
§6.1 and §6.2 describe the built, enforced state.

### 6.1 Built state (api-key mode) and the mixed-instance residual

The capability broker is now built: an agent in **`llm_auth: api-key`** mode (the default) holds only
a short-lived, CN-bound `capability(llm)` token and routes the model through the broker `/llm` proxy,
which strips the token and injects the real Console key host-side. Such an agent **never receives a
real Anthropic credential**. Adding **`egress: closed`** (valid only for a uniformly api-key instance)
then seals outbound network to the broker alone ([§2.2](/security-model/jail/)), closing the §6 blast radius entirely for that
instance.

**Mixed instances are unsupported, rejected at config validation.** The OAuth credential is set as a
single Hub *secret* (`internal/scion/bringup.go:SecretSet`, user/owner scope), gated only by
`manager.credential_file` and **projected into every container regardless of its `llm_auth` mode**.
So in a *mixed* instance (any subscription agent ⇒ `credential_file` set ⇒ token hub-projected) an
`api-key` worker would *also* hold `$CLAUDE_CODE_OAUTH_TOKEN`, letting it read the real token and
(egress permitting) reach `api.anthropic.com` directly, bypassing the proxy's capability gating; its
key isolation would silently not hold.

Rather than ship that footgun, **an instance must be uniformly api-key OR uniformly subscription**:
`App.Validate` (`internal/config/config.go:validateBroker`) rejects any config whose *effective*
agent modes mix the two (`mixedLLMAuth`), so a mixed instance never reaches apply. The two pure cases
are both clean: **all-api-key** (the default) = no agent holds a real key, capabilities gated by
signed tokens, and `egress: closed` is available to seal the network jail-wide; **all-subscription**
= every agent holds the OAuth key with open egress (the owner/dev trade, by design). This is
*not* an escalation surface: the `api-key` flag controls only whether an agent obtains a capability
token, **not** credential availability, and a worker's mode is fixed by the config the broker reads
(nothing the manager can rewrite), so it could never *conjure* a token the host did not project; the
validation gate is about preventing a misleading config, not about containment.

To *support* mixed instances later would require per-container egress and/or projecting the OAuth secret
only into subscription agents' projects (not as a hub-wide secret), deferred; until then, mixed is a
hard config error.

### 6.2 What the broker enforces (built lock-downs)

The capability model itself — identities, minting, delegation, revocation — is described in
[capabilities.md](/capabilities/). This section lists the shipped, code-enforced properties, the
*how* behind §6.1:

- **Capabilities are mTLS-CN-bound and non-transferable.** Every token carries an intrinsic check
  `caller == bound_agent` (`internal/cap/token/token.go`); the broker authenticates the caller from its
  verified client-cert CN and fails closed without one (`ca.RequireAgent`, called first on `/request`,
  `/llm`, and every brokered tool route). A stolen token is useless without that agent's
  in-container private key, which is generated in the container and never leaves it.
- **Per-call epoch + revocation is the real cut, not the TTL.** A generous 24h grant TTL is only a
  backstop; on **every** call the broker re-checks the live revocation set and `MinEpoch`
  (`/revoke` and `/bump-epoch`, persisted across restarts and seeded at construction). Revoking an
  agent or bumping the epoch denies its outstanding tokens immediately, this is verified for both the
  `/llm` proxy and first-party tool calls.
- **Constraints can only narrow, and only the broker mints.** `/request` validates each requested
  constraint against the tool's `AllowedValues` (fail closed) and bakes it into the signed token as an
  equality check on that request parameter (`internal/cap/token`), so the minted capability is usable
  only for that exact value. The broker is the sole minter, there is no offline way to widen a token,
  and any tampered token fails Ed25519 signature verification.
- **Forged identity headers are scrubbed.** The broker deletes every inbound `X-Lever-*` header before
  processing a tool call (`internal/broker/gateway.go`), then sets `X-Lever-Caller` itself from the
  verified CN, a jail agent cannot forge broker-internal context.
- **The admin surface is loopback-only.** `/register`, `/revoke`, `/bump-epoch`, `/bootstrap`, `/epoch`
  are unauthenticated and protected solely by binding to loopback, enforced twice and fail-closed
  (`resolveAdminAddr` rejects any non-loopback bind; `ServeListeners`). The jail reaches the broker only
  via the *separate* mTLS jail listener, never the admin port.
- **`/bootstrap` is single-use.** The first manager-enrolment ticket latches the broker; every later
  `/bootstrap` returns 403 (`internal/broker/bootstrap.go`). `apply` tolerates that 403 so re-apply is
  idempotent, but the latch bounds manager-identity minting to one per broker process.
- **The api-key placeholder is a sentinel, not a credential.** scion's start-time auth gate needs *some*
  credential before the container (and thus `lever-agent boot`) can launch, so api-key mode sets a fixed
  placeholder `ANTHROPIC_API_KEY` (`sk-ant-placeholder…`) as a Hub secret. It is not a real key: claude
  sends it as `x-api-key`, which the broker `/llm` proxy **overwrites** with the real Console key
  host-side. Projecting it to every container is safe precisely because the instance is uniformly
  api-key (§6.1).
- **Operator directives authenticate the requester; they mint no capability.** A separate,
  operator-signed channel lets an agent verify that an instruction genuinely came from the human
  operator (its own SSH signing key, held to a similar host-side trust posture as the credentials
  above — own-machine or hardware-backed by default). A consumed directive only ever hands the agent
  a validated action *descriptor*; the agent still executes it through the ordinary capability path
  described in this section, subject to the same `MayObtain`-at-mint and `token.Verify`-at-call
  checks. See [Operator directives](/security-model/operator-directives/) (§11) for the full threat
  model, including the honest Phase-1 scope (delivery and verification are built; call-time
  enforcement is not).

**External MCP servers (broker-fronted).** A `broker.tools` entry with `external: true` is a
host server the broker *fronts but does not spawn*: it registers from config, is gated on the
broker's mTLS listener like any third-party tool (token verified, then **stripped** — the server
never sees a capability), and replaces the old ambient pattern (per-server `allow_ports` holes + a
`.mcp.json` pointing at the host alias). Three honest boundaries:

- **`gate: coarse` is wholesale trust.** One wildcard capability (`{tool,"*"}`) admits every
  MCP call the server exposes, including destructive ones. It is honored *only* for a
  declared-coarse tool — the broker picks the required op per tool, so a wildcard token can
  never satisfy a `fine` tool — and the audit log records the real MCP tool called either
  way. Put sensitive servers behind `gate: fine`.
- **The loopback-backend guard closes a LAN bypass.** The broker proxies host-side, so a
  non-loopback `backend` would hand a jailed agent a path to other hosts *through the
  broker*, circumventing the jail's LAN-drop egress. Config validation rejects it unless the
  tool sets `allow_non_loopback: true` (an explicit, reviewed opt-in).
- **The gate protects the *jailed agent's* path, nothing more.** Any other host-local
  process can still hit the server's `127.0.0.1` port directly — host processes are already
  inside the host trust boundary; the broker does not claim to sandbox them from each other.

### 6.3 Leaf rotation and the re-read invariant

The mTLS identity this section rests on — the cert an agent presents to mint a capability, and
the CN every token is bound to — is a **24h leaf** signed at enrolment; the short life bounds the
exposure window of a leaked agent key to a day, backstopping the per-call epoch + revocation that
is the real cut (§6.2). The private key is generated inside the container and never leaves it;
enrolment rides a single-use, CN-bound ticket (§6.2). One operational invariant is
security-relevant, because getting it wrong is a silent availability failure that masquerades as
the broker being down: **every long-lived broker client must re-read the rotating leaf per TLS
handshake**, or all brokered tools fail together at the 24h mark while the broker stays healthy.
The full lifecycle — enrolment, the `lever-renew` sidecar, the broker's self-rotating serving
cert, and the two long-lived clients — is on
[Agent identity & certificates](/agent-identity/).
