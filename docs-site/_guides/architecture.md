---
title: Architecture
nav_order: 4
---
# Architecture

> **Mostly built.** Jail bring-up, the manager up/attach lifecycle, the capability broker, worker
> dispatch (the manager calling the broker's `/worker/*` endpoints), broker-routed messaging
> (`/msg/send`, `/msg/list`), and the `lever-manager watch` bridge are implemented and validated (see
> [security-model.md](/security-model/)). The notification contract in
> §4 (the `input-needed`/`completed` event names) is still being refined; treat those event names as
> illustrative, not literal identifiers.

Lever is a thin orchestration-and-interface layer over [Scion](https://github.com/GoogleCloudPlatform/scion),
which provides the container runtime, agent sessions, attach/resume, and typed messaging. Two Scion
terms recur below: the **Scion broker** (Scion's host-side component that asks the container runtime to
create containers and apply mounts) and the **hub** (Scion's registry of projects and agents). Lever
adds four things Scion does not: an **opinionated project model** (a project is a directory), a
**security jail** that contains the whole runtime, a **capability broker** (Lever's own host-side
credential and tool-access broker, distinct from the Scion broker above), and a **single operator
surface** (`lever`).

## 1. Layers

{% raw %}
```mermaid
graph TD
    subgraph host[macOS host]
        L[lever CLI, operator binary]
        BK["Capability broker<br/>real credentials, capability minting,<br/>/llm proxy, worker dispatch, MCP gateway,<br/>messaging (/msg/send, /msg/list)"]
        MCP[first-party tool servers<br/>bound to 127.0.0.1]
    end
    subgraph vm[OrbStack VM, the one hardware-virtualization boundary]
        subgraph jail[Isolated machine, THE JAIL]
            SS["Scion server + runtime broker<br/>one project for the whole instance"]
            RD[rootless dockerd]
            FW{{egress allowlist<br/>iptables / ip6tables<br/>enforced in jail netns}}
            subgraph agents[Agent containers, rootless, same project]
                MGR[Manager agent, the coordinator]
                WA[Worker agent A]
                WB[Worker agent B]
            end
        end
    end
    L -->|attach / drive| SS
    L -.->|spawns| BK
    SS --> RD
    RD --> MGR
    RD --> WA
    RD --> WB
    BK --- MCP
    BK -->|drives Scion for worker dispatch| SS
    agents -->|all network egress| FW
    FW -->|"allowlisted: broker + model API<br/>via host.orb.internal"| BK
    FW -.->|LAN ranges dropped| LAN[LAN / other hosts]
```
{% endraw %}

- **Only the OrbStack VM is a hardware-virtualization boundary**, you already run it for all Docker
  use. The jail (an OrbStack *isolated machine*) and every container below it are kernel namespaces,
  so there is effectively no per-level CPU tax; nesting is cheap. (With the `orbstack`/`lima`
  backends this also means a *single* kernel is shared across the manager and all workers, a security
  trade noted in [security-model.md §7](/security-model/); the `apple-container` backend gives each
  agent its own VM kernel instead.)
- **The jail is the containment boundary**, not Scion. The egress allowlist is enforced in the
  jail's network namespace, outside the agent containers.
- **OrbStack is the reference *backend*, not a hard dependency.** The jail is a contract (a
  hypervisor boundary, no host files, a controllable netns, egress enforced in it, a host-reachable
  broker); OrbStack is one implementation, `lima` (macOS/Linux, its own VM kernel) is the second,
  and `apple-container` (per-agent micro-VM) is on the roadmap. Each declares its own guarantees,
  run `lever backends` or see [containment backends](/reference/backends/). Notably, **Docker
  Desktop is not a backend** (its shared VM auto-mounts your home and its netns is not yours to
  control), and a native, no-VM Linux backend (`linux-docker`) was explored and rejected for
  sharing the host kernel outright, see the backends page for both writeups.

## 2. The project model: a project is a directory

A Lever **instance is one Scion project**, registered once at the tree root (a non-git "linked"
project — Scion's `.scion` marker is externalized, not committed into the tree). The manager and
every worker are **agents inside that single project** — not, as an earlier design had it, separate
projects per agent. Each agent is bound to an explicit, in-place workspace via `--workspace`: the
manager's workspace is the whole tree root; a worker's workspace is one subdirectory of it. There
are no clones, no git worktrees, and no sync loop, an agent edits the real files, exactly as a human
in that directory would. A worker's own subdirectory may itself contain a git repository; the runtime
neither knows nor cares (a git repo is just files) — but the **tree root itself must be non-git**, see
below.

{% raw %}
```mermaid
graph TD
    root["project tree root = MANAGER workspace (whole tree, rw)<br/>- the jail's only mounted host dir<br/>- ONE Scion project for the whole instance"]
    root --> kb["instance content: knowledge base + tools<br/>(instance convention, not required by the core)"]
    root --> workers["workers/"]
    workers --> a["workers/app-a/ → worker A workspace (repo inside)"]
    workers --> b["workers/app-b/ → worker B workspace (repo inside)"]
```
{% endraw %}

- **Manager**, an agent whose workspace is the whole tree root, so it sees everything the instance
  keeps there (its knowledge base, tools) and a live view of every worker.
- **Workers**, each an agent in the *same* project as the manager, bound to its own subdirectory.
  Isolation between workers is **"defense by absence"**: Scion never enumerates sibling agents to
  build deny/shadow mounts, a worker's container simply never bind-mounts anything but its own
  configured subdirectory, so a sibling's directory is not a mount source at all for it, not merely
  hidden by convention. This holds only on a **non-git tree root** (config validation refuses/warns on
  one at load time, and the pinned Scion always plain-mounts an explicit `--workspace` regardless of a
  stray ancestor `.git`); see [security-model.md §4](/security-model/) for the full guarantee and its
  residual.
- **Overlapping mounts are intentional for the manager**, its workspace physically contains every
  worker's directory, so edits are live to all parties. This is a single writable tree from the
  manager's vantage point: *file-level* isolation between the manager and a worker's subdirectory
  rests on convention, not enforcement, the manager is trusted with whole-tree oversight by design.
  It is **not** a file-access control against a hostile *worker* though, a worker cannot reach outside
  its own subdirectory in the first place (previous bullet). The *dispatch* boundary is separately
  enforced: the manager can only start workers declared in the config, and only via the broker (see
  [security-model.md §5.4](/security-model/)).
- The core only requires a tree root plus a configured worker subdirectory; the `knowledge base +
  tools` layout above is an *instance* convention, as is nesting worker directories under `workers/`.

**Git mode is never used, and the tree root itself must be non-git.** Scion's git-anchored project
mode triggers a clone per agent and the shared-worktree path is unreliable; more fundamentally, a git
tree root would defeat the defense-by-absence guarantee above, so the directory model targets
non-git tree roots and config validation enforces it.

## 3. Components

| Component | Role | Core or instance |
|---|---|---|
| `lever` (Go binary) | operator CLI + entry point; drives Scion; provisions the jail | **core** (runs on host) |
| Scion server + Scion broker | container lifecycle, sessions, attach/resume, typed messaging | core (runs inside the jail) |
| rootless dockerd | the container runtime the Scion broker drives (rootless, see security-model.md) | core (inside the jail) |
| Lever capability broker | host-side: holds the real model key, mints CN-bound capability tokens, proxies `/llm` and gated MCP tool calls, and relays typed agent messaging (`/msg/send`, `/msg/list`) | **core** (runs on host) |
| Manager **runtime/role** | the coordinator: a singleton agent with the whole-tree workspace that dispatches work and watches events | **core role** |
| Manager **prompt / skills / tool (MCP) config** | what makes it *this* manager | **instance-supplied config** |
| Worker agents | agents in the instance's one Scion project, each bound to its own subdirectory workspace; isolated from siblings by defense-by-absence (§2), not a separate project | core lifecycle; instance defines the workers |
| Agent base image | the coding-agent harness container | **core ships a generic minimal base; the instance extends/bakes its own** (see §6) |
| Notification bridge | turns Scion's event stream into a file/sink the operator watches | core mechanism; **sink path is instance-configured** |

The core knows the *manager* as a first-class role (singleton, whole-tree workspace, event-watcher),
but everything that makes it a *particular* manager, its boot prompt, its skills, which tool/MCP
ports it may reach, is configuration the instance supplies.

## 4. The dispatch / notification loop

The manager dispatches a unit of work to a worker and then watches a typed event stream rather than
polling. Two event types matter most: `input-needed` (the worker is blocked on a decision) and a
terminal `completed`.

{% raw %}
```mermaid
sequenceDiagram
    participant Hu as Human
    participant Mg as Manager
    participant Br as Broker
    participant Sc as Scion
    participant Wk as Worker agent
    Hu->>Mg: "do X in app-a"
    Mg->>Br: start worker (POST /worker/start, mTLS, correlation id)
    Br->>Sc: start worker (controller PAT, host-side)
    Sc->>Wk: launch container, deliver task
    Wk-->>Sc: event: input-needed ("which DB?")
    Sc-->>Br: typed event (polled via POST /msg/list, mTLS)
    Br-->>Mg: relayed via the watch bridge
    Mg->>Hu: relay question
    Hu->>Mg: answer
    Mg->>Br: message worker (POST /msg/send, mTLS)
    Br->>Sc: relay message (controller PAT, host-side)
    Sc->>Wk: deliver
    Wk-->>Sc: event: completed
    Sc-->>Br: typed event (polled via POST /msg/list, mTLS)
    Br-->>Mg: relayed via the watch bridge (echoes the correlation id)
    Mg->>Hu: report done
```
{% endraw %}

Messaging follows the same broker-mediated shape as dispatch above: `lever-manager msg send`/`msg
list`/`watch` are thin mTLS clients of the broker's `/msg/send` and `/msg/list`, never scion
directly, an in-container `scion` CLI call has no hub credential to authenticate with in the first
place, the real hub runs dev-auth off and only the host-side broker holds the controller PAT (see
[security-model.md §4](/security-model/)), so only the broker's host-side, controller-PAT-authorized
scion access can safely address an arbitrary agent's inbox.

**The task ↔ agent contract.** The core knows nothing about an instance's task records. At dispatch
the instance supplies an opaque **correlation id**; the core echoes that id on lifecycle events
(notably `completed`). The instance maps the id back to its own record and decides what "close the
task" means. So the live agent stream tells you *how it's going*; the instance's records remain the
authority on *what* and *whether done*.

## 5. Entry point

`lever` is the single command an operator runs on the host. It:

1. Ensures the jail (isolated machine) is up, with rootless Docker, the Scion server/broker, and
   the egress allowlist applied.
2. Ensures the manager agent is up, resuming the prior conversation if it was suspended, creating
   it if absent, attaching if already running.
3. Hands the terminal to the manager session (the Scion server/broker run inside the jail; `lever`
   attaches in from the host). On detach, the manager is left **suspended** so the next `lever`
   resumes the same conversation.

(How much of the attach/tmux UX is generic core vs instance presentation is still being decided.)

Three lifecycle verbs, at increasing cost: **detach** (`Ctrl-b d`, leave the TTY, manager suspended
in memory, jail machine keeps running) < **`lever stop`** (suspend the manager best-effort, stop the
host broker, power the jail machine off — disk and session preserved; `lever up` powers back on and
resumes the same manager conversation) < **`lever
destroy`** (delete the jail machine and clear staged runtime state — `lever up` fully re-provisions;
`lever down` is a deprecated alias, kept for compatibility).

## 6. Agent image & runtime provisioning

The **core ships a generic, minimal base image** carrying only the coding-agent harness, it is
deliberately language-agnostic. An **instance extends it** (or bakes its own) for whatever its
workers need. Two patterns, both instance choices:

- **Per-worker on demand:** agents install language runtimes inside their containers as needed (a
  Ruby version manager, Node, Python). Keeps the image small; pays a cold-start.
- **Baked:** the instance builds an image with its common runtimes pre-installed. Faster start; less
  generic. (The reference instance bakes a default toolchain, an *instance* artifact, not part of
  the core.)

**Filesystem performance note:** compute nesting is near-native, but files served from the host via
the project-tree mount cross OrbStack's virtiofs, which is slow for metadata-heavy operations (large
dependency installs). A worker that runs its *own* Docker compounds overlay filesystems; prefer
sibling containers (sharing the jail's rootless daemon) over a nested daemon. See
[security-model.md §2.3](/security-model/) for the rootless requirement.
