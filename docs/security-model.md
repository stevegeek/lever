# Security model

Lever's threat model is deliberately harsh: **assume any agent — including the manager — can be
prompt-injected or otherwise turned hostile.** An autonomous agent that processes untrusted content
(web pages, dependencies, issue text, tool output) can be steered into running arbitrary code. The
design goal is not "prevent compromise" but **bound the blast radius of a compromise to a single
directory subtree and a curated set of network endpoints — enforced by the OS, not by the agent
behaving.**

> **Validation status.** The containment *primitives* below were validated by hand on macOS +
> OrbStack (Apple Silicon) — see §8. The *integrated* full-system test (the real manager agent under
> rootless Docker, the project-tree mount's allow-side, the manager's MCP path, and the inner auth
> layer of §4) is **still pending**. Read claims below as "designed and validated in part," not
> "shipped and proven end-to-end."

## 1. The core idea: put the boundary *outside* the runtime

Scion's **broker** (its host-side component that creates containers and applies mounts) is a
*confused deputy*: it asks the **Docker daemon** to bind-mount host paths and to join networks, and
the daemon obliges. Concretely, the escape that motivated this design works because Scion's **hub
performs no path validation** — any caller can register a project provider with an arbitrary host
path, and the broker will then mount it (see §8).

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
  the host* — open internet egress is intentionally allowed; see the exfiltration caveat in §7.)
- Enforcement lives in the jail's network namespace. A **non-privileged** agent container is in a
  separate namespace from the rules and cannot flush them; reaching the rules would require a
  container/namespace escape (which reduces to the kernel/runtime caveats in §7).

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
| exfiltration of in-tree data | n/a | **not bounded** — see §7 |

**Result:** an injected manager or grove can reach neither host secrets nor the LAN; its blast
radius is the project subtree it was given — *plus* whatever it can send over allowed internet
egress (§7).

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

## 5. The operator boundary: the config is host-side code, kept out of the mount

Sections 1–4 harden the *inside* of the jail against hostile agents. But the host-side operator
binary (`lever`) reads its config and acts on it **before the jail exists**: which host directory to
bind-mount, which host file to read as a credential, which image to run, what text to inject as the
manager's task. The config is, in effect, host-side code — so two things matter: **where it comes
from** (can an attacker substitute it?) and **whether the agents it constrains can rewrite it**.

Both are now structurally closed:

### 5.1 The config never enters the mount (no in-jail tamper → host escalation)

The instance **root** holds `lever.yaml` and the boot prompt and is **not** mounted; only a `tree:`
**subdirectory** is bind-mounted. `tree` is validated as a confined relative subdir — not `.` (the
root itself), not absolute, no `..` — so it cannot be widened to `/`, `$HOME`, or the root. The boot
prompt is resolved at the root too (`ManagerPromptPath` joins the root, not the tree) and confined
there.

Why this matters: if the config or prompt lived *inside* the mount (the natural "config at the
project root, root == mount" layout), a compromised manager or grove could **rewrite the config the
host trusts on the next `lever apply`** — an in-jail-compromise → host-escalation persistence
channel (point it at `credential_file: ~/.ssh/id_rsa`, `tree: /`, an attacker image, etc.). Keeping
the root unmounted removes that channel: agents can't see or edit what the host re-reads.

### 5.2 No walk-up discovery (no planted-parent config)

Config is resolved from the **current directory only** — there is deliberately **no walk-up**. A
`lever.yaml` planted in a parent directory of wherever you happen to `cd` can never be picked up and
trusted. Run `lever` from the instance root, or pass an explicit (trusted) path.

### 5.3 Field validation (defence in depth, even for a trusted config)

`config.Validate()` and the credential read now enforce:

| Field | Check |
|---|---|
| `name`, grove `name` | `^[a-z0-9][a-z0-9-]{0,62}$` (it becomes the jail machine name and a shell token). |
| `tree` | confined relative subdir (not `.`/absolute/`..`). |
| `manager.prompt_file` | confined relative path under the root (no `..`, not absolute). |
| `manager.image`, grove `image` | safe OCI-ref charset; plus **opt-in** `security.allowed_image_registries` (run only images from trusted registries/namespaces) and `security.require_image_digest` (require `@sha256:`-pinned images, no mutable tags). |
| `credential_file` | read with a **permission check** (rejected if world-readable) and a **size cap** — defence in depth for the secret it becomes (§6). |
| grove `dir` | already rejected absolute/`..` (unchanged). |

**What was already sound:** the execution plumbing is argv-clean — no shell injection in the hot
paths; the single `bash -c` (scion install) correctly single-quote-escapes its interpolated values;
`jailPath` never fabricates an in-jail path for an out-of-tree target; the credential value is
base64'd and redacted in error output at its one call site.

### 5.4 The in-jail manifest is sanitized

The in-jail manager no longer reads the operator config (which would require it in the mount and
would expose host paths). Instead the host writes a **sanitized runtime manifest**
(`.lever-manifest.yaml`, grove→resolved-image only — no host paths, no credential, no tree/ports)
into the mount at apply. A compromised manager rewriting it can at most re-select an
already-loaded image for a grove it dispatches — no host escalation, no secret leak.

### 5.5 Residual

Image **registry allowlist** and **digest pinning** are now available as opt-in `security:` policy
(§5.3) — enable them to bound *which* registry an image comes from and to require vetted, immutable
images. Still open: redaction by secret-key-name rather than argv shape (L1 in the backlog). And the
dominant in-jail risks remain §6 (the projected credential) and §7 (open-egress exfiltration),
addressed by the capability-broker roadmap.

## 6. Credential blast radius, and the path to capabilities

**Finding.** The manager sets the upstream credential (`CLAUDE_CODE_OAUTH_TOKEN`) as a Hub secret;
scion **resolves and injects it into every agent container's environment** at start (user/owner
scope, single jail = single tenant). So **every grove holds the real, long-lived OAuth token** in
`$CLAUDE_CODE_OAUTH_TOKEN` (or a token file in its home). Combined with §4 (development auth lets any
in-jail agent drive the broker) and §7 (open internet egress for the model API), a single
prompt-injected grove can read the token and exfiltrate it — **impersonating the operator's account
beyond the jail.** The token is *ambient, shared, and long-lived* — the worst combination, and the
highest-value secret in the system.

This is the strongest argument for replacing **pushing keys to agents** with **agents exchanging
identity for narrow capabilities.**

### Target design: a host-side capability broker (macaroons)

- **Today:** the raw upstream credential is pushed into every agent. Any holder has full authority —
  no attenuation, no expiry, no audience binding.
- **Target:** a **capability broker on the host** (outside the jail) holds the raw credential and
  never projects it into a container. Agents present their identity and **exchange it for an
  attenuated, caveated, short-TTL capability** (a [macaroon](https://research.google/pubs/pub41892/)
  or equivalent) scoped to exactly what they need — bound by caveats to **audience** (the specific
  upstream/model endpoint), **scope** (this project / read-only / these tools), and **expiry**. An
  agent uses the capability and it expires; compromising one grove leaks at most a narrow,
  short-lived token, not the master credential.
- **Placement rides the existing fence:** the broker is reachable only via the allowlisted host
  alias (it becomes one of the `allow_ports` endpoints), so the egress allowlist (§2.2) already gates
  which containers can reach it. The raw credential stays on the host.
- **Macaroon caveats** encode the attenuation: the broker mints a broad (but still caveated)
  capability for the manager, and the manager (or broker) further attenuates per grove — a grove's
  token is strictly weaker than the manager's, by construction.

### Migration path

1. **Pull, don't push** — move the credential out of per-agent env into the broker the agents call.
2. **Coarse scoping first** — issue per-project tokens (this is the §4 "groves get project-scoped
   tokens" step), retiring development auth so the broker becomes the single token authority.
3. **Caveats for least privilege** — add macaroon caveats (expiry, audience, method/scope) for true
   least-privilege, attenuated per grove.
4. **Pair with an egress proxy** — close the §7 exfiltration gap so even a leaked capability can only
   be used against approved endpoints, not arbitrary internet.

This subsumes the §4 dev-auth weakness (one authority issuing scoped tokens replaces "every agent
gets the same broker token") and directly bounds the credential blast radius identified above.

### 6.1 Built state (api-key mode) and the mixed-instance residual

The capability broker is now built: an agent in **`llm_auth: api-key`** mode holds only a
short-lived, CN-bound `capability(llm)` biscuit and routes the model through the broker `/llm` proxy,
which strips the biscuit and injects the real Console key host-side. Such an agent **never receives a
real Anthropic credential**, and in a **uniformly api-key** instance the jail also runs closed-internet
egress (§2.2) — so the §6 blast radius is closed for that instance.

**Mixed instances are unsupported — rejected at config validation.** The OAuth credential is set as a
single Hub *secret* (`internal/scion/bringup.go:SecretSet`, user/owner scope), gated only by
`manager.credential_file` and **projected into every container regardless of its `llm_auth` mode**;
and egress is applied **jail-wide, not per-container**. So in a *mixed* instance (any subscription
agent ⇒ `credential_file` set ⇒ token hub-projected; and egress forced open) an `api-key` grove would
*also* hold `$CLAUDE_CODE_OAUTH_TOKEN` and have open internet egress, letting it read the real token
and reach `api.anthropic.com` directly, bypassing the proxy's capability gating — its key isolation
would silently not hold.

Rather than ship that footgun, **an instance must be uniformly api-key OR uniformly subscription**:
`App.Validate` (`internal/config/config.go:validateBroker`) rejects any config whose *effective*
agent modes mix the two (`mixedLLMAuth`), so a mixed instance never reaches apply. The two pure cases
are both clean: **all-subscription** = every agent holds the OAuth key, open egress (the owner/dev
trade, by design); **all-api-key** = no agent holds a real key, capabilities gated by biscuits, egress
closed jail-wide. Note this is *not* an escalation surface: the `api-key` flag controls only whether an
agent obtains a capability token, **not** credential availability, so flipping a grove's
(manager-writable) manifest mode could never *conjure* a token the host did not project; the validation
gate is about preventing a misleading config, not about containment.

To *support* mixed instances later would require per-container egress and/or projecting the OAuth secret
only into subscription agents' projects (not as a hub-wide secret) — deferred; until then, mixed is a
hard config error.

## 7. What this model does *not* claim

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

## 8. Validation evidence

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

## 9. Reporting a vulnerability

Pre-release; a security contact will be published with the first release. If you find a containment
hole in the meantime, please open a minimal-detail issue and request a private channel.
