---
title: "Worker isolation"
nav_order: 5.2
permalink: /security-model/worker-isolation/
---
Part of the [security model](/security-model/). Sections keep their original § numbers.

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

How the subdir mount is delivered: a per-agent **absolute** `--workspace` does not survive Scion's
hub path for a directory project — the hub discards it, so every agent would otherwise fall back to
mounting the whole project root. Worker confinement therefore uses a **project-relative
`--workspace-subdir`** mount with a containment guard (rejecting `..`/symlink escape), which Scion
resolves within the project root and mounts as exactly that subtree. This is a small Scion addition
currently carried on our fork branch `feat/per-agent-workspace-subpath`, **not yet upstreamed and
not in the pinned `scion.version`** — so dispatching workers today requires building Scion from the
fork (`scion.source`). Live-validated 2026-07-10 (worker `scratch` mounted `/lever/workers/scratch`,
not `/lever`).

This guarantee also holds only on a **non-git tree root**: a git repository at the tree root can
pull Scion's mount builder into a worktree branch that also bind-mounts the whole `.git` object
store, through which a worker could read *committed* sibling content. Config validation refuses (or
warns on) a git tree root at load time; the `--workspace-subdir` guard likewise resolves within the
project root regardless of a stray ancestor `.git`. A worker's *own* subdirectory may still contain
its own git repository; that is unaffected.

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
talk to the Scion hub directly. It cannot register a project, request an arbitrary mount, or
list/attach to another agent. All of that is host-side-only, gated by the controller PAT, and, for
dispatch specifically, further gated by the config-declared subdirectory per §5.4.

**Residual.** This closes the isolation gap between workers, and between an agent and the hub
itself. It does not change the manager's own trust position: the manager legitimately mounts the
whole tree (§4.1), so a compromised manager can still read and write everything the instance keeps
there, including the knowledge base and every worker's subdirectory, that is an inherent cost of
giving the manager whole-tree oversight (§7), not a gap in the worker-isolation model above. **Not
yet done:** the live acceptance checks that would exercise this guarantee against a real
`scion start` (sibling subdirectories, a stray ancestor `.git`, the controller PAT's exact scopes)
are not yet wired into `lever acceptance`. The mechanism is implemented and was live-validated once
by hand (2026-07-10), but worker isolation currently depends on the fork-only `--workspace-subdir`
addition (not yet in the pinned commit; see §4.1), and no dedicated automated live gate exists today.
