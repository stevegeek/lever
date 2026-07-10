# Lever single-project re-architecture — design spec

**Status:** implemented (P1–P4) · 2026-07-08, updated 2026-07-10
**Implementation note (2026-07-10) — supersedes the "zero fork" framing wherever it appears (§1, §5 heading + pin recipe, and the `-w <root>/sub` absolute-workspace mechanism in §4.1 and the §12 acceptance line):** the "no lever-specific Scion fork" claim held for the two originally-submitted PRs but did **not** survive the R4 crux (§2). Confining each worker to its subdir needed a *third* Scion change — a project-relative `--workspace-subdir` mount with a containment guard — because Scion's hub path **discards a per-agent absolute `--workspace`** for directory projects (so the `-w <root>/sub` mechanism §4.1/§5/§12 describe does not actually isolate a worker; every agent falls back to the whole root). That feature lives on fork branch `feat/per-agent-workspace-subpath`, **not yet upstreamed**; until it lands, worker isolation is delivered (and was live-validated 2026-07-10) by building Scion from the fork (`scion.source`), not the pinned `scion.version`. This stays consistent with R9 (*minimize, ideally zero* Scion changes) — one small, upstreamable addition, not a maintained fork — but the pin recipe in §5 must additionally include `--workspace-subdir`, and the "zero fork" wording is now aspirational rather than current.
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
| Grove `dir` must be a strict subdir; `dir: "."` rejected to avoid colliding with the manager's project | `config.go:483-491` | Manager (root) and workers (subdirs) legitimately share one project, so the project-**collision rationale** is obsolete — but the **subdir requirement itself is retained** (now load-bearing for R4: a worker mounting root would see the whole tree), as are **both name-collision rules** (now enforcing scion agent-slug uniqueness in the shared project). See §11. |
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
| `ListProjects` cursor pagination | `fix/listprojects-cursor-pagination` | Defensive: correct project enumeration beyond the first page. Only load-bearing on a *shared* hub accumulating >50 projects; a dedicated per-instance hub has exactly one project, so this never triggers for the core model — carried as a correctness fix, not a hard dependency. |
| `--workspace` git guard (`run.go`, honor explicit workspace over git auto-detection) | `fix/explicit-workspace-git-guard` | Makes §4.2 isolation a guarantee, not a layout precaution |

Everything else — mounts, agent lifecycle, project-scoped tokens, per-agent JWT identity, messaging, suspend/resume — is **stock Scion**. The offline `hub bootstrap` command and the `auto_provide` change explored earlier are **retired**: the single-project model eliminates the multi-project token dilemma, and normal `hub link` project creation fires provider auto-linking for free.

lever pins the Scion version it builds against; these two fixes must be present in that pin (via the fork branch or once merged upstream). **The pin bump is an explicit P2 task** (see the decomposition): move `scion.version` to a fork commit containing *both* `fix/explicit-workspace-git-guard` and `fix/listprojects-cursor-pagination`, and verify their presence — R4's isolation guarantee and the §12 acceptance checks cannot pass on a pin lacking the git guard. The current pin `666333f9` predates both.

## 6. Bring-up and bootstrap

### 6.1 Controller token via a throwaway dev-auth server

One project ⇒ one project-scoped PAT drives every agent's lifecycle. The real hub runs **dev-auth off** (hardening: agents never see an admin-open hub). During **agent-free** bring-up:

1. Start a **throwaway** hub: `scion server start --port <random> --dev-auth=true`, host-only, on a random port no agent ever learns.
2. As dev-auth admin against it: `project init` + `hub link` for the instance project, then mint the controller PAT: `scion hub token create --scopes agent:manage,agent:attach,project:read`.
3. Persist the PAT host-side, `0600`, under `.lever-state/`.
4. **Kill the throwaway** and delete the residual `<scionDir>/dev-token` file it wrote (an inert `0600` admin bearer while the real hub runs dev-auth-off, but a dormant credential any future dev-auth hub on that dir would honor).
5. Start the real hub: `scion server start --port 8080 --dev-auth=false`. **The throwaway and the real hub must share the same hub DB** (`cfg.Database.URL`): the project and the controller PAT are rows in that sqlite DB, so a differing data dir would make the minted project + PAT invisible to the real hub and break bring-up.
6. Only then dispatch the manager (and, later, workers).

**Controller token scope is exact and load-bearing:** `agent:manage,agent:attach,project:read`. `agent:attach` is the pivotal scope: the `agent:manage` alias expands to `{create,read,list,start,stop,delete,dispatch}` and does **not** include `attach`, yet every interactive verb — `start`/`stop`/`suspend`/`resume`/`attach` **and `message`** — is gated on `agent:attach` for user tokens (verified in scion authz), so a naive `agent:manage`-only PAT 403s on `start`. `agent:message` is therefore **not required and is omitted**; should a future scion pin ever gate the `message` verb on `agent:message` specifically, add it back.

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
- **Worker subdirs must exist on the host before `start`** (Scion `Stat`s the path). The operator declares them in config; the manager/broker ensures the directory exists before dispatch. **Owner: P2** (the dispatch-path change).
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
- The `dir: "."`-rejection rule (`config.go:483-491`) is **kept, with its rationale rewritten**: manager (root) and workers (subdirs) now legitimately coexist in one project, so the *project-collision* reason is gone — but the **subdir requirement itself stays**, now load-bearing for R4 (a worker mounting the whole root would defeat sibling isolation), and validation keeps "worker dir must be a proper non-empty subdir of tree". **Both name-collision rules also stay** (they now enforce scion agent-slug uniqueness in the shared project, not project-name collision). P2 must **rewrite the stale `config.go:485` comment** (which today justifies the rule via the per-worker-project register step that P2 deletes) — reword the reasoning, do **not** delete the rule.
- **Non-git tree is enforced, not assumed.** R4's isolation holds only on a non-git tree (§4.2); P2 adds a **config-time guard** that refuses (or loudly warns) when `tree` or an ancestor is a git repo, rather than leaving it to operator discipline.
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
- **P2 — Single-project model.** Collapse per-agent projects into one instance project: project registration, the Scion client scoping, broker worker-lifecycle, and guest state. Also in P2: **bump the `scion.version` pin** to a commit carrying both §5 fixes (and verify their presence); add the **config-time non-git guard** (§11); **rewrite the stale `config.go:485` comment**. P2's intermediate **stays bootable on the current dev-auth-open loopback hub** — the controller PAT + dev-auth-off is strictly P3 — so P2 lands as a working increment.
- **P3 — Bootstrap + token plumbing.** Throwaway dev-auth server, controller PAT mint/persist, real hub dev-auth-off, `SCION_HUB_TOKEN` plumbing across apply/attach/msg/stop.
- **P4 — Reconciliation, messaging, and the acceptance gate.** Fleet resume, constant-project messaging, and the §12 checks wired into `lever acceptance`.

Each phase ends with a working, testable increment.

---

## 15. Implementation status (2026-07-08)

The re-architecture **model code is implemented and merged to `main`** across all four phases (P1-P4 shipped). Of the two items that were gated on live infrastructure: the scion version pin is resolved and dev-auth-off is live-validated (gated final #1, below); the §12 live acceptance gate remains open, and a related isolation gap has since resurfaced (gated final #2, below).

- **P1 — grove→worker rename + config schema.** ✅ merged. Reviewed, tests green.
- **P2 — single-project model.** ✅ merged. One instance project (`register-project`); broker uses a constant `instanceProject` for `-g` + per-worker subdir `--workspace`; list collapsed; config non-git guard (narrowed to *tree-itself* — an ancestor `.git` is harmless under the workspace guard); `config.go:485` comment rewritten. Reviewed, `-race` green.
- **P3 — bootstrap + controller PAT + dev-auth-off.** ✅ merged. Throwaway `--dev-auth=true` server mints a scoped controller PAT (`agent:manage,agent:attach,project:read` — `agent:message` dropped; attach gates it), persisted `0600` under `.lever-state/` (survives `down`→`up`); real hub `--dev-auth=false`; `SCION_HUB_TOKEN` threaded into all five scion clients incl. attach. Reviewed, `-race` green.
- **P4 — reconciliation, messaging, acceptance gate.** §8/§9 were **largely delivered inside P2/P3** (single registration, observe-first manager resume, persistent idempotent PAT, constant-`-g` routing + lifecycle, `worker_to_worker` gate). Remaining work + decisions:

**Resolved open questions:**
- **§14 Q3 (worker-record reconciliation) — RESOLVED as: workers are lazy, manager-dispatched.** There is no apply-time worker-fleet start/restore; `up` reconciles only the manager (which re-dispatches workers on demand through the broker). Worker agent records are therefore NOT auto-pruned on `up` — pruning a record the manager intends to resume would be destructive. A worker removed from config whose scion record still lingers is left for manual `scion delete` (or a future opt-in prune); not auto-cleaned in P4. This ratifies the P2 §7 "workers not started at apply time" model.
- **§9 residual fixed:** `attachTarget` (`internal/cli/attach.go`) returned the old per-worker project (`/lever/workers/<name>`) for workers, mis-scoping `lever msg send --to <worker>` and `lever attach <worker>`. Fixed to return the instance project (`mountDest`), addressing the worker by slug — matching the single-project model.
- **§14 Q2 (PAT rotation):** an invalid persisted PAT fails loud at first hub call (acceptable per spec); beyond-fail-loud remediation remains deferred.

**Gated finals (need live infrastructure, not code):**
1. **Scion version pin — RESOLVED (2026-07-09).** The combined fix landed upstream via PR #652 (merged to `GoogleCloudPlatform/scion` 2026-07-09); `scion.version` is pinned to `80777eaf`, carrying both required fixes. Real-hub `--dev-auth=false` (controller-PAT-driven, no dev-auth server) was live-validated end-to-end 2026-07-10.
2. **§12 acceptance gate (live) — still open.** All seven §12 checks are inherently live (real `scion start` + agent containers + the dev-auth-off hub + PAT); today's `lever acceptance` runs broker-only (scion skipped). Wiring them means extending the harness to a full bring-up + a scion client + the controller PAT and extending the fixture (sibling subdirs, a stray ancestor `.git`, a multi-worker fleet); this is no longer blocked by the pin (resolved above) but has not yet been done.
   **Note (2026-07-10): the §12 worker-subtree-isolation guarantee itself is not fully closed.** The grove-mount bug (a worker mounting the wrong tree) has resurfaced under the single-project model; a scion-side fix (`WorkspaceSubdir`, branch `feat/per-agent-workspace-subpath`) is in progress upstream to close it before this gate can be wired and pass.
