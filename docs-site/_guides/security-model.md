---
title: Security model
nav_order: 5
---
# Security model

Lever's threat model is deliberately harsh: **assume any agent, including the manager, can be
prompt-injected or otherwise turned hostile.** An autonomous agent that processes untrusted content
(web pages, dependencies, issue text, tool output) can be steered into running arbitrary code. The
design goal is not "prevent compromise" but **bound the blast radius of a compromise to a single
directory subtree and a curated set of network endpoints, enforced by the OS, not by the agent
behaving.**

This page holds the core idea and the summary; the mechanism detail lives in the sub-pages below.
Section numbers (§N) are stable across the split — each sub-page keeps its original numbering.
What is shipped versus pending is tracked on [validation](/security-model/validation/).

| Section | Page |
|---|---|
| §2 The jail (filesystem, network egress, rootless podman, substrates) | [The jail](/security-model/jail/) |
| §4 Cross-worker isolation (defense by absence, hub authority) | [Worker isolation](/security-model/worker-isolation/) |
| §5 The operator boundary (config out of the mount, validation, dispatch) | [Config trust](/security-model/config-trust/) |
| §6 Credential blast radius and the capability broker | [Credentials & capabilities](/security-model/credentials/) |
| §7–§8 What a compromised agent can and cannot do; non-claims | [Compromise scenarios](/security-model/compromise/) |
| §9 Validation evidence and current status | [Validation](/security-model/validation/) |
| §11 Operator directives — an authenticated human channel (honest Phase-1 scope) | [Operator directives](/security-model/operator-directives/) |

§5 covers the operator's control over instance *configuration*; §11 covers a verifiable channel for
the operator's *runtime* instructions to a live agent — the two close different halves of the same
"who is the operator, and how does an agent know it" boundary.

## 1. The core idea: put the boundary *outside* the runtime

Scion's **broker** (its host-side component that creates containers and applies mounts) is a
*confused deputy*: it asks the **container runtime** to bind-mount host paths and to join networks, and
the runtime obliges. Concretely, the escape that motivated this design works because Scion's **hub
performs no path validation**, any caller can register a project provider with an arbitrary host
path, and the broker will then mount it (see [§9](/security-model/validation/)).

**The real boundary is the container runtime Scion drives, plus the environment that runtime runs
in, not the runtime's code.** Constrain the runtime's filesystem and network view, and it can ask
for anything it likes; it cannot exceed what the environment physically permits. This is why **no
fork of Scion is needed** for containment. It is enforced by the jail around it.

> This concerns the **host-containment** boundary (host filesystem, credentials, and LAN). A
> separate, finer boundary — confining each *worker* to its own subtree so siblings cannot read
> one another — relies on one small Scion capability (a relative `--workspace`, resolved against
> the project root with a containment guard), merged upstream in
> [scion#815](https://github.com/GoogleCloudPlatform/scion/pull/815). That capability does **not**
> affect the host-containment guarantee above: a worker that mounted the whole tree would still be
> fully jailed from the host; it simply would not be isolated from its sibling workers.

## 3. What containment buys

| Concern | Without the jail | With the jail |
|---|---|---|
| `~/.ssh`, cloud creds, host `$HOME` | mountable by a compromised agent via the broker | **not in the environment**, nothing to mount |
| runtime's own host-side secrets | reachable via mount escape | absent from the jail |
| arbitrary host path bind-mount | accepted by the runtime (hub does no path validation) | bounded to the project tree |
| host LAN / business network | full reach via host networking | **unreachable** (OrbStack routing; firewall to back it) |
| host loopback (local tools) | n/a | only allowlisted `host:port`s; rest dropped |
| real LLM credential in every container | ambient, shared, long-lived OAuth token in every agent | **api-key mode:** no real key in any container, only a CN-bound, short-lived `capability(llm)` token; the broker injects the Console key host-side ([§6.1](/security-model/credentials/)) |
| exfiltration of in-tree data | n/a | **not bounded** in subscription mode; narrowed (not eliminated) under api-key closed egress, see [§8](/security-model/compromise/) |

**Result:** an injected manager or worker can reach neither host secrets nor the LAN; its blast
radius is the project subtree it was given, *plus* whatever it can send over allowed internet
egress ([§8](/security-model/compromise/)).

## 10. Reporting a vulnerability

Pre-release; a security contact will be published with the first release. If you find a containment
hole in the meantime, please open a minimal-detail issue and request a private channel.
