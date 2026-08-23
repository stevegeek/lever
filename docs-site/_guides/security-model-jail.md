---
title: "The jail"
nav_order: 5.1
parent: Security model
permalink: /security-model/jail/
---
Part of the [security model](/security-model/). Sections keep their original § numbers.

## 2. The jail

The entire runtime, Scion server, broker, the container runtime, and every agent, runs inside one
**isolated machine** (an OrbStack isolated Linux guest that shares no host files and has its own
network namespace by default). The `lever` operator binary runs on the host and drives in. The jail
provides two properties:

### 2.1 Filesystem: the secrets are not in the environment

An isolated machine **shares none of the host's files by default.** Lever opts **only the project
tree** in (a single bind mount). Therefore:

- The broker's "bind-mount any host path" power is bounded to the project tree, the only directory
  the jail can see. `~/.ssh`, cloud credentials, the runtime's own host-side state, and the rest of
  the disk **are not in the daemon's filesystem at all**, so a malicious mount of them fails or
  comes up empty.
- This is **removing the secret from the environment**, not fencing an attacker away from a secret
  that is present. No mount-allowlist patch to the runtime is required; the path simply does not
  exist to mount.

### 2.2 Network: the LAN is unreachable; egress is allowlisted

Inside the jail, containers that use host networking share the *jail's* network namespace, not the
host's. Validated behaviour and its limits:

- **The LAN is unreachable.** From inside the jail, other devices on the local network (router,
  NAS, other hosts) cannot be reached; only the host *itself* is, via a runtime alias. This rests on
  OrbStack's default routing for an isolated machine (it is *not* the `--isolate-network` mode, which
  would also cut the host alias we need for tools), **backed by explicit firewall drops** so it does
  not depend on OrbStack routing alone: the allowlist drops the private/special-use ranges
  unconditionally, `10/8`, `172.16/12`, `192.168/16`, **`169.254/16` (link-local/metadata)**,
  **`100.64/10` (CGNAT/Tailscale)**, and IPv6 `fe80::/10` + `fc00::/7`
  (`internal/egress/allowlist.go`). LAN-unreachability must still be re-checked on OrbStack upgrades.
- **Host loopback services are reachable via the alias** (`host.orb.internal`), which forwards to
  the host's `127.0.0.1` over **both IPv4 and IPv6**. This is how the manager reaches local tool
  servers (e.g. the broker, an MCP server) without exposing them on the LAN. The alias exposes *all*
  host loopback, so it must be clamped.

**Network egress: two postures.** All lever rules live in a dedicated **`LEVER_EGRESS`** iptables/
ip6tables chain that `OUTPUT` jumps to (never direct `OUTPUT` rules), so re-apply flushes and rebuilds
only that chain (idempotent, and it never touches non-lever rules). The posture is chosen by the
explicit, jail-wide **`egress:`** knob, **independent of `llm_auth`**:

- **`egress: open` (default):** `OUTPUT` default-ACCEPT; `LEVER_EGRESS` ACCEPTs the allowlisted host
  `host:port`s on the alias, DROPs the rest of the host alias and the private ranges above. **Public
  internet stays open** (for the model API and package installs). See the exfiltration caveat in [§8](/security-model/compromise/).
- **`egress: closed`:** additionally a **catch-all DROP** for both families at the end of the chain,
  with loopback (`-o lo`) ACCEPTed *first* so the in-machine scion hub (127.0.0.1:8080) and host-alias
  tools keep working. The jail can then reach **only** the already-ACCEPTed broker port; arbitrary
  public-internet egress is closed. **`closed` is valid only for a uniformly api-key instance** (a
  subscription agent needs direct internet to reach Anthropic), enforced at config load.
- **Reachability under closed egress.** With the catch-all DROP, DNS/53 is dropped too, so an agent
  cannot resolve `host.orb.internal`. Agents therefore dial the broker by its **resolved alias IP**
  (already allowlisted), and the broker mints its server cert with that **IP as a SAN** so TLS still
  validates (`internal/cap/ca/rotate.go NewServerCertSource`, which mints via `IssueServerCertSANs`;
  `internal/brokerctl/serve.go` passes the IP from `$LEVER_HOST_ALIAS_IP`, which
  `internal/cli/apply.go` sets on the broker child). Re-applying a *live* closed instance detects the active catch-all DROP
  and skips the flush/rebuild, so egress is never momentarily reopened under a running agent.

**Enforcement** lives in the jail's network namespace, for both postures. A **non-privileged** agent
container is in a separate namespace from the rules and cannot flush them; reaching the rules would
require a container/namespace escape (which reduces to the kernel/runtime caveats in [§8](/security-model/compromise/)).

### 2.3 Rootless podman (required)

Scion runs the agents in **rootless podman** (it auto-prefers podman over Docker; a rootless Docker
daemon is provisioned in the guest too, but the agent containers are podman). Inside an isolated
machine, **rootful containers do not work** and a **rootless runtime is required.** An isolated
machine applies a seccomp filter that blocks the `bpf()` syscall, which a rootful OCI runtime
(`runc`/`crun`) needs to program the cgroup-v2 device controller; containers fail with
`bpf_prog_query(BPF_CGROUP_DEVICE) failed: operation not permitted`. This is the isolation hardening
itself, **not** a missing capability (the machine has full capabilities; a *non-isolated* machine
runs rootful nested containers fine). A rootless runtime does not manage device cgroups, so it never
calls `bpf()` and containers run normally. Rootless is also a security **bonus**: an extra
user-namespace layer around every agent. On the OrbStack VM's modern kernel the rootless runtime uses
native `overlayfs` (not the slow userspace `fuse-overlayfs`), so the performance cost is small.

### 2.4 Substrates: the jail is a contract, not a product

Everything above describes OrbStack, but the jail is a **contract**. A containment backend must
provide a hypervisor boundary between the agent workload and the host kernel (guarantee 0, added
2026-07-02 after the native-Linux backend was rejected on that ground), no host filesystem beyond
the project tree (§2.1), a network namespace Lever controls with egress enforced in it (§2.2), and a
host-reachable broker endpoint. The full contract, the per-backend guarantee matrix, the `lima`
template mechanism, the `apple-container` roadmap entry, and the rejected Docker Desktop and
`linux-docker` backends are on the [containment backends](/reference/backends/) page. Config
validation rejects any backend name that is not implemented rather than falling back to OrbStack,
so a containment posture is never silently substituted.

**Lima operational notes**, from the T13 security review:

- **Lima's in-guest kernel attack surface is intentionally widened for rootless runtimes.**
  Provisioning re-enables the unprivileged user-namespace knob
  (`kernel.apparmor_restrict_unprivileged_userns=0`) that Ubuntu ≥ 23.10 disables by default, a
  prerequisite for rootless Docker/Podman's rootlesskit/pasta. This widens attack surface *inside the
  guest kernel* only, in exchange for the rootless containers the whole containment model depends on;
  the boundary that actually matters — the hypervisor (guarantee 0) — is untouched. An escalation via
  this surface reaches VM root, not the host, consistent with the [§8](/security-model/compromise/) "containing runtime authority
  inside the jail" stance (in-jail privilege escalation is accepted; the jail's own bound is not).
- **The jail VM survives host reboots, and also `lever stop`.** It is destroyed only by `lever
  destroy` (`limactl delete --force`; `lever down` is a deprecated alias); there is no
  reboot-triggered teardown. `lever stop` (`limactl stop`) merely powers the VM off — its disk is
  preserved, and the next `lever up` resumes it. "Throwaway guest" therefore means per-`lever
  destroy`, not per-boot and not per-`lever stop` — a long-lived instance keeps the same guest state
  (and any in-guest compromise) across host restarts and stop/resume cycles until explicitly
  destroyed.
- **The egress allowlist (§2.2) depends on the VM/rootless boundary above it.** Every no-reopen /
  allowlist property in this document assumes the agent lacks `CAP_NET_ADMIN` in the VM's *init*
  network namespace — enforcement lives in that namespace, not the agent's container namespace (§2.2).
  A container→VM-root escape would let the agent rewrite `LEVER_EGRESS` directly, bypassing the
  allowlist rather than merely being contained by it; that escape reduces to the kernel/runtime
  caveats in [§8](/security-model/compromise/).
- **A global lima config can widen the containment surface beyond what the lever template
  requests.** `~/.lima/_config/{default,override}.yaml`, if present on the host, is merged into every
  lima instance's *realized* config, including this one — an operator's own global lima settings could
  add a mount or port-forward the lever-rendered template (template.go) never asked for. This is a
  host-operator supply-chain concern (an attacker would need to already control the operator's
  `~/.lima/_config`, not just the guest), not a guest-exploitable path. The realized-config drift check
  (`Lima.verifyRealizedConfig`, `internal/backend/lima/lima.go`) is the backstop: `lever up` fails
  closed if the VM's live merged config (mounts/port-forwards/containerd) doesn't match the template's
  intent, whether adopting a pre-existing VM or verifying a freshly created one.
- **Open-posture IPv6 caveat.** The alias-scoped ACCEPT/DROP rules (§2.2) are only emitted for a
  family whose alias actually resolved; `host.lima.internal` is typically IPv4-only today, so under
  the OPEN posture no v6-specific alias DROP is emitted (there is no resolved v6 alias to drop traffic
  to). Safe today — the CLOSED posture's catch-all DROP covers v6 regardless of alias resolution — but
  if a future lima version or host configuration ever resolves `host.lima.internal` to a *global-scope*
  IPv6 address, the OPEN posture's protection depends on that address actually being picked up as the
  resolved `aliasV6`: the unconditional private-range drops (`fe80::/10`, `fc00::/7`) don't cover a
  global-scope address. `make test-lima-e2e` could assert `getent ahosts host.lima.internal` returns no
  global-scope v6 today, to catch this drifting silently in the future.

The reference-instance trade today (`orbstack`) is a **single shared kernel** across the manager and
all workers ([§8](/security-model/compromise/)); `lima` carries the same trade one level up (its own
kernel is separate from the host, but still one kernel shared within the jail). That trade is a
property of the backend, not of Lever; run `lever backends` for the live matrix.
