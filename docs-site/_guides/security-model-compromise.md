---
title: "Compromise scenarios"
nav_order: 5.5
permalink: /security-model/compromise/
---
Part of the [security model](/security-model/). Sections keep their original § numbers.

## 7. If an agent is compromised: what it can and can't do

Prompt injection, a poisoned dependency, a malicious tool description — assume any agent *can* be
turned. This section states what a compromised agent achieves in each scenario, and what it does
not.

**What containment holds (the same in every posture):**

- It cannot read your host filesystem, your `~/.ssh`, your other projects — only its own mounted
  tree. The host home is not mounted (§2.1).
- It cannot reach your LAN or other machines; egress is allowlisted (§2.2).
- It cannot obtain the real model key: in api-key mode the key is injected host-side and never
  enters the container (§6). (In subscription mode the projected OAuth token *is* exposed — see
  below.)
- Capability tokens it holds or mints are CN-bound and revocable: leaked token text is inert
  elsewhere, and `lever revoke <agent>` cuts it off at use time (see [capabilities](/capabilities/)).
- Other agents' trees and tokens are not reachable — cross-agent authority goes through the broker,
  default-deny.

**What it can do, and you must assume it did:**

- **Anything within its own bind-mounted tree is now suspect.** A compromised agent can rewrite any
  file it can reach: its `CLAUDE.md` (which the harness auto-loads on every future boot), the
  scaffolded skills, source files, and it can plant scripts, hooks, or data that a later run — or
  a *human* — might execute or trust. The tree is inside its blast radius by design.
- **Use whatever tools it was granted, as itself.** Compromise doesn't widen grants (the broker
  still gates every call), but it can misuse what you *did* grant — so grant least-privilege, and
  in subscription mode treat the projected OAuth token as exposed.
- **Exfiltrate in-tree data over its allowed egress.** See §8 — this is not bounded in
  subscription mode, and only narrowed (not eliminated) in api-key mode.

> **Operator warning — treat a compromised agent's tree as contaminated.** Restarting the agent,
> even with `lever up --fresh`, gives a fresh *conversation*, not a fresh *tree*: a modified
> `CLAUDE.md`, a planted script, or tampered working files persist and will contaminate the next
> run. Before you re-run that agent, dispatch a new one onto the same tree, or attach that tree to
> any other system, **the human operator must audit the tree and confirm it is clean** (or restore
> it from a known-good state / discard it). Containment limits the blast radius to that tree; it
> does not clean the tree for you.

## 8. What this model does *not* claim

- **Data-exfiltration protection.** The bound is on host-secret and LAN reach, **not** on
  exfiltrating in-tree data. This differs by posture (§2.2):
  - *Subscription:* with open internet egress (allowed, for the model API and package installs), a
    compromised agent can leak anything in the tree, the model-API endpoint alone is a covert
    channel. Tightening this would require an egress proxy / data-loss controls not yet specified.
  - *api-key (closed egress):* arbitrary internet egress is closed, so the only outbound path is the
    broker → fixed `api.anthropic.com`. That **narrows but does not eliminate** exfiltration: the
    `/llm` proxy is still a covert channel. The proxy scrubs response *headers*
    (`WWW-Authenticate`, `x-api-key`) and only ever talks to the fixed trusted upstream (no SSRF), but
    it streams the SSE response **body** unbuffered and so cannot scrub it, an accepted residual.
  - **Closed by failing closed:** the design anticipated a possible fallback where `/llm` would
    authorize on the token alone if Claude Code presented no client cert. The shipped code does the
    opposite, `ca.RequireAgent` runs first and returns 403 with no cert, so the `capability(llm)`
    token is always CN-bound; there is no non-bound path.
- **Containing runtime authority inside the jail.** The architecture depends on the Scion broker
  driving the container runtime. Any agent that holds the rootless podman socket directly can launch
  arbitrary (including `--privileged`) containers *within the jail*. (No agent, manager included,
  holds Scion hub authority itself, that lives host-side only, gated by the controller PAT, §4.2,
  so an agent driving the hub to launch containers is not a route here.) Containment of direct
  runtime-socket access rests entirely on the jail's filesystem/network bound and the kernel, not on
  denying runtime access.
- **A separate kernel.** An isolated machine shares the host VM's kernel, and the manager and all
  workers share that one kernel *with each other*. So a kernel-level escape from any single agent
  defeats inter-agent isolation wholesale and reaches the VM, not merely host-secret protection.
  For a hypervisor-hard boundary between the agent workload and the *host* kernel, the `lima`
  backend is the stronger substrate (its own VM kernel, not shared with the host); the manager and
  its workers still share *that* kernel with each other, the same inter-agent trade one level up.
  The OrbStack isolated machine is judged sufficient for a single-operator workstation under this
  threat model. This is an explicit, documented trade — see the
  [containment backends](/reference/backends/) matrix to compare before choosing.
- **Hostile multi-tenant / cloud.** The model targets a single operator's workstation. A
  remote/cloud deployment faces a harsher threat model and would need the inner layer (§4), egress
  controls (above), and hardening not yet specified.
- **Defeating a kernel 0-day or a Docker/OrbStack escape.** Containment rests on the correctness of
  the kernel, the container runtime, and the hypervisor. Lever reduces attack surface; it does not
  make those components infallible.
- **Stability across OrbStack versions.** Containment depends on specific, empirically-discovered
  OrbStack `--isolated` behaviours (no host home; the `bpf()` seccomp block forcing rootless;
  host-alias-reachable-but-LAN-not routing). These are validated against current OrbStack and **must
  be re-validated on upgrades**; provisioning resolves the host alias dynamically rather than
  hardcoding it.
