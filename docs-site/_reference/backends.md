---
title: Containment backends
nav_order: 2
---
# Containment backends

A **containment backend** is the substrate that provides Lever's jail. The jail is a *contract*, not
a single product, and one of its guarantees decides whether a backend can even exist in code.

## The jail contract

0. **A hypervisor boundary between the agent workload and the host kernel.** Mandatory. Agents run
   arbitrary, potentially adversarial code with real credentials (see the threat model in
   [security model §1](/security-model/)); a network/mount/user namespace is not a substitute for a
   separate kernel, one kernel-level exploit from any agent reaches everything sharing that kernel.
   No backend without a VM boundary is added to `lever backends`.
1. **No host filesystem** beyond the one chosen project tree.
2. **A network namespace Lever controls**, so the egress allowlist can be enforced *outside* the
   agent containers.
3. **Egress enforced in that namespace**, not by the agent behaving.
4. **A host-reachable capability-broker endpoint** for capability, LLM, and tool traffic.

See [security model §2.4](/security-model/jail/) for the full contract in context.

Run `lever backends` to print the live matrix; this page mirrors it. A candidate exists in
code iff a registry constructor exists for it. Select a backend with the
[`backend:` key](/reference/config/) in `lever.yaml`; an unrecognised name is rejected at config
load rather than silently substituted, so a containment posture is never quietly swapped for
another.

## Implemented

| Backend | Kernel | FS bounded by | Egress enforced at | Version-fragile |
|---|---|---|---|---|
| `orbstack` | shared | isolated machine: no host files + project tree mounted at `/lever` | jail netns iptables/ip6tables | yes |
| `lima` | separate | VM: no host files + project tree mounted at `/lever` | jail netns iptables/ip6tables | yes |

(Columns mirror `lever backends`' own output.)

### `orbstack` — reference

macOS on Apple Silicon with [OrbStack](https://orbstack.dev). The runtime, the Scion server/broker,
rootless podman, and every agent run inside one OrbStack **isolated machine** that shares no host
files and has its own network namespace. This is the reference substrate the other backends are
measured against. Its trade: a **single kernel** shared across the manager and all
workers (a kernel-level container escape reaches the whole jail — see [security model §8](/security-model/compromise/)).

### `lima` — the non-OrbStack path

The [Lima](https://lima-vm.io) VM backend: macOS (`vz`) and Linux (QEMU/KVM), for anyone who does
not run OrbStack. It preserves the VM boundary, its own kernel, not shared with the host or with
other jails, so its guarantees match `orbstack`'s. The containment surface is a **lever-owned
template** (stock Lima templates are not used), built from three mechanisms:

- **Exactly one writable mount**: the project tree, at `/lever`. Nothing else, in particular not
  Lima's stock `~` read-only home mount.
- **All automatic guest→host port-forwarding suppressed.** Lima's default forwards *every* guest
  listener to the host's `127.0.0.1`: on a stock template, a guest-side `0.0.0.0` listener is
  reachable at the host's loopback. Left on, a jailed agent could squat a
  host-loopback port and impersonate a local service (a dev server, a credential helper). The
  lever template's `portForwards` block carries an ignore-all rule for both `0.0.0.0` and
  `127.0.0.1` guest binds, closing it.
- **Lima's bundled containerd disabled** (`containerd: {system: false, user: false}`). Lever
  provisions rootless podman/Docker itself, exactly as it does for `orbstack`.

Requires **Lima ≥ 2.0.0**, checked at bring-up: the ignore rules depend on Lima 2.0's
`portForwards` semantics, and an older `limactl` forwards guest ports despite the rendered ignore
rules. The template sets `guestIPMustBeZero: true` explicitly, so the containment property does
not depend on Lima's auto-inference.

`host.lima.internal` (resolving to `192.168.5.2`) is the host alias, the direct analog of OrbStack's `host.orb.internal`: it's how an agent
reaches the broker and any allowlisted host tool port.

**Guest DNS goes through the host alias too.** Lima's guest resolver (`192.168.5.3`) is a nat-table
DNAT (Lima's `LIMADNS` chain) to the host agent's DNS server at `host.lima.internal:<per-boot
port>`, and nat `OUTPUT` runs before filter `OUTPUT`, so every lookup reaches `LEVER_EGRESS` as a
new dial to the alias on a non-allowlisted port. Under `egress: open`, lever reads the live
`LIMADNS` chain at apply time and ACCEPTs exactly those DNAT targets ahead of the alias DROP, so the
guest and every agent container resolve names through the host's own resolver (VPN and split-DNS
included). Without that carve-out (lever < 0.22.2) a subscription-mode agent never resolved
`api.anthropic.com` and every turn ended in `Request timed out` while `lever up` and `lever doctor`
stayed green. Under `egress: closed` DNS stays dropped by design; agents dial the broker by IP.
`lever doctor`'s `guest DNS` row resolves a public name from inside the guest so a DNS-dead jail
is no longer indistinguishable from a healthy idle one. OrbStack is unaffected: its resolver path
is not in the dropped set.
