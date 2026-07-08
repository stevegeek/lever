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

> **Validation status.** *Shipped and validated:* the containment primitives (§9); the capability
> broker, mTLS enrolment, CN-bound capability minting, the six-check `lever acceptance` gate,
> and the **api-key `/llm` strip-and-inject path end-to-end** (broker verifies the capability token,
> strips it, injects the real Console key host-side), guarded by `make test-apikey-e2e`; container boot
> enrols the agent and registers the broker tools over mTLS; the **single-project model** (§4) — one
> Scion project per instance, the real hub running dev-auth off, lifecycle driven only by a host-only
> controller PAT, and worker↔worker isolation by defense-by-absence — is implemented and merged.
> *Still pending:* the full in-container claude driving a first-party tool
> (`/mcp/db/`) end-to-end, mid-session token-refresh pickup (the agent reads `ANTHROPIC_AUTH_TOKEN`
> once at startup, the 12h renew sidecar runs, but a running session's pickup of a rotated token is
> unverified), and a **dedicated live acceptance gate for the single-project isolation guarantee**:
> the mechanism (§4) is merged and the required upstream Scion fixes are present in the pinned
> commit, but the checks that would exercise it against a real `scion start` (sibling subdirectories,
> a stray ancestor `.git`, the controller PAT's exact scopes) are not yet wired into
> `lever acceptance`.

## 1. The core idea: put the boundary *outside* the runtime

Scion's **broker** (its host-side component that creates containers and applies mounts) is a
*confused deputy*: it asks the **Docker daemon** to bind-mount host paths and to join networks, and
the daemon obliges. Concretely, the escape that motivated this design works because Scion's **hub
performs no path validation**, any caller can register a project provider with an arbitrary host
path, and the broker will then mount it (see §9).

The key realisation: **the real boundary is the Docker daemon the runtime drives, plus the
environment that daemon runs in, not the runtime's code.** Constrain the daemon's filesystem and
network view, and the runtime can ask for anything it likes; it cannot exceed what the environment
physically permits. This is why **no fork of Scion is needed**, containment is enforced by the jail
around it.

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
  that is present. No mount-allowlist patch to the runtime is required, the path simply does not
  exist to mount.

### 2.2 Network: the LAN is unreachable; egress is allowlisted

Inside the jail, containers that use host networking share the *jail's* network namespace, not the
host's. Validated behaviour and its limits:

- **The LAN is unreachable**, from inside the jail, other devices on the local network (router,
  NAS, other hosts) cannot be reached; only the host *itself* is, via a runtime alias. This rests on
  OrbStack's default routing for an isolated machine (it is *not* the `--isolate-network` mode, which
  would also cut the host alias we need for tools), **backed by explicit firewall drops** so it does
  not depend on OrbStack routing alone: the allowlist drops the private/special-use ranges
  unconditionally, `10/8`, `172.16/12`, `192.168/16`, **`169.254/16` (link-local/metadata)**,
  **`100.64/10` (CGNAT/Tailscale)**, and IPv6 `fe80::/10` + `fc00::/7`
  (`internal/egress/allowlist.go`). LAN-unreachability must still be re-checked on OrbStack upgrades.
- **Host loopback services are reachable via the alias** (`host.orb.internal`), which forwards to
  the host's `127.0.0.1` over **both IPv4 and IPv6**. This is how the manager reaches local tool
  servers (e.g. the broker, an MCP server) without exposing them on the LAN. The flip side: the alias
  exposes *all* host loopback, so it must be clamped.

**Network egress: two postures.** All lever rules live in a dedicated **`LEVER_EGRESS`** iptables/
ip6tables chain that `OUTPUT` jumps to (never direct `OUTPUT` rules), so re-apply flushes and rebuilds
only that chain (idempotent, and it never touches non-lever rules). The posture is chosen by the
explicit, jail-wide **`egress:`** knob, **independent of `llm_auth`**:

- **`egress: open` (default):** `OUTPUT` default-ACCEPT; `LEVER_EGRESS` ACCEPTs the allowlisted host
  `host:port`s on the alias, DROPs the rest of the host alias and the private ranges above. **Public
  internet stays open** (for the model API and package installs). See the exfiltration caveat in §8.
- **`egress: closed`:** additionally a **catch-all DROP** for both families at the end of the chain,
  with loopback (`-o lo`) ACCEPTed *first* so the in-machine scion hub (127.0.0.1:8080) and host-alias
  tools keep working. The jail can then reach **only** the already-ACCEPTed broker port; arbitrary
  public-internet egress is closed. **`closed` is valid only for a uniformly api-key instance** (a
  subscription agent needs direct internet to reach Anthropic), enforced at config load.
- **Reachability under closed egress.** With the catch-all DROP, DNS/53 is dropped too, so an agent
  cannot resolve `host.orb.internal`. Agents therefore dial the broker by its **resolved alias IP**
  (already allowlisted), and the broker mints its server cert with that **IP as a SAN** so TLS still
  validates (`internal/cap/ca/issue.go IssueServerCertSANs`, wired in `internal/brokerctl/serve.go`
  and `internal/cli/apply.go`). Re-applying a *live* closed instance detects the active catch-all DROP
  and skips the flush/rebuild, so egress is never momentarily reopened under a running agent.

**Enforcement** lives in the jail's network namespace, for both postures. A **non-privileged** agent
container is in a separate namespace from the rules and cannot flush them; reaching the rules would
require a container/namespace escape (which reduces to the kernel/runtime caveats in §8).

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

### 2.4 Substrates: the jail is a contract, not a product

Everything above describes OrbStack, but the jail is a **contract**, not that one product. A
containment backend must provide five things. Guarantee 0 was added 2026-07-02, after rejecting a
native-Linux backend on exactly this ground (see below):

0. **A hypervisor boundary between the agent workload and the host kernel.** Mandatory: agents run
   arbitrary, potentially adversarial code (§1), and a network/mount/user namespace is not a
   substitute for a separate kernel — one kernel-level exploit from any agent reaches everything
   sharing that kernel. No backend without a VM boundary is added to `lever backends`.
1. **No host filesystem** beyond the one chosen project tree (§2.1).
2. **A network namespace Lever controls**, so the egress allowlist can be enforced *outside* the agent
   containers (§2.2).
3. **Egress enforced in that namespace**, not by the agent behaving.
4. **A host-reachable broker endpoint** for capability, LLM, and tool traffic.

OrbStack is the **reference implementation** of that contract; `lima` is the second, both are
**implemented**. Each **declares** what it actually guarantees rather than pretending they are
equivalent — a `Profile` in code, surfaced by `lever backends`:

| Backend | Kernel boundary | FS bounded by | Egress enforced at |
|---|---|---|---|
| **orbstack** | shared jail-VM kernel | isolated machine: no host files + project tree mounted at `/lever` | jail netns iptables/ip6tables |
| **lima** | own VM kernel | VM: no host files + project tree mounted at `/lever` | jail netns iptables/ip6tables |

Full mechanism detail, the roadmap entry, and rejected backends live on the
[containment backends](/reference/backends/) page; the highlights:

- **`lima`'s containment surface is a lever-owned template**, not a stock one. Besides the VM kernel
  boundary and the single project-tree mount, it closes a hazard specific to Lima: by default Lima
  forwards *every* guest listener to the host's `127.0.0.1` (live-confirmed 2026-07-02: on a stock
  template, a guest-side `0.0.0.0` listener is reachable at the host's loopback), which would let a
  jailed agent squat a host-loopback port and impersonate a local service (a dev server, a
  credential helper). The template's `portForwards` block carries an ignore-all rule, closing this
  before the guest VM ever exists.
- **Docker Desktop is *not* a backend.** The jail is a VM-isolation construct, not a container
  runtime. Docker Desktop's VM is *shared* infrastructure — it auto-mounts your home directory and
  its network namespace is not yours to control — so it satisfies neither guarantee 1 nor 2. Running
  Lever's containers directly in it would appear to work while silently voiding the containment.
- **A native, no-VM Linux backend (`linux-docker`) was explored and rejected**, the opposite failure
  from Docker Desktop's: its namespace-based design (root-owned netns, mount-namespace + `tmpfs`
  shadowing) would have satisfied guarantees 1-3, but shares the host kernel outright, violating the
  new guarantee 0 — an adversarial agent (§1) is one kernel-level privilege escalation from host
  root. Its egress and filesystem answers were sound and are recorded on the backends page for the
  record; a weaker, explicit-opt-in variant for hosts with no nested-virtualization support (a bare
  VPS) remains a possible future direction, never a silent substitute for a VM backend.
- **`apple-container` (roadmap) is a different topology.** It runs each agent in its own micro-VM (a
  kernel per agent — the strongest isolation of any Mac option, satisfying guarantee 0 *per agent*
  rather than per jail, turning the shared-kernel trade in §8 into a non-issue), but there is no
  single jail to hang one egress chokepoint on, and its networking is young (full support needs
  macOS 26).

This is exactly why config validation rejects any backend name that isn't implemented instead of
quietly falling back to OrbStack: a containment posture must never be silently substituted.

**Lima operational notes**, from the T13 security review:

- **Lima's in-guest kernel attack surface is intentionally widened for rootless runtimes.**
  Provisioning re-enables the unprivileged user-namespace knob
  (`kernel.apparmor_restrict_unprivileged_userns=0`) that Ubuntu ≥ 23.10 disables by default, a
  prerequisite for rootless Docker/Podman's rootlesskit/pasta. This widens attack surface *inside the
  guest kernel* only, in exchange for the rootless containers the whole containment model depends on;
  the boundary that actually matters — the hypervisor (guarantee 0) — is untouched. An escalation via
  this surface reaches VM root, not the host, consistent with the §8 "containing daemon authority
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
  caveats in §8.
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
all workers — see §8; `lima` carries the same trade one level up (its own kernel is separate from the
host, but still one kernel shared *within* the jail by the manager and every worker). That trade is a
property of the *backend*, not of Lever, and the table above is how you see it before you choose.
Run `lever backends` for the live matrix; set the backend with the `backend:` key
([config reference](/reference/config/)).

## 3. What containment buys

| Concern | Without the jail | With the jail |
|---|---|---|
| `~/.ssh`, cloud creds, host `$HOME` | mountable by a compromised agent via the broker | **not in the environment**, nothing to mount |
| runtime's own host-side secrets | reachable via mount escape | absent from the jail |
| arbitrary host path bind-mount | accepted by the runtime (hub does no path validation) | bounded to the project tree |
| host LAN / business network | full reach via host networking | **unreachable** (OrbStack routing; firewall to back it) |
| host loopback (local tools) | n/a | only allowlisted `host:port`s; rest dropped |
| real LLM credential in every container | ambient, shared, long-lived OAuth token in every agent | **api-key mode:** no real key in any container, only a CN-bound, short-lived `capability(llm)` token; the broker injects the Console key host-side (§6.1) |
| exfiltration of in-tree data | n/a | **not bounded** in subscription mode; narrowed (not eliminated) under api-key closed egress, see §8 |

**Result:** an injected manager or worker can reach neither host secrets nor the LAN; its blast
radius is the project subtree it was given, *plus* whatever it can send over allowed internet
egress (§8).

## 4. Within the jail: cross-worker isolation (defence in depth)

The jail (§2-§3) protects host secrets and the LAN. *Within* the jail, the manager and every worker
are agents in the **same Scion project** — Lever registers exactly one project per instance
([architecture.md §2](/architecture/)), not a separate project per agent as an earlier design did.
Two structural properties bound what one agent can reach inside that shared project.

### 4.1 Defense by absence: a worker only ever mounts its own subdirectory

Each agent's container bind-mounts **only its own configured workspace path**. The manager's
container mounts the whole tree root; each worker's container mounts exactly the one subdirectory
declared for it in config, in place. Scion never enumerates sibling agents to build deny/shadow
mounts, so a sibling worker's subdirectory is simply **not a mount source** for a given worker's
container: it is unreadable at the kernel/VM boundary, not merely hidden by convention or file
permission (container UIDs are synced to the host UID, so file permissions alone give no
inter-agent isolation here).

This guarantee holds only on a **non-git tree root**: a git repository at the tree root can pull
Scion's mount builder into a worktree branch that also bind-mounts the whole `.git` object store,
through which a worker could read *committed* sibling content. Two things close this: config
validation refuses (or warns on) a git tree root at load time, and the pinned Scion always
plain-mounts an explicit `--workspace` regardless of a stray ancestor `.git` (the upstream
`--workspace` git-guard fix; the current `scion.version` pin carries it). A worker's *own*
subdirectory may still contain its own git repository, that is unaffected.

**The manager still sees everything, by design.** Because the manager's mount is the whole tree,
and Scion does not shadow child workspace dirs inside a broader mount, the manager's live view
legitimately includes every worker's in-place edits — the same "mount only your own workspace"
mechanism, viewed at the manager's wider scope. A compromised *manager* therefore still has
whole-tree reach (§7); this isolation guarantee is about one *worker* reaching another worker's
subdirectory, not about bounding the manager.

### 4.2 No agent holds hub authority: dev-auth off, host-only controller PAT

Cross-worker mount isolation would still be moot if a compromised agent could simply ask Scion's
hub to attach an arbitrary mount, or start a new agent, itself. Scion's **development auth** mode
(a built-in convenience that issues a shared, admin-equivalent bearer token to any caller) would let
it do exactly that. Lever closes this: the real, long-lived Scion hub inside the jail runs with
**`--dev-auth=false`** — no agent, manager or worker, is ever handed a hub credential.

Instead, every Scion lifecycle call (start/stop/suspend/resume/message — issued by the host-side
capability broker on the manager's behalf, and by `lever` itself for attach/msg/stop) is
authenticated with a **controller PAT**: a Scion hub token scoped to exactly
`agent:manage,agent:attach,project:read` (`agent:attach` is load-bearing — the `agent:manage` alias
alone 403s on `start`, since scion gates every interactive verb, including `start`, on
`agent:attach`). It is:

- **Minted through a throwaway, jail-local hub.** Before any agent container exists, bring-up starts
  a temporary `scion server --dev-auth=true` on a fixed private port (48080) no agent ever learns, initializes the
  instance's single project against it, mints the PAT, then kills that throwaway server (removing
  the dev-auth token file it left behind) and starts the real `--dev-auth=false` hub agents actually
  run against.
- **Persisted host-side only**, `0600`, under `.lever-state/` — never written into the mounted
  tree, never set as a container environment variable or Scion hub secret, so there is no path by
  which an agent inside the jail can read it.
- **Re-minted, not blindly reused, across restarts.** The PAT persists across `stop`→`up`; if the
  hub rejects it, lever re-runs the agent-free throwaway-hub window rather than ever re-enabling
  dev-auth on a hub that already has agents.
- **Injected only into lever's own host-side Scion client calls**, as the `SCION_HUB_TOKEN`
  environment variable, by the capability broker and by `lever attach`/`lever msg`/`lever stop`.

The result: even a fully compromised worker or manager container has no credential that lets it
talk to the Scion hub directly, it cannot register a project, request an arbitrary mount, or
list/attach to another agent. All of that is host-side-only, gated by the controller PAT, and, for
dispatch specifically, further gated by the config-declared subdirectory per §5.4.

**Residual.** This closes the isolation gap between workers, and between an agent and the hub
itself. It does not change the manager's own trust position: the manager legitimately mounts the
whole tree (§4.1), so a compromised manager can still read and write everything the instance keeps
there, including the knowledge base and every worker's subdirectory, that is an inherent cost of
giving the manager whole-tree oversight (§7), not a gap in the worker-isolation model above. **Not
yet done:** the live acceptance checks that would exercise this guarantee against a real
`scion start` (sibling subdirectories, a stray ancestor `.git`, the controller PAT's exact scopes)
are not yet wired into `lever acceptance`, the mechanism is merged and the required Scion fixes are
present in the pinned commit, but the dedicated live gate for it does not exist today.

## 5. The operator boundary: the config is host-side code, kept out of the mount

Sections 1-4 harden the *inside* of the jail against hostile agents. But the host-side operator
binary (`lever`) reads its config and acts on it **before the jail exists**: which host directory to
bind-mount, which host file to read as a credential, which image to run, what text to inject as the
manager's task. The config is, in effect, host-side code, so two things matter: **where it comes
from** (can an attacker substitute it?) and **whether the agents it constrains can rewrite it**.

Both are now structurally closed:

### 5.1 The config never enters the mount (no in-jail tamper → host escalation)

The instance **root** holds `lever.yaml` and the boot prompt and is **not** mounted; only a `tree:`
**subdirectory** is bind-mounted. `tree` is validated as a confined relative subdir, not `.` (the
root itself), not absolute, no `..`, so it cannot be widened to `/`, `$HOME`, or the root. The boot
prompt is resolved at the root too (`ManagerPromptPath` joins the root, not the tree) and confined
there.

Why this matters: if the config or prompt lived *inside* the mount (the natural "config at the
project root, root == mount" layout), a compromised manager or worker could **rewrite the config the
host trusts on the next `lever apply`**, an in-jail-compromise → host-escalation persistence
channel (point it at `credential_file: ~/.ssh/id_rsa`, `tree: /`, an attacker image, etc.). Keeping
the root unmounted removes that channel: agents can't see or edit what the host re-reads.

**The boundary this draws — and what deliberately sits on the other side of it.** Operator-owned
boot material (`lever.yaml`, `prompt_file`) is host-side and tamper-proof from inside the jail.
Tree-resident material — `CLAUDE.md`, the skills scaffolded by `lever init`, the working files
themselves — is *inside the mount* and therefore inside the agent's own blast radius: a
compromised agent can rewrite its own `CLAUDE.md`, and Claude Code will auto-load the modified
version in every later session. This is not a containment failure (the file is within what the
agent already controls, and CLAUDE.md-loading is how the harness works — an agent could always
create one), but it has one consequence worth knowing: **tree-resident instructions persist across
restarts**. `lever up --fresh` gives you a fresh conversation, not a fresh tree — if you suspect
an agent was prompt-injected, audit what it wrote to the tree (CLAUDE.md included), don't just
restart the thread. When you need boot instructions an agent cannot alter, `prompt_file` is the
mechanism; `lever init`'s scaffolds are working material, not a trust anchor.

### 5.2 No walk-up discovery (no planted-parent config)

Config is resolved from the **current directory only**, there is deliberately **no walk-up**. A
`lever.yaml` planted in a parent directory of wherever you happen to `cd` can never be picked up and
trusted. Run `lever` from the instance root, or pass an explicit (trusted) path.

### 5.3 Field validation (defence in depth, even for a trusted config)

`config.Validate()` and the credential read now enforce:

| Field | Check |
|---|---|
| `name`, worker `name` | `^[a-z0-9][a-z0-9-]{0,62}$` (it becomes the jail machine name and a shell token). |
| `tree` | confined relative subdir (not `.`/absolute/`..`); also rejected (or warned on) if it is itself a git repository, see §4.1. |
| `manager.prompt_file` | confined relative path under the root (no `..`, not absolute). |
| `manager.image`, worker `image` | safe OCI-ref charset; plus **opt-in** `security.allowed_image_registries` (run only images from trusted registries/namespaces) and `security.require_image_digest` (require `@sha256:`-pinned images, no mutable tags). |
| `credential_file` | read with a **permission check** (rejected if world-readable) and a **size cap**, defence in depth for the secret it becomes (§6). |
| worker `dir` | already rejected absolute/`..` (unchanged). |

**What was already sound:** the execution plumbing is argv-clean, no shell injection in the hot
paths; the single `bash -c` (scion install) correctly single-quote-escapes its interpolated values;
`jailPath` never fabricates an in-jail path for an out-of-tree target; the credential value is
base64'd and redacted in error output at its one call site.

### 5.4 The manager holds no worker-dispatch authority

Worker lifecycle is owned by the host-side capability broker, not the in-jail manager, and the
broker itself is the only holder of the controller PAT (§4.2) — the manager has no Scion hub
credential of its own, in-jail or otherwise. The manager's `agent start/stop/suspend/resume`
commands are thin mTLS clients of the broker's `/worker/*` endpoints. Each request is authenticated
by the manager's certificate CN and authorized against the config: only a worker **declared in the
config** can be dispatched, and the manager passes a worker **name**, never a filesystem path — the
broker resolves the subdirectory, image, and LLM-auth mode from the config host-side, within the
one instance project (there is no separate per-worker project to mount instead). A compromised
manager therefore cannot start an agent against an arbitrary path, widen a worker's mount beyond
its declared subdirectory, or inject a host path; the worst it can do is (re)dispatch a worker it
was already permitted to dispatch. Because the broker (not the mount) is the source of worker
configuration, there is no in-jail config file for a compromised manager to tamper with.

### 5.5 Residual

Image **registry allowlist** and **digest pinning** are now available as opt-in `security:` policy
(§5.3), enable them to bound *which* registry an image comes from and to require vetted, immutable
images. Still open: redaction by secret-key-name rather than argv shape (L1 in the backlog). The
dominant in-jail risks are §6 (the projected credential) and §8 (open-egress exfiltration): **closed
in api-key mode** (the default) by the built capability broker (§6.1) plus `egress: closed`, and
still present under the subscription opt-in.

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
`$CLAUDE_CODE_OAUTH_TOKEN` (or a token file in its home), this is a **subscription-mode** exposure
only (§6.1). Combined with §8 (open internet egress for the model API), a single prompt-injected
worker can read the token and exfiltrate it, **impersonating the operator's account beyond the
jail.** (§4's dev-auth-off/controller-PAT hardening closes the *separate* risk of a compromised
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
then seals outbound network to the broker alone (§2.2), closing the §6 blast radius entirely for that
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
= every agent holds the OAuth key with open egress (the owner/dev trade, by design). Note this is
*not* an escalation surface: the `api-key` flag controls only whether an agent obtains a capability
token, **not** credential availability, and a worker's mode is fixed by the config the broker reads
(nothing the manager can rewrite), so it could never *conjure* a token the host did not project; the
validation gate is about preventing a misleading config, not about containment.

To *support* mixed instances later would require per-container egress and/or projecting the OAuth secret
only into subscription agents' projects (not as a hub-wide secret), deferred; until then, mixed is a
hard config error.

### 6.2 What the broker enforces (built lock-downs)

These are the shipped, code-enforced properties of the capability layer, the *how* behind §6.1:

- **Capabilities are mTLS-CN-bound and non-transferable.** Every token carries an intrinsic check
  `caller == bound_agent` (`internal/cap/token/token.go`); the broker authenticates the caller from its
  verified client-cert CN and fails closed without one (`ca.RequireAgent`, called first on `/request`,
  `/llm`, and the gateway). A stolen token is useless without that agent's in-container private key,
  which is generated in the container and never leaves it.
- **Per-call epoch + revocation is the real cut, not the TTL.** A generous 24h grant TTL is only a
  backstop; on **every** call the broker re-checks the live revocation set and `MinEpoch`
  (`/revoke` and `/bump-epoch`, persisted across restarts and seeded at construction). Revoking an
  agent or bumping the epoch denies its outstanding tokens immediately, this is verified for both the
  `/llm` proxy and the first-party tool gateway.
- **Constraints can only narrow, and only the broker mints.** `/request` validates each requested
  constraint against the tool's `AllowedValues` (fail closed) and bakes it into the signed token as an
  equality check on that request parameter (`internal/cap/token`), so the minted capability is usable
  only for that exact value. The broker is the sole minter, there is no offline way to widen a token,
  and any tampered token fails Ed25519 signature verification.
- **Forged identity headers are scrubbed.** The gateway deletes every inbound `X-Lever-*` header before
  processing (`internal/broker/gateway.go`), then sets `X-Lever-Caller` itself from the verified CN, a
  jail agent cannot forge broker-internal context.
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

**External MCP servers (broker-fronted).** A `broker.tools` entry with `external: true` is a
host server the broker *fronts but does not spawn*: it registers from config, is gated at the
mTLS gateway like any third-party tool (token verified, then **stripped** — the server never
sees a capability), and replaces the old ambient pattern (per-server `allow_ports` holes + a
`.mcp.json` pointing at the host alias). Three honest boundaries:

- **`gate: coarse` is wholesale trust.** One wildcard capability (`{tool,"*"}`) admits every
  MCP call the server exposes, including destructive ones. It is honored *only* for a
  declared-coarse tool — the gateway picks the required op per tool, so a wildcard token can
  never satisfy a `fine` tool — and the audit log records the real MCP tool called either
  way. Put sensitive servers behind `gate: fine`.
- **The loopback-backend guard closes a LAN bypass.** The gateway proxies host-side, so a
  non-loopback `backend` would hand a jailed agent a path to other hosts *through the
  broker*, circumventing the jail's LAN-drop egress. Config validation rejects it unless the
  tool sets `allow_non_loopback: true` (an explicit, reviewed opt-in).
- **The gate protects the *jailed agent's* path, nothing more.** Any other host-local
  process can still hit the server's `127.0.0.1` port directly — host processes are already
  inside the host trust boundary; the broker does not claim to sandbox them from each other.

## 7. If an agent is compromised: what it can and can't do

Prompt injection, a poisoned dependency, a malicious tool description — assume any agent *can* be
turned. This section is the honest accounting of what a compromised agent achieves in each
scenario, and what it does not.

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
- **Containing daemon authority inside the jail.** The architecture depends on the Scion broker
  driving a Docker daemon. Any agent that holds the rootless Docker socket directly can launch
  arbitrary (including `--privileged`) containers *within the jail*. (No agent, manager included,
  holds Scion hub authority itself, that lives host-side only, gated by the controller PAT, §4.2,
  so an agent driving the hub to launch containers is not a route here.) Containment of direct
  daemon-socket access rests entirely on the jail's filesystem/network bound and the kernel, not on
  denying daemon access.
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

## 9. Validation evidence

Validated by hand on macOS + OrbStack (Apple Silicon). What was demonstrated:

- **The escape exists without the jail.** From inside an ordinary agent container (no host
  filesystem, no Docker socket), using only a Scion hub **development-auth token** (the mode used
  for this test; the real hub now runs `--dev-auth=false` and no agent is ever handed a hub
  credential, §4.2), the hub accepted a new project + a **provider with an arbitrary host path (no
  path validation)**; starting an agent then drove the broker to bind-mount a sensitive host
  directory into a new container, whose files were readable. This is the threat the jail closes:
  even where an agent *did* hold a hub credential, there is no host filesystem outside the project
  tree for it to mount (§2.1).
- **Filesystem containment holds with the jail.** An isolated machine has no host home; the host
  secret directories are absent, so the same escape mounts nothing.
- **LAN unreachable; host loopback reachable and clampable.** From inside the jail the LAN router
  was unreachable (100% packet loss) while a host loopback tool server answered via the alias (over
  both IPv4 and IPv6); an egress allowlist then permitted one tool port and dropped the rest, and
  the rule still held for a rootless `--network=host` container (the topology agents actually use).
- **The runtime runs inside the jail.** Rootless Docker runs containers (native `overlayfs`); the
  Scion binary builds and runs; a rootless host-networked container obeys the egress allowlist.

What is **not** yet validated (pending the full-system test): the project-tree mount's *allow* side
(that exactly the chosen tree is present and nothing else), the real manager Claude agent under
rootless Docker, the manager's MCP reachability in practice, and a live run of the §4
single-project isolation guarantee against a real `scion start` (the code is merged and the
required Scion fixes are in the pinned commit, but there is no wired acceptance check for it yet,
see §4.2).

> Validation was performed by checking reachability and file presence by size/permission, never by
> printing secret contents.

## 10. Reporting a vulnerability

Pre-release; a security contact will be published with the first release. If you find a containment
hole in the meantime, please open a minimal-detail issue and request a private channel.
