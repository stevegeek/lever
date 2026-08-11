---
title: "Worker isolation"
nav_order: 5.2
parent: Security model
permalink: /security-model/worker-isolation/
---
Part of the [security model](/security-model/). Sections keep their original § numbers.

## 4. Within the jail: cross-worker isolation (defence in depth)

The jail ([§2](/security-model/jail/)-[§3](/security-model/)) protects host secrets and the LAN. *Within* the jail, the manager and every worker
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

How the subdir mount is delivered: worker confinement uses a **project-relative `--workspace`**
mount with a containment guard (rejecting `..`/symlink escape), which Scion resolves within the
project root and mounts as exactly that subtree (an absolute `--workspace` instead mounts that
exact host path — Scion branches on `filepath.IsAbs`). Merged upstream in
[scion#815](https://github.com/GoogleCloudPlatform/scion/pull/815); requires a `scion.version`
pin of `b4c9911d` or later (the shipped examples pin it). Live-validated 2026-07-10 on the
pre-merge fork implementation of the same guard (worker `scratch` mounted
`/lever/workers/scratch`, not `/lever`); the upstream implementation has not yet been
live-revalidated.

This guarantee also holds only on a **non-git tree root**: a git repository at the tree root can
pull Scion's mount builder into a worktree branch that also bind-mounts the whole `.git` object
store, through which a worker could read *committed* sibling content. Config validation refuses (or
warns on) a git tree root at load time; the relative-`--workspace` guard likewise resolves within the
project root regardless of a stray ancestor `.git`. A worker's *own* subdirectory may still contain
its own git repository; that is unaffected.

**The manager still sees everything, by design.** Because the manager's mount is the whole tree,
and Scion does not shadow child workspace dirs inside a broader mount, the manager's live view
legitimately includes every worker's in-place edits — the same "mount only your own workspace"
mechanism, viewed at the manager's wider scope. A compromised *manager* therefore still has
whole-tree reach ([§7](/security-model/compromise/)); this isolation guarantee is about one *worker* reaching another worker's
subdirectory, not about bounding the manager.

**Scion's default shared directory is removed, because it would defeat this.** Since
[scion#925](https://github.com/GoogleCloudPlatform/scion/pull/925), the hub stamps a `scratchpad`
shared directory on every new project and mounts it **read-write into every agent of that project**,
at `/scion-volumes/scratchpad`. That is a writable channel between the manager and every worker, and
between any two workers — exactly the reach §4.1 denies. So `lever apply` removes the directory from
the hub's project record after registration, on both the fresh and the already-registered path,
then **re-reads the record to confirm it is gone**
— the hub answers `404` for "no such shared dir", "no such project" and "no such route" alike, so
the delete status on its own cannot prove the removal happened. A failure at either step **fails
the bring-up** rather than starting a fleet that silently shares a directory. The request runs
inside the jail, like every other Scion interaction: the hub binds the jail's loopback, and the
Lima template suppresses guest→host forwarding on purpose, so a host-side call could not reach it
there at all.

**A hub-wide opt-out now exists upstream, and it does not retire the removal.** When lever first
shipped the removal there was no way to switch the default off at all in file/SQLite mode: the
setting lived in Scion's operational settings, which are Postgres-gated, and the admin endpoint
answered `501`. In response to lever's request
([scion#1098](https://github.com/GoogleCloudPlatform/scion/issues/1098)),
[scion#1103](https://github.com/GoogleCloudPlatform/scion/pull/1103) added a `settings.yaml` key
that works in file/SQLite mode:

```yaml
project_defaults:
  default_scratchpad: false
```

It acts **at project creation time**. It stops the hub stamping the directory on a *new* project;
it does not remove one from a project the hub already records. So it narrows what the removal has
to do, and never replaces it — any instance registered before the key was set still needs the
delete, and lever still verifies the record either way. Two caveats if you set it yourself: Scion
reads the top-level `project_defaults` section only when the same `settings.yaml` also carries a
top-level `server:` section, and ignores it silently otherwise; and at the time of writing the
commit carrying the key cannot be fetched through the Go module proxy, so `scion.version` cannot
reach it — only [`scion.source` or `scion.binary`](/reference/config/) can.

`lever doctor` reports any shared directory the hub still records for the project. That check reads
the hub record, so it describes newly started agents: an agent that started before the removal
keeps its bind mount until it restarts.

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
- **Minted once and reused verbatim.** The PAT persists across `stop`→`up`, and `lever apply`
  short-circuits on any PAT already on disk: it is never validated against the hub, and nothing
  re-mints it. Only `lever destroy` clears it. So a PAT the hub no longer accepts — after the hub
  database is reset, after the jail is deleted out of band rather than with `lever destroy`, or
  after a lever upgrade widened the scope set — is a **hard bring-up failure** the operator has to
  resolve, by clearing `.lever-state/controller.pat` (which re-runs the agent-free mint window on
  the next apply) or by destroying the instance. lever does not re-enable dev-auth on a hub that
  already has agents to recover automatically, and re-minting is not attempted behind your back.
- **Injected only into lever's own host-side Scion client calls**, as the `SCION_HUB_TOKEN`
  environment variable, by the capability broker and by `lever attach`/`lever msg`/`lever stop`.

The result: even a fully compromised worker or manager container has no credential that lets it
talk to the Scion hub directly. It cannot register a project, request an arbitrary mount, or
list/attach to another agent. All of that is host-side-only, gated by the controller PAT, and, for
dispatch specifically, further gated by the config-declared subdirectory per [§5.4](/security-model/config-trust/).

**Residual.** This closes the isolation gap between workers, and between an agent and the hub
itself. It does not change the manager's own trust position: the manager legitimately mounts the
whole tree (§4.1), so a compromised manager can still read and write everything the instance keeps
there, including the knowledge base and every worker's subdirectory, that is an inherent cost of
giving the manager whole-tree oversight ([§7](/security-model/compromise/)), not a gap in the worker-isolation model above. **Not
yet done:** the live acceptance checks that would exercise this guarantee against a real
`scion start` (sibling subdirectories, a stray ancestor `.git`, the controller PAT's exact scopes)
are not yet wired into `lever acceptance`. The mechanism is implemented and was live-validated once
by hand (2026-07-10, on the pre-merge fork implementation — the guard has since merged upstream as
Scion's relative `--workspace`, see §4.1), and no dedicated automated live gate exists today.

### 4.3 Per-agent network namespace: a private loopback per agent

Mount and hub-authority isolation would still leave a network gap if all agents shared one network
namespace. Each agent runs an in-container **gateway proxy** on `127.0.0.1:8462`
([agent identity](/agent-identity/)) that holds that agent's mTLS client leaf and is itself
unauthenticated — it trusts whoever connects on its loopback. If agents shared the jail's network
namespace (`--network=host`), that loopback would be jail-wide: a compromised worker could `POST` to
the manager's `127.0.0.1:8462` and be authenticated to the broker **as the manager**, with no
credential — full cross-agent impersonation.

Lever closes this by giving every agent its **own** network namespace. Agents run under rootless
podman's default **pasta** networking (lever no longer forces `--network=host`), so each container's
`127.0.0.1` is private to that container; one agent cannot reach another's gateway proxy at all
(live-validated: a second container's `curl` to a co-resident agent's `127.0.0.1:8462` is refused).

Two things that host networking used to provide are preserved without it:

- **Hub reachability.** The agent's Scion runtime connects to the jail-local hub on loopback. With a
  private netns the hub is no longer directly reachable, so lever stages a pasta
  `--map-host-loopback` option (guest `containers.conf.d`) mapping the address podman already resolves
  `host.containers.internal` to; Scion's auto-computed container hub endpoint
  (`host.containers.internal:PORT` for podman) then reaches the jail-loopback hub across the netns
  boundary. The mapping exposes the jail's loopback to each agent — the same surface `--network=host`
  already gave them, now minus the per-agent gateways that host networking made jail-wide. Nothing
  unauthenticated should bind the jail loopback beyond the hub and the allowlisted local tools; the
  broker's control surfaces are host-side, not on the jail loopback.
- **Egress containment.** The allowlist is enforced on the jail's `OUTPUT` chain. pasta's userspace
  egress re-emerges as `OUTPUT` traffic in the jail netns, so it still traverses `LEVER_EGRESS`
  unchanged — a per-agent bridge/`FORWARD` topology would have bypassed it, but pasta does not
  (live-validated: private-range probes from inside an agent netns still hit the chain's DROP rules).

Escape hatch: setting `LEVER_FORCE_HOST_NETWORK=1` on the host restores `--network=host` for
debugging — this reopens the shared-loopback gap above and is not isolation-safe.
