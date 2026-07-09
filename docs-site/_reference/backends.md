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
   No backend without a VM boundary is added to `lever backends` (see [Rejected](#rejected) for the
   backend this ruled out).
1. **No host filesystem** beyond the one chosen project tree.
2. **A network namespace Lever controls**, so the egress allowlist can be enforced *outside* the
   agent containers.
3. **Egress enforced in that namespace**, not by the agent behaving.
4. **A host-reachable capability-broker endpoint** for capability, LLM, and tool traffic.

See [security model §2.4](/security-model/) for the full contract in context.

Run `lever backends` to print the live matrix; this page mirrors it, then adds the roadmap and
rejected backends the command doesn't list. `lever backends` prints only **implemented**
candidates, a candidate exists in code iff a registry constructor exists for it, so roadmap and
rejected entries live here as documentation, not in the binary. Select a backend with the
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
files and has its own network namespace. This is the validated substrate today and the reference the
other backends are measured against. Its trade: a **single kernel** shared across the manager and all
workers (a kernel-level container escape reaches the whole jail — see [security model §7](/security-model/)).

### `lima` — the non-OrbStack path

The [Lima](https://lima-vm.io) VM backend: macOS (`vz`) and Linux (QEMU/KVM), for anyone who does
not run OrbStack. It preserves the VM boundary, its own kernel, not shared with the host or with
other jails, so its guarantees match `orbstack`'s. The containment surface is a **lever-owned
template** (stock Lima templates are not used), built from three mechanisms:

- **Exactly one writable mount**: the project tree, at `/lever`. Nothing else, in particular not
  Lima's stock `~` read-only home mount.
- **All automatic guest→host port-forwarding suppressed.** Lima's default forwards *every* guest
  listener to the host's `127.0.0.1`, live-confirmed 2026-07-02: on a stock template, a guest-side
  `0.0.0.0` listener is reachable at the host's loopback. Left on, a jailed agent could squat a
  host-loopback port and impersonate a local service (a dev server, a credential helper). The
  lever template's `portForwards` block carries an ignore-all rule for both `0.0.0.0` and
  `127.0.0.1` guest binds, closing it.
- **Lima's bundled containerd disabled** (`containerd: {system: false, user: false}`). Lever
  provisions rootless podman/Docker itself, exactly as it does for `orbstack`.

Requires **Lima ≥ 2.0.0**, checked at bring-up. The floor exists because Lima 2.0 changed
`portForwards` ignore-rule semantics (`guestIPMustBeZero` auto-inference for a `0.0.0.0` guest
bind); an older `limactl` could silently forward guest ports despite the rendered ignore rules
being present. The template also sets `guestIPMustBeZero: true` explicitly, so the containment
property doesn't depend on the auto-inference alone.

`host.lima.internal` (live-confirmed reaching host-loopback services, resolving to `192.168.5.2` in
testing) is the host alias, the direct analog of OrbStack's `host.orb.internal`: it's how an agent
reaches the broker and any allowlisted host tool port.

## Roadmap

### `apple-container`

[Apple's `container`](https://github.com/apple/container) runs each Linux container in its **own
lightweight VM**, so every agent gets its own kernel, the strongest isolation of any Mac option
(guarantee 0 satisfied *per agent*, not just per jail). Not yet built, for two reasons:

- **A different topology.** With a VM per agent there is no single netns to hang one egress
  chokepoint on; guarantees 2-3 would need per-VM enforcement or a gateway, not the single-jail
  chain the other backends share.
- **Immature networking.** Full support needs macOS 26.

## Rejected

### Docker Desktop

Docker Desktop is a container **runtime**, not a jail. Its VM is *shared* infrastructure that
auto-mounts your home directory and whose network namespace you do not control, so it satisfies
neither guarantee 1 nor guarantee 2. Running Lever's containers directly in it would appear to work
while silently voiding containment. To use it safely you would have to stand up a dedicated
hardened VM under it, which is the `lima` path. Lever therefore does not offer a Docker Desktop
backend.

### `linux-docker` — a native, no-VM Linux backend

Explored and rejected 2026-07-02. The design got as far as a full jail: a lever-owned network
namespace, a mount namespace with the project tree as the one writable mount plus `tmpfs` shadowing
the rest of the user's data, and nftables/iptables egress enforcement in that namespace, all
root-owned so the jail user could not modify them. **The egress and filesystem answers were
sound.** What killed it: a native jail shares the host kernel. No namespace substitutes for a
hypervisor boundary, so an adversarial agent (Lever's own threat model, see
[security model §1](/security-model/)) is one kernel-level privilege escalation from host root,
violating guarantee 0 outright. It's rejected on the same grounds as Docker Desktop, from the
opposite direction: Docker Desktop fails guarantees 1-2 despite having a VM; `linux-docker` would
have satisfied guarantees 1-3 but had no VM at all.

Recorded for honesty rather than deleted, because the underlying audience is real: a Linux host
with no nested-virtualization support (a bare VPS, some CI runners) cannot run `lima`'s QEMU/KVM
path or any other VM backend. A **weaker, explicit-opt-in** native backend for that case remains a
possible future direction, for an operator who knowingly accepts a shared kernel because the
hardware has no other option, never a default and never silently substituted for a VM backend. Not
built.
