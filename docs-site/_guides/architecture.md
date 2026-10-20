---
title: Architecture
nav_order: 4
---
# Architecture

The bridge (`lever-manager watch`) relays Scion's inbox events verbatim as JSON lines; lever defines
no event types of its own. The `input-needed` event and the `COMPLETED` state change in §4 are
Scion's names, which the operator skill relies on (internal/skills/lever-operator/SKILL.md).

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
flowchart TB
    subgraph host["HOST · macOS or Linux"]
        direction LR
        L(["<b>lever</b><br/>operator CLI"])
        BK["<b>Capability broker</b><br/>holds the real credentials<br/>mints capability tokens<br/>/llm · /mcp/&lt;tool&gt; · /msg/*"]
        MCP[("<b>Broker tools</b><br/>MCP servers<br/>on 127.0.0.1")]
        L -. spawns .-> BK
        BK --- MCP
    end

    subgraph vm["LINUX VM · OrbStack or Lima · the hardware-virtualization boundary"]
        subgraph jail["THE JAIL · an OrbStack isolated machine, or the Lima VM itself"]
            SS["<b>Scion</b><br/>server + runtime broker<br/>one project per instance"]
            RD["<b>rootless podman</b><br/>runs every agent"]
            MGR(["Manager"])
            WA(["Worker A"])
            WB(["Worker B"])
            FW{{"<b>Egress allowlist</b><br/>iptables / ip6tables<br/>in the jail's netns"}}
        end
    end

    LAN["LAN · other hosts"]

    L ==>|attach / drive| SS
    BK ==>|worker dispatch| SS
    SS --> RD
    RD --> MGR & WA & WB
    MGR --> FW
    WA -->|all egress| FW
    WB --> FW
    FW ==>|"allowlisted only:<br/>broker + model API<br/>via the host alias"| BK
    FW -. dropped .-x LAN

    classDef hostNode fill:#3b82f61f,stroke:#3b82f6,stroke-width:1.5px
    classDef jailNode fill:#f59e0b1f,stroke:#d97706,stroke-width:1.5px
    classDef agentNode fill:#10b9811f,stroke:#059669,stroke-width:1.5px
    classDef guard fill:#ef44441f,stroke:#dc2626,stroke-width:2px
    classDef outside fill:none,stroke:#9ca3af,stroke-dasharray:4 3,color:#9ca3af
    class L,BK,MCP hostNode
    class SS,RD jailNode
    class MGR,WA,WB agentNode
    class FW guard
    class LAN outside

    style host fill:#3b82f60d,stroke:#3b82f6,stroke-width:2px
    style vm fill:#8b5cf60d,stroke:#8b5cf6,stroke-width:2px,stroke-dasharray:6 4
    style jail fill:#f59e0b0d,stroke:#d97706,stroke-width:2.5px
```
{% endraw %}

- **The host** is macOS or Linux. `lever`, the capability broker and the broker tools run there,
  outside the jail.
- **The Linux VM is the only hardware-virtualization boundary.** With the `orbstack` backend
  (macOS) the jail is an OrbStack *isolated machine* inside OrbStack's VM; with the `lima` backend
  (macOS or Linux) the jail is the Lima VM itself, with its own kernel. The jail and the containers
  inside it are kernel namespaces, so nesting adds no per-level CPU cost, and on either backend a
  single kernel is shared across the manager and all workers — a security trade noted in
  [security-model.md §8](/security-model/compromise/).
- **The jail is the containment boundary**, not Scion. The egress allowlist is enforced in the
  jail's network namespace, outside the agent containers. Allowlisted traffic reaches the broker
  through the backend's host alias: `host.orb.internal` on OrbStack, `host.lima.internal` on Lima.
- **The jail is a contract, not a product.** OrbStack and Lima both implement it; see
  [containment backends](/reference/backends/).

## 2. The project model: a project is a directory

A Lever **instance is one Scion project**, registered once at the tree root (a non-git "linked"
project — Scion's `.scion` marker is externalized, not committed into the tree). The manager and
every worker are **agents inside that single project**. Each agent is bound to an explicit,
in-place workspace via `--workspace`: the manager's workspace is the whole tree root; a worker's
is one subdirectory of it. There are no clones, no git worktrees, and no sync loop — an agent
edits the real files. A worker's subdirectory may itself contain a git repository (to the runtime
it is just files), but the **tree root itself must be non-git**, see below.

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

- **Manager** — workspace is the whole tree root: the instance's knowledge base and tools, plus a
  live view of every worker.
- **Workers** — each an agent in the *same* project, bound to its own subdirectory. Isolation
  between workers is **"defense by absence"**: a worker's container bind-mounts only its own
  configured subdirectory, so a sibling's directory is never a mount source for it — not merely
  hidden by convention. This holds only on a **non-git tree root** (config validation enforces it
  at load time, and the pinned Scion plain-mounts an explicit `--workspace` even under a stray
  ancestor `.git`); see [security-model.md §4](/security-model/worker-isolation/) for the full guarantee and its
  residual.
- **The manager's mounts overlap the workers' by design** — its workspace physically contains
  every worker directory, so edits are live to all parties. File-level isolation between the
  manager and a worker's subdirectory is convention, not enforcement: the manager is trusted with
  whole-tree oversight. This is not an access control against a hostile *worker* — a worker cannot
  reach outside its own subdirectory at all (previous bullet). The *dispatch* boundary is enforced
  separately: the manager can start only workers declared in the config, and only via the broker
  ([security-model.md §5.4](/security-model/config-trust/)).
- The core requires only a tree root plus configured worker subdirectories; the `knowledge base +
  tools` layout and the `workers/` nesting above are instance conventions.

**Scion's git mode is never used.** Git-anchored project mode triggers a clone per agent and its
shared-worktree path is unreliable; more fundamentally, a git tree root would defeat the
defense-by-absence guarantee above. Config validation enforces a non-git tree root.

## 3. Components

| Component | Role | Core or instance |
|---|---|---|
| `lever` (Go binary) | operator CLI + entry point; drives Scion; provisions the jail | **core** (runs on host) |
| Scion server + Scion broker | container lifecycle, sessions, attach/resume, typed messaging | core (runs inside the jail) |
| rootless podman | the container runtime the Scion broker drives (rootless, see security-model.md) | core (inside the jail) |
| Lever capability broker | host-side: holds the real model key (api-key mode), mints CN-bound capability tokens, proxies `/llm` (api-key mode) and gated MCP tool calls, relays typed agent messaging (`/msg/send`, `/msg/list`), and runs the [operator-directive](/operator-directives/) channel (a 0600 UDS admin socket + agent-facing `directive_consume` over mTLS; see [security-model](/security-model/operator-directives/)) | **core** (runs on host) |
| First-party broker tools | MCP servers the broker supervises on the host (`command:` in `broker.tools`): `lever-tool-github` ([github](/github-tool/)), `lever-tool-whisper` ([whisper](/whisper-tool/)), `lever-tool-tts` ([tts](/tts-tool/)); the jail holds none of their credentials | **core** (runs on host) |
| Remote proxy (`lever remote serve`) | optional (`remote:`): the credential-injecting web front for the hub and the chat page, on loopback (default port 8445) | **core** (runs on host) |
| Manager **runtime/role** | a singleton agent with the whole-tree workspace that dispatches work and watches events | **core role** |
| Manager **prompt / skills / tool (MCP) config** | what makes it *this* manager | **instance-supplied config** |
| Worker agents | agents in the instance's one Scion project, each bound to its own subdirectory workspace; isolated from siblings by defense-by-absence (§2), not a separate project | core lifecycle; instance defines the workers |
| Agent base image | the coding-agent harness container | **core ships a generic minimal base; the instance extends/bakes its own** (see §6) |
| Notification bridge | turns Scion's event stream into a file/sink the operator watches | core mechanism; **sink path is instance-configured** |

The core knows the *manager* as a first-class role (singleton, whole-tree workspace, event-watcher),
but everything that makes it a *particular* manager, its boot prompt, its skills, which tool/MCP
ports it may reach, is configuration the instance supplies.

## 4. The dispatch / notification loop

The manager dispatches a unit of work to a worker and then watches a typed event stream rather than
polling. Two events matter most: `input-needed` (the worker is blocked on a decision) and the state
change to `COMPLETED`. Both are Scion events that the bridge relays verbatim; lever adds nothing to
them.

{% raw %}
```mermaid
sequenceDiagram
    participant Hu as Human
    participant Mg as Manager
    participant Br as Broker
    participant Sc as Scion
    participant Wk as Worker agent
    Hu->>Mg: "do X in app-a"
    Mg->>Br: start worker (POST /worker/start, mTLS)
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
    Wk-->>Sc: event: state-change (COMPLETED)
    Sc-->>Br: typed event (polled via POST /msg/list, mTLS)
    Br-->>Mg: relayed via the watch bridge
    Mg->>Hu: report done
```
{% endraw %}

Messaging follows the same broker-mediated shape as dispatch: `lever-manager msg send`/`msg
list`/`watch` are thin mTLS clients of the broker's `/msg/send` and `/msg/list`, never of Scion
directly. An in-container `scion` CLI call has no hub credential to authenticate with beyond
scion's own per-agent token — the hub runs with dev-auth off and trusts only assertions signed with
a host-held key, and only the host-side broker holds the controller PAT (see
[security-model.md §4](/security-model/worker-isolation/)) — so only the broker can address an arbitrary agent's
inbox. Because every broker send wears the controller's hub user as its sender, the broker records
each one in a host-side sent ledger first, and a recipient learns who wrote a message only by
asking the broker (`message_verify`); see [verified web chat](/remote-access/#verified-web-chat).

**The task ↔ agent contract.** The core knows nothing about an instance's task records and carries
no correlation id. The bridge relays agent messages verbatim; an instance that needs correlation
instructs its workers to echo a task id in their messages (see [conventions](/conventions/)). The
instance's records remain the authority on what was asked and whether it is done.

## 5. Entry point

`lever up` is the everyday command an operator runs on the host. It:

1. Ensures the jail (the OrbStack isolated machine or the Lima VM) is up, with rootless podman,
   the Scion server/broker, and the egress allowlist applied.
2. Ensures the manager agent is up, resuming the prior conversation if it was suspended, creating
   it if absent, attaching if already running.
3. Hands the terminal to the manager session (the Scion server/broker run inside the jail; `lever`
   attaches in from the host). On detach, the manager keeps **running**; the next `lever up` or
   `lever attach` re-attaches to the same conversation.

Three lifecycle verbs, at increasing cost:

- **detach** (`Ctrl-b d`) — leave the TTY. The manager keeps running (lever suspends nothing on
  detach); the jail machine keeps running.
- **`lever stop`** — suspend the manager and every running worker (best-effort), stop the host
  broker, power the jail machine off. Disk and session are preserved; `lever up` powers back on and resumes the same
  conversation.
- **`lever destroy`** — delete the jail machine and clear staged runtime state; `lever up` fully
  re-provisions. (`lever down` is a deprecated alias.)

## 6. Agent image & runtime provisioning

The **core ships a generic, minimal base image** carrying only the coding-agent harness;
it is deliberately language-agnostic. An **instance extends it** (or bakes its own) for whatever
its workers need. Two patterns, both instance choices:

- **Per-worker on demand:** agents install language runtimes inside their containers as needed (a
  Ruby version manager, Node, Python). Keeps the image small; pays a cold-start.
- **Baked:** the instance builds an image with its common runtimes pre-installed. Faster start; less
  generic. (A baked toolchain is an *instance* artifact, not part of the core.)

**Images are tagged by architecture.** Agent images carry an arch tag
(`lever.local/lever-claude:arm64` / `:amd64`), never a shared `:latest`, so a host that cross-builds
both — an arm64 laptop producing an amd64 server image — never clobbers one arch with the other. A
**tagless** `manager.image` in the config auto-resolves to the jail's arch at apply time (the jail's
arch equals its host's), so **one config is portable** across an arm64 laptop and an amd64 server; an
explicitly-tagged or digest-pinned ref is honored verbatim as an escape hatch. `make lever-image
LEVER_IMAGE_ARCH=<arch>` builds `FROM scion-claude:<arch>` and tags the output to match.

**Filesystem performance note:** compute nesting is near-native, but files served from the host via
the project-tree mount cross the VM's shared-folder mount (virtiofs on OrbStack and on Lima's
`vz`), which is slow for metadata-heavy operations (large
dependency installs). A worker that runs its *own* Docker compounds overlay filesystems; prefer
sibling containers (sharing the jail's rootless podman) over a nested daemon. See
[security-model.md §2.3](/security-model/jail/) for the rootless requirement.

## 7. Agent identity & the cert path

Every call an agent makes to the capability broker is mTLS, authenticated by a short-lived (24h)
per-agent certificate whose CN every capability token is bound to. Enrolment, the renewal
sidecars, the per-handshake re-read invariant, and the two long-lived in-container clients (the
agent gateway and the capability server) are described in
[Agent identity & certificates](/agent-identity/).
