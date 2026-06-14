# Security model

Lever's threat model is deliberately harsh: **assume any agent — including the manager — can be
prompt-injected or otherwise turned hostile.** An autonomous agent that processes untrusted content
(web pages, dependencies, issue text, tool output) can be steered into running arbitrary code. The
design goal is not "prevent compromise" but **bound the blast radius of a compromise to a single
directory subtree and a curated set of network endpoints — enforced by the OS, not by the agent
behaving.**

> **Validation status.** The containment *primitives* below were validated by hand on macOS +
> OrbStack (Apple Silicon) — see §6. The *integrated* full-system test (the real manager agent under
> rootless Docker, the project-tree mount's allow-side, the manager's MCP path, and the inner auth
> layer of §4) is **still pending**. Read claims below as "designed and validated in part," not
> "shipped and proven end-to-end."

## 1. The core idea: put the boundary *outside* the runtime

Scion's **broker** (its host-side component that creates containers and applies mounts) is a
*confused deputy*: it asks the **Docker daemon** to bind-mount host paths and to join networks, and
the daemon obliges. Concretely, the escape that motivated this design works because Scion's **hub
performs no path validation** — any caller can register a project provider with an arbitrary host
path, and the broker will then mount it (see §6).

The key realisation: **the real boundary is the Docker daemon the runtime drives, plus the
environment that daemon runs in — not the runtime's code.** Constrain the daemon's filesystem and
network view, and the runtime can ask for anything it likes; it cannot exceed what the environment
physically permits. This is why **no fork of Scion is needed** — containment is enforced by the jail
around it.

## 2. The jail

The entire runtime — Scion server, broker, the container runtime, and every agent — runs inside one
**isolated machine** (an OrbStack isolated Linux guest that shares no host files and has its own
network namespace by default). The `lever` operator binary runs on the host and drives in. The jail
provides two properties:

### 2.1 Filesystem: the secrets are not in the environment

An isolated machine **shares none of the host's files by default.** Lever opts **only the project
tree** in (a single bind mount). Therefore:

- The broker's "bind-mount any host path" power is bounded to the project tree — the only directory
  the jail can see. `~/.ssh`, cloud credentials, the runtime's own host-side state, and the rest of
  the disk **are not in the daemon's filesystem at all**, so a malicious mount of them fails or
  comes up empty.
- This is **removing the secret from the environment**, not fencing an attacker away from a secret
  that is present. No mount-allowlist patch to the runtime is required — the path simply does not
  exist to mount.

### 2.2 Network: the LAN is unreachable; egress is allowlisted

Inside the jail, containers that use host networking share the *jail's* network namespace, not the
host's. Validated behaviour and its limits:

- **The LAN is unreachable** — from inside the jail, other devices on the local network (router,
  NAS, other hosts) cannot be reached; only the host *itself* is, via a runtime alias. **Important
  nuance:** this currently rests on OrbStack's default routing for an isolated machine (it is *not*
  the `--isolate-network` mode, which would also cut the host alias we need for tools). It is **not
  yet** independently enforced by the firewall — so the egress allowlist should also add explicit
  drops for private ranges (`10/8`, `172.16/12`, `192.168/16`, and IPv6 link-local/ULA) as
  defence-in-depth, and LAN-unreachability must be re-checked on OrbStack upgrades.
- **Host loopback services are reachable via the alias** (`host.orb.internal`), which forwards to
  the host's `127.0.0.1` over **both IPv4 and IPv6**. This is how the manager reaches local tool
  servers (e.g. an MCP server) without exposing them on the LAN. The flip side: the alias exposes
  *all* host loopback, so it must be clamped.
- **The egress allowlist** (iptables + ip6tables, both families) permits only the specific tool
  `host:port`s the manager needs and the open internet (for the model API and package installs),
  and **drops all other host-loopback traffic**. ("Drops everything else" means everything else *to
  the host* — open internet egress is intentionally allowed; see the exfiltration caveat in §5.)
- Enforcement lives in the jail's network namespace. A **non-privileged** agent container is in a
  separate namespace from the rules and cannot flush them; reaching the rules would require a
  container/namespace escape (which reduces to the kernel/runtime caveats in §5).

### 2.3 Rootless Docker (required)

Inside an isolated machine, **rootful Docker does not work** and **rootless Docker is required.** An
isolated machine applies a seccomp filter that blocks the `bpf()` syscall, which rootful `runc`
needs to program the cgroup-v2 device controller; containers fail with
`bpf_prog_query(BPF_CGROUP_DEVICE) failed: operation not permitted`. This is the isolation hardening
itself, **not** a missing capability (the machine has full capabilities; a *non-isolated* machine
runs rootful nested Docker fine). Rootless `runc` does not manage device cgroups, so it never calls
`bpf()` and containers run normally. Rootless is also a security **bonus**: an extra user-namespace
layer around every agent. On the OrbStack VM's modern kernel the rootless daemon uses native
`overlayfs` (not the slow userspace `fuse-overlayfs`), so the performance cost is small.

## 3. What containment buys

| Concern | Without the jail | With the jail |
|---|---|---|
| `~/.ssh`, cloud creds, host `$HOME` | mountable by a compromised agent via the broker | **not in the environment** — nothing to mount |
| runtime's own host-side secrets | reachable via mount escape | absent from the jail |
| arbitrary host path bind-mount | accepted by the runtime (hub does no path validation) | bounded to the project tree |
| host LAN / business network | full reach via host networking | **unreachable** (OrbStack routing; firewall to back it) |
| host loopback (local tools) | n/a | only allowlisted `host:port`s; rest dropped |
| exfiltration of in-tree data | n/a | **not bounded** — see §5 |

**Result:** an injected manager or grove can reach neither host secrets nor the LAN; its blast
radius is the project subtree it was given — *plus* whatever it can send over allowed internet
egress (§5).

## 4. A second, inner layer (defence in depth)

The jail protects host secrets and the LAN. *Within* the tree, Scion's **development auth** (a
built-in mode that issues every agent the same broker token — convenient for local dev, but it lets
any agent drive the broker and reach any project under the hub) means a hijacked grove could ask
the broker to mount a **sibling grove or the knowledge base**. Both live inside the project tree —
not a host-secret leak, but a real **cross-grove / KB leak**.

**Be explicit: until the inner layer ships, a compromised grove can read and write the entire
project tree, including the knowledge base and sibling groves.** The "manager reads groves but
doesn't write them" convention is *not* a security control. Closing this means turning off
development auth and giving the manager a real credential while groves get only project-scoped
tokens. It is **defence in depth, scheduled after the jail** — the system is already bounded against
the primary threats (host secrets, LAN) without it, but cross-grove isolation is not yet enforced.

## 5. What this model does *not* claim

- **Data-exfiltration protection.** The bound is on host-secret and LAN reach, **not** on
  exfiltrating in-tree data. A compromised agent with read access to the project tree and open
  internet egress (allowed, for the model API and package installs) can leak anything in the tree —
  the model-API endpoint alone is a covert channel. Tightening this would require an egress proxy /
  data-loss controls not yet specified.
- **Containing daemon authority inside the jail.** The architecture depends on the broker driving a
  Docker daemon. Any agent that holds the rootless Docker socket — or, like the manager, has hub
  authority to drive the broker — can launch arbitrary (including `--privileged`) containers *within
  the jail*. Containment of that rests entirely on the jail's filesystem/network bound and the
  kernel, not on denying daemon access.
- **A separate kernel.** An isolated machine shares the host VM's kernel, and the manager and all
  groves share that one kernel *with each other*. So a kernel-level escape from any single agent
  defeats inter-agent isolation wholesale and reaches the VM — not merely host-secret protection.
  For a hypervisor-hard kernel boundary, a dedicated VM (e.g. Lima/Colima) is the stronger
  substrate; the isolated machine is judged sufficient for a single-operator workstation under this
  threat model. This is an explicit, documented trade.
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

## 6. Validation evidence

Validated by hand on macOS + OrbStack (Apple Silicon). What was demonstrated:

- **The escape exists without the jail.** From inside an ordinary agent container (no host
  filesystem, no Docker socket), using only the development-auth token every agent is given, the hub
  accepted a new project + a **provider with an arbitrary host path (no path validation)**; starting
  an agent then drove the broker to bind-mount a sensitive host directory into a new container,
  whose files were readable. This is the threat the jail closes.
- **Filesystem containment holds with the jail.** An isolated machine has no host home; the host
  secret directories are absent, so the same escape mounts nothing.
- **LAN unreachable; host loopback reachable and clampable.** From inside the jail the LAN router
  was unreachable (100% packet loss) while a host loopback tool server answered via the alias (over
  both IPv4 and IPv6); an egress allowlist then permitted one tool port and dropped the rest — and
  the rule still held for a rootless `--network=host` container (the topology agents actually use).
- **The runtime runs inside the jail.** Rootless Docker runs containers (native `overlayfs`); the
  Scion binary builds and runs; a rootless host-networked container obeys the egress allowlist.

What is **not** yet validated (pending the full-system test): the project-tree mount's *allow* side
(that exactly the chosen tree is present and nothing else), the real manager Claude agent under
rootless Docker, the manager's MCP reachability in practice, and the §4 inner auth layer.

> Validation was performed by checking reachability and file presence by size/permission, never by
> printing secret contents.

## 7. Reporting a vulnerability

Pre-release; a security contact will be published with the first release. If you find a containment
hole in the meantime, please open a minimal-detail issue and request a private channel.
