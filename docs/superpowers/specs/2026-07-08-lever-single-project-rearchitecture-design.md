# Lever single-project re-architecture — design spec

**Status:** design spec for review · 2026-07-08
**Supersedes framing in:** `drafts/2026-07-08-lever-on-scion-rearchitecture-findings.md` (candidates + clean-room spike record). This spec consolidates only the locked decisions into an implementation-facing form.
**Clean break:** lever is prerelease. No backward compatibility with the current grove-per-project model is required or attempted.

---

## 1. Goal

Re-architect lever so that **one lever instance is one Scion project**, with the manager and every worker running as **agents in that single project**, each bound to an explicit in-place subdirectory workspace. This replaces today's model — one Scion project *per agent* — which fights Scion's design (a project is a namespace of many agents) and forces per-agent project registration, per-project verb scoping, and an identity overload where a grove's name is simultaneously its agent slug, project name, ticket subject, and cert CN.

The re-architecture is enabled by a single insight ("defense by absence", §4) and depends on **two tiny upstream Scion fixes already submitted as PRs** — no lever-specific Scion fork.

## 2. Requirements

- **R1 — Containment is the boundary.** The whole agent stack sits behind a VM-isolation wall; a compromised or prompt-injected agent cannot reach the host filesystem, host credentials, the LAN, or the internet except through controlled channels.
- **R2 — Edit the real tree in place.** Agents modify the user's actual working directory (bind-mounted), never a copy.
- **R3 — One manager + N workers.** A persistent manager agent (the human converses with it; it survives restart) dispatches and coordinates scoped worker agents.
- **R4 — Nested workspaces + sibling isolation (the crux).** One shared in-place tree. The manager's workspace is the whole tree (live oversight). Each worker's workspace is a **subdirectory** of that tree, in place. Workers are isolated from each other: a worker reaches only its own subdir, never a sibling's nor the broader tree. The manager reaches all.
- **R5 — Capability-brokered host access (preserved).** Host resources reach agents only via the host-side broker and per-agent capability tokens over mTLS. No ambient authority inside the jail.
- **R6 — Per-agent least privilege.** Each agent has its own scoped identity; no agent can act as another or as hub admin; admin authority exists host-side only.
- **R7 — Persistence & clean re-bootstrap.** Manager conversation and agent state survive `stop`→`start`; a full teardown→bring-up re-bootstraps idempotently.
- **R8 — Manager↔worker messaging**, both directions.
- **R9 — Minimize (ideally zero) Scion changes.**
- **R10 — Single-config operator UX** (bring-up / stop / teardown, attach to the manager TTY).

## 3. Current architecture and what breaks

The current model (grove-per-project) is load-bearing in these places; the re-architecture removes the assumption at each:

| Current (grove-per-project) | Anchor | Target (single-project) |
|---|---|---|
| Manager = its own project at tree root; **each grove = its own separate Scion project** rooted at its subdir | `broker/grove.go:44-45`, `brokerctl/grovespecs.go:19` | One project per instance; manager + workers are **agents** in it |
| Grove `Name` == agent slug **==** project name == ticket subject == cert CN | `broker/grove.go:44-45`, `cap/ca/ticket.go`, `enrol.go` | Worker name == agent slug **only**; project name is the single instance project (identities decoupled) |
| Every Scion verb `-g <perGroveProject>`-scoped | `scion/client.go:65-70` | Every verb `-g <instanceProject>` — one constant scope |
| `register-manager` + one `register-grove` **per grove**, each an independent `scion init` + `hub link` | `apply/plan.go:68-71`, `run.go:217-278` | **One** project init + link for the instance; agents need no per-agent project registration |
| `/grove/list` fans `scion list` across N per-grove projects and concatenates | `broker/grove.go:242-250` | One `scion list -g <instanceProject>` returns all agents |
| Grove `dir` must be a strict subdir; `dir: "."` rejected to avoid colliding with the manager's project | `config.go:483-491` | Manager (root) and workers (subdirs) legitimately share one project; the collision rule is obsolete |
| No Scion token minted; relies on Scion **dev-auth open on loopback** (`--dev-auth=true` default), passes no dev token | `bringup.go:60-61`, `client.go:42-63` | Real hub runs **dev-auth off**; lever drives all lifecycle with a minted **controller PAT** (hardening + enables the model) |
| Guest state hardcodes `/lever` and `/lever/groves/<name>` as one workspace_path per agent | `backend/guest/scionstate.go:20-22` | One project; agent workspaces are `tree` (manager) and `tree/<subdir>` (workers) |

What is **unchanged**: lever's host-side capability broker + mTLS enrolment (CA, `/bootstrap` latch, `/enrol` CSR signing, per-agent tickets); the CLI shell-out seam to `scion`; the manager TTY attach via `syscall.Exec`; live in-place `--workspace` bind mounts; secret/env plumbing via `scion hub secret set`.

## 4. Target model — one project, agents as in-place subdir workspaces

lever instance = **one Scion Project**, `scion project init` at the real working-tree root, treated as **non-git** (the `.scion` marker is externalized). The manager and every worker are **Agents** in that project:

```
manager   container:   <root>        → /workspace   (whole tree, in place)
worker-A  container:   <root>/svcA   → /workspace   (only svcA, in place)
worker-B  container:   <root>/svcB   → /workspace   (only svcB, in place)
```

- Manager: `scion start <app> --workspace <root> -g <instanceProject>`.
- Worker N: `scion start <worker-N> --workspace <root>/<subN> -g <instanceProject> "<task>"`.

### 4.1 Defense by absence (why R4 is free)

Scion's per-agent filesystem isolation is **structural, not configured**. Each agent's container bind-mounts **only its own workspace path** and nothing else; Scion never enumerates sibling agents to build deny/shadow mounts. Therefore:

- A worker's container only ever bind-mounts `<root>/<subN>`. A sibling's `<root>/<otherN>` is **not a mount source** — it is unreadable at the kernel/VM boundary, not merely hidden. (Container UIDs are synced to the host UID, so file permissions give *no* inter-agent isolation; the guarantee is structural absence plus the per-container VM wall from R1.)
- The manager's container bind-mounts `<root>`, and Scion does **not** shadow child workspace dirs inside a broader mount — so the manager sees every worker's live in-place edits.

Manager oversight and sibling isolation are the **same mechanism** viewed at two mount scopes. This is what the previous design missed by using managed worktrees/clones instead of in-place subdir mounts.

### 4.2 Non-git requirement and the `.git` trap

The isolation guarantee holds cleanly **only on a non-git tree**. If the tree (or any ancestor) is a git repo, Scion's mount builder currently takes a worktree branch that also bind-mounts the whole `.git` object store at `/repo-root/.git`, through which a worker can read *committed* sibling content (uncommitted sibling files stay invisible). This was confirmed live in the clean-room spike (`git --git-dir=/repo-root/.git show HEAD:<sibling>` recovered a sibling secret).

**Resolution:** the `--workspace` git-guard fix (§5, PR4) makes an explicit `--workspace` always plain-mount that exact directory regardless of surrounding `.git`, restoring airtight isolation and making per-worker git (a worker's *own* `.git`) work correctly. lever targets non-git-root trees; the guard makes a stray ancestor `.git` harmless rather than a leak.

## 5. Scion dependency — two upstream fixes, zero fork

The net Scion delta is **two small, self-contained upstream bug fixes**, both already opened as PRs against `GoogleCloudPlatform/scion` from the `stevegeek/scion` fork. Neither is lever-specific.

| Fix | Branch | Role in this design |
|---|---|---|
| `ListProjects` cursor pagination | `fix/listprojects-cursor-pagination` | Correct project/agent enumeration beyond the first page |
| `--workspace` git guard (`run.go`, honor explicit workspace over git auto-detection) | `fix/explicit-workspace-git-guard` | Makes §4.2 isolation a guarantee, not a layout precaution |

Everything else — mounts, agent lifecycle, project-scoped tokens, per-agent JWT identity, messaging, suspend/resume — is **stock Scion**. The offline `hub bootstrap` command and the `auto_provide` change explored earlier are **retired**: the single-project model eliminates the multi-project token dilemma, and normal `hub link` project creation fires provider auto-linking for free.

lever pins the Scion version it builds against; these two fixes must be present in that pin (via the fork branch or once merged upstream).

## 6. Bring-up and bootstrap

### 6.1 Controller token via a throwaway dev-auth server

One project ⇒ one project-scoped PAT drives every agent's lifecycle. The real hub runs **dev-auth off** (hardening: agents never see an admin-open hub). During **agent-free** bring-up:

1. Start a **throwaway** hub: `scion server start --port <random> --dev-auth=true`, host-only, on a random port no agent ever learns.
2. As dev-auth admin against it: `project init` + `hub link` for the instance project, then mint the controller PAT: `scion hub token create --scopes agent:manage,agent:attach,agent:message,project:read`.
3. Persist the PAT host-side, `0600`, under `.lever-state/`.
4. **Kill the throwaway.**
5. Start the real hub: `scion server start --port 8080 --dev-auth=false`.
6. Only then dispatch the manager (and, later, workers).

**Controller token scope is exact and load-bearing:** `agent:manage,agent:attach,agent:message,project:read`. The `agent:manage` alias does **not** include `agent:attach` or `agent:message`, which gate `start`/`stop`/`suspend`/`resume`/`message`/`attach`; a token missing them 403s on `start`.

**Safety with no Scion change:** the dev-auth window exists only on a random throwaway port and is dead before any container exists; the canonical `:8080` hub is `--dev-auth=false` from birth. "Agent-free" is an assertable precondition of the window, so no worker can race in to exploit open admin. Reconciliation on `down`→`up` is agent-free again. A mid-life invalid PAT fails loud and forces a controlled `stop`→`up`; dev-auth is **never** re-enabled on a running instance that has agents.

### 6.2 Token plumbing

lever's Scion CLI seam must present the controller PAT on every verb once the real hub is dev-auth-off. This is a new env var (e.g. `SCION_HUB_TOKEN`) injected by the client env builder (`scion/client.go:54-63`), read from the persisted `.lever-state/` PAT. Host-side operator paths (attach, `lever msg`, stop) use the same PAT.

### 6.3 Revised apply plan (shape)

The ordered `apply` steps collapse the per-grove registrations into one project registration and insert the bootstrap window:

1. `jail-up` — backend brings the VM + egress allowlist up.
2. `broker-up` — start the host-side lever broker + health check.
3. `load-image` ×N — load distinct images into the jail.
4. `init-machine`, `config-registry` — `scion init --machine`, set the local image registry.
5. **`bootstrap-token`** *(new)* — throwaway dev-auth server → project init + link + mint controller PAT → persist → kill (§6.1).
6. `scion-server` — start the real hub **dev-auth off** + wait ready.
7. `credential` (optional) — stage the LLM OAuth/API credential.
8. **`register-project`** *(replaces `register-manager` + N×`register-grove`)* — one idempotent project init/link for the instance.
9. `mint-manager-bootstrap` — broker `/bootstrap` enrol ticket for the manager; stage `bootstrap.json`.
10. `start-manager` — observe-then-act on the manager's agent record (running → verify; suspended/stopped → resume; else fresh) + wait live.

Workers are **not** started at apply time; the manager dispatches them on demand (§7).

## 7. Lifecycle and dispatch

- **Workers as agents in the one project.** The broker's worker lifecycle (`/grove/*`, to be renamed) issues `scion start <worker> --workspace <root>/<subdir> -g <instanceProject>` — the **same** project as the manager. `list` becomes a single `scion list -g <instanceProject>` returning all agents; the per-project fan-out is deleted.
- **Worker subdirs must exist on the host before `start`** (Scion `Stat`s the path). The operator declares them in config; the manager/broker ensures the directory exists before dispatch.
- **Manager drives dispatch** via the host broker, exactly as today, but every call now targets the constant instance project instead of a per-grove project.
- **stop / suspend / resume.** `scion suspend` → `scion start`/`resume` continues the harness (`claude --continue`) because the agent **home** is a host bind-mount outliving the container. This is unchanged; it now applies uniformly to manager and workers in one project.

## 8. Reconciliation and resume (R7)

- **One project registration** to reconcile instead of N. `ScionProjectRegistered` idempotency (skip destructive clean+init when exactly one project-configs entry + in-tree marker exist) applies to the single instance project.
- **Manager resume** is unchanged: on `up`, list the project, find the record whose slug == `<app>`, branch on phase (running → verify live; suspended/stopped → `resume`, restoring the conversation; otherwise loud delete + fresh create).
- **Worker records** live in the same project; reconciliation enumerates and restores/cleans them alongside the manager.
- **Bootstrap re-mint** on resume: the controller PAT persists across `down`→`up`; if invalid, re-run the §6.1 window (agent-free). The broker enrol latch re-arms as today (`RearmBootstrap`).

## 9. Messaging (R8)

Transport stays Scion `message` / `notifications`, but all agents are now in one project, so routing uses the constant `-g <instanceProject>`:

- Manager↔worker and human↔manager route through the host broker (identity-derived from mTLS CN, config-authoritative, default-deny), unchanged except the project is constant.
- `scion message <agent> <body> [--interrupt] -g <instanceProject>`; `scion notifications --json -g <instanceProject>`.
- Worker→worker messaging stays gated by config (`broker.messaging.*`).
- The host-side operator path (`lever msg send`) continues to bypass the broker with operator authority.

## 10. Terminology: grove → worker (folded in)

The `grove` vocabulary is renamed to `worker` as part of this re-architecture (not a separate pre-pass). Scion's own term for the unit is **Agent**; lever's user-facing term is **worker** (a worker *is* a Scion agent). Do **not** rename grove → project (that re-entrenches the old one-project-per-worker structure).

The rename is entangled with identity and must be done with care, not as a blind string replace:

- A grove name currently serves as agent slug, **project name**, ticket subject, and cert CN. Under the new model the project name is the single instance project, so the worker name maps to **agent slug + ticket subject + cert CN only**. This decoupling is part of the change.
- Touch points span `internal/broker` (heaviest — `grove.go`, `msg.go`, `provision.go`, `enrol.go`, `server.go`), `internal/cli`, `internal/config` (the `Grove` struct → `Worker`), `internal/brokerctl`, `internal/apply`, `internal/scion`, `internal/cap` (ticket subject), `internal/backend`, `internal/agent`.
- Config surface: `groves:` → `workers:`; `Grove{Name,Dir,Image,LLMAuth,Obtain,Delegate}` → `Worker{...}` unchanged in shape (a worker still declares a name + a subdir of `tree`).

## 11. Config schema (clean break)

The user-facing config stays close in *shape* but changes in *meaning*: a user declares one instance (`name`, `backend`, `tree`, `manager`, `workers:[]`), and the workers now become agents in one project rather than separate projects.

- `App.Groves` → `App.Workers`; `Grove` → `Worker` (fields unchanged).
- The `dir: "."`-rejection rule (`config.go:483-491`) is removed/rethought: manager (root) and workers (subdirs) now legitimately coexist in one project. Workers still require a subdir (a worker mounting the whole root would defeat R4 sibling isolation), so validation keeps "worker dir must be a proper non-empty subdir of tree", but no longer for project-collision reasons.
- Messaging config keys renamed (`grove_to_grove` → `worker_to_worker`).

## 12. Isolation guarantees and validation gate

Before this design is considered proven end-to-end, the implementation must confirm (from the spike's residual list) via a real `scion start`, not just argv/mount inspection:

- With the PR4 guard, a worker started with `-w <root>/sub` on a **non-git** tree plain-mounts exactly that subdir; `.git` is absent; sibling subdirs are not mount sources.
- A stray ancestor `.git` does **not** pull a worker into git mode (guard holds).
- No `.scion` walk-up: a worker does not get an ancestor `.scion` mounted; the manager's root mount exposes only the harmless externalized marker file.
- The controller PAT with the exact scope list drives `start/stop/suspend/message` (naive `agent:manage` 403s).
- Explicit worker workspaces are never deleted on `scion delete`.
- Concurrent in-place writes by manager (root) and workers (subdirs) are tolerated.
- Suspend/resume restores the manager conversation **and** the worker fleet across `stop`→`up`, idempotently.

These become the acceptance checks for the implementation (extending the existing `lever acceptance` gate).

## 13. Out of scope / deferred

- **Git-workflow trees (Candidate 2).** Running agents against a real git repo with per-worker git and accepting (or patching out via `--plain-workspace`) the committed-history sibling read is **deferred**. This design targets non-git trees.
- **Any Scion source change beyond the two PRs in §5.**
- **New broker capabilities or MCP surface.** The capability broker, enrolment, and external-tool gateway are carried forward as-is.

## 14. Open questions to resolve during planning

1. **Throwaway dev-auth server mechanics.** Exact `scion server start` invocation for an ephemeral host-only instance, how lever detects it is ready, and clean teardown (does killing the process leave state the real hub must not inherit?).
2. **PAT rotation / invalidation UX.** What lever does when the persisted controller PAT is rejected mid-life beyond "fail loud" — surface a specific operator remediation.
3. **Worker record reconciliation detail.** Precisely how the fleet of worker agent records is enumerated and matched to config on `up` (by slug), and the policy for a config-removed worker whose agent record still exists.
4. **Rename sequencing within the plan.** Whether the grove→worker rename lands as the first plan phase (rename on the current structure, then restructure) or interleaved with the model change per package — decided at writing-plans time.

---

## Decomposition for planning

The pieces are tightly coupled (the single-project model drives bootstrap, dispatch, messaging, and reconciliation), so this is **one spec** but the implementation should be **phased** into sequential plans, roughly:

- **P1 — Terminology + config surface.** grove→worker rename (identity-aware) and the config schema change, on the current structure, as a mostly-mechanical but reviewed base.
- **P2 — Single-project model.** Collapse per-agent projects into one instance project: project registration, the Scion client scoping, broker worker-lifecycle, and guest state.
- **P3 — Bootstrap + token plumbing.** Throwaway dev-auth server, controller PAT mint/persist, real hub dev-auth-off, `SCION_HUB_TOKEN` plumbing across apply/attach/msg/stop.
- **P4 — Reconciliation, messaging, and the acceptance gate.** Fleet resume, constant-project messaging, and the §12 checks wired into `lever acceptance`.

Each phase ends with a working, testable increment.
