---
title: Containment backends
nav_order: 2
---
# Containment backends

A **containment backend** is the substrate that provides Lever's jail. The jail is a *contract*, not
a single product: a backend must provide (1) no host filesystem beyond the one chosen project tree,
(2) a network namespace Lever controls, (3) egress enforced in that namespace, and (4) a
host-reachable broker endpoint. See [security model §2.4](/security-model/) for the full contract.

Run `lever backends` to print the live matrix; this page mirrors it. Select a backend with the
[`backend:` key](/reference/config/) in `lever.yaml`. Only **implemented** backends can be selected —
a `planned` or `experimental` value is rejected at config load rather than silently substituted, so a
containment posture is never quietly swapped for another.

| Backend | Status | Kernel boundary | FS bounded by | Egress enforced at | Version-fragile |
|---|---|---|---|---|---|
| `orbstack` | implemented | shared jail-VM kernel | isolated machine: no host files + one bind mount | jail netns iptables/ip6tables | yes |
| `linux-docker` | planned | none (host kernel) | host netns+userns + one bind mount | jail netns nftables/iptables | no |
| `lima` | planned | own VM kernel | VM: no host files + one bind mount | jail netns iptables/ip6tables | yes |
| `apple-container` | experimental | per-agent VM kernel | per-agent VM: no host files + mount | per-VM / gateway | yes |

## Backends

### `orbstack` — implemented (reference)

macOS on Apple Silicon with [OrbStack](https://orbstack.dev). The runtime, the Scion server/broker,
rootless Docker, and every agent run inside one OrbStack **isolated machine** that shares no host
files and has its own network namespace. This is the validated substrate today and the reference the
other backends are measured against. Its trade: a **single kernel** shared across the manager and all
groves (a kernel-level container escape reaches the whole jail — see [security model §7](/security-model/)).

### `linux-docker` — planned

A native-Linux backend with **no VM**: the jail is built from kernel primitives (a network namespace,
a user namespace, a single locked-down bind mount, and nftables/iptables in that namespace). Stronger
on filesystem and egress than a VM backend, and it drops the virtiofs metadata tax entirely — but
honest about one thing: **no separate kernel.** A kernel-level escape reaches the host. The trade
Linux users accept for skipping the hypervisor.

### `lima` — planned

The [Lima](https://lima-vm.io)/Colima VM backend — the true OrbStack *alternative* for macOS (and
Linux) users who do not run OrbStack. It preserves the VM boundary (its own kernel), so its guarantees
match `orbstack`'s; it exists so the reference posture does not depend on one vendor.

### `apple-container` — experimental

[Apple's `container`](https://github.com/apple/container) runs each Linux container in its **own
lightweight VM**, so every agent gets its own kernel — the strongest isolation of any Mac option, and
it turns the shared-kernel trade above into a non-issue. It fits a **different topology** than the
single-jail model: with a VM per agent there is no single netns to hang one egress chokepoint on
(egress is enforced per-VM or via a gateway), and its networking is young — full support needs macOS
26. Hence *experimental*.

## Not a backend: Docker Desktop

Docker Desktop is a container **runtime**, not a jail. Its VM is *shared* infrastructure that
auto-mounts your home directory and whose network namespace you do not control, so it satisfies
neither the no-host-filesystem nor the controllable-netns guarantee. Running Lever's containers
directly in it would appear to work while silently voiding containment. To use it safely you would
have to stand up a dedicated hardened VM under it — which is the `lima` path. Lever therefore does not
offer a Docker Desktop backend.
