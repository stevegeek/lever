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
([architecture.md §2](/architecture/)).
Two structural properties bound what one agent can reach inside that shared project.

### 4.1 Defense by absence: a worker only ever mounts its own subdirectory

Each agent's container bind-mounts **only its own configured workspace path**. The manager's
container mounts the whole tree root; each worker's container mounts exactly the one subdirectory
declared for it in config, in place. Scion never enumerates sibling agents to build deny/shadow
mounts, so a sibling worker's subdirectory is simply **not a mount source** for a given worker's
container: it is unreadable at the kernel/VM boundary, not merely hidden by convention or file
permission (container UIDs are synced to the host UID, so file permissions alone give no
inter-agent isolation here).

Config validation also rejects two workers whose `dir`s overlap (the same subtree, or one an
ancestor of the other; `validateWorkerDirsDisjoint`, `internal/config/validate.go`), since the outer
worker would otherwise mount the inner one's whole workspace. A worker named `manager` is rejected separately: the
operator-directive channel resolves that literal name to the manager regardless of
`broker.manager_identity`, so such a worker could never be addressed.

How the subdir mount is delivered: worker confinement uses a **project-relative `--workspace`**
mount with a containment guard (rejecting `..`/symlink escape), which Scion resolves within the
project root and mounts as exactly that subtree (an absolute `--workspace` instead mounts that
exact host path — Scion branches on `filepath.IsAbs`). The shipped examples pin a `scion.version`
that supports this; an unsupported pin fails at `lever apply` with an explanatory error.

This guarantee also holds only on a **non-git tree root**: a git repository at the tree root can
pull Scion's mount builder into a worktree branch that also bind-mounts the whole `.git` object
store, through which a worker could read *committed* sibling content. Config validation refuses a git
tree root at load time (`checkNonGitTree`, `internal/config/host.go`); it checks only the tree
itself, not ancestor directories. The relative-`--workspace` guard likewise resolves within the
project root regardless of a stray ancestor `.git`. A worker's *own* subdirectory may still contain
its own git repository; that is unaffected.

The one opt-in exception is a **shared folder** (`shared_folders`, [§4.5](#45-opt-in-shared-folders)):
a tree directory the config names, mounted into the agents it lists and no others.

**The manager still sees everything, by design.** Because the manager's mount is the whole tree,
and Scion does not shadow child workspace dirs inside a broader mount, the manager's live view
legitimately includes every worker's in-place edits — the same "mount only your own workspace"
mechanism, viewed at the manager's wider scope. A compromised *manager* therefore still has
whole-tree reach ([§7](/security-model/compromise/)); this isolation guarantee is about one *worker* reaching another worker's
subdirectory, not about bounding the manager.

**A worker's enrolment ticket never enters the tree.** Until 0.22 a worker's single-use ticket
was staged at `<tree>/<dir>/.lever/bootstrap.json` — inside the manager's whole-tree mount — and
the jail listener's `/provision` route minted a fresh one for any declared worker to the manager.
Between staging and the worker's boot the ticket was unspent, and `/enrol` signs any CSR whose CN
matches the ticket, so a manager that redeemed it first enrolled *as* that worker and gained its
`obtain:` grants, its inbox and any operator directive targeted at it. The broker now stages a
worker's ticket in the **guest**, under the run user's `XDG_RUNTIME_DIR`
(`/run/user/<uid>/lever/tickets/<worker>/bootstrap.json`, 0700 directories, 0600 file, a tmpfs no
container mounts by default), and starts the worker's container with exactly that directory as a
read-only volume at `/run/lever`. The ticket is therefore visible to one container, the worker's
own; the manager's mount does not contain it and no route an agent can reach mints one
(`/provision` is gone; the host-side `/worker-ticket` on the loopback admin listener replaces it
for the acceptance harness). Every start and resume re-stages a fresh ticket, so the tmpfs being
emptied on a VM reboot costs nothing. A worker created before 0.22 has no `/run/lever` mount and
cannot re-enrol; `lever doctor` names it and the fix is `lever worker purge NAME --force` followed
by a re-dispatch.

**Scion's default shared directory is removed, because it would defeat this.** The hub stamps a
`scratchpad` shared directory on every new project and mounts it **read-write into every agent of that project**,
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

**A hub-wide opt-out exists, and it does not replace the removal.** Scion's `settings.yaml` accepts:

```yaml
project_defaults:
  default_scratchpad: false
```

It acts **at project creation time**: it stops the hub stamping the directory on a *new* project,
and it does not remove one from a project the hub already records. lever does not set this key;
it performs and verifies the removal either way.

`lever doctor` reports any shared directory the hub still records for the project. That check reads
the hub record, so it describes newly started agents: an agent that started before the removal
keeps its bind mount until it restarts.

### 4.2 No agent holds hub authority: proxy auth with a host-held key, host-only controller PAT

Cross-worker mount isolation would still be moot if a compromised agent could simply ask Scion's
hub to attach an arbitrary mount, or start a new agent, itself. Scion's **development auth** mode
(a built-in convenience that issues a shared, admin-equivalent bearer token to any caller) would let
it do exactly that. Lever never runs it: the Scion hub inside the jail starts with
**`--dev-auth=false`**, always, and no agent, manager or worker, is handed any hub credential
beyond scion's own per-agent token.

**How lever itself is a hub user.** The hub runs in scion's **proxy auth mode** with its JWT
provider: it trusts a short-lived assertion that lever signs on the host, and nothing else, as an
interactive sign-in.

- **The key stays on the host.** Lever holds an ES256 (P-256) private key in
  `.lever-state/hub-auth.key` (`0600`, directory `0700`). The hub reads only the public half, from
  `/etc/lever/hub-auth.pub` in the jail: root-owned, `0644`, in a root-owned `0755` directory outside
  every home, written by an atomic rename. No process in the jail can sign an assertion, and an
  agent cannot replace the key the hub reads.
- **One assertion per request, never stored.** Each carries `iss` = `lever:<instance-id>`,
  `aud` = `scion-hub:<instance-id>` (scion checks either only when it is configured, so lever always
  sets both), a lifetime of 30 seconds and an `iat` 5 seconds in the past, signed against the
  guest's clock. Scion does not remember a used assertion (it ignores `jti`) and adds 30 seconds of
  skew, so the short lifetime is what bounds a replay. It travels in the `X-Lever-Hub-Assertion`
  header over lever's existing jail transports, on the child's stdin, never in host or guest argv
  and never in an environment: the jail-side shell reads it into an unexported variable and hands
  it to `curl` on a pipe. `curl` sends it only for the exact method and path of each call lever
  makes (no other route, no other method, no agent port-proxy path), and never with an
  `Authorization` header (scion tries the assertion only on a request with no bearer token).
- **Pinned through the environment.** Lever starts the hub with the auth settings as `SCION_SERVER_*`
  variables (`AUTH_MODE=proxy`, `AUTH_USERACCESSMODE=invite_only`, `AUTH_PROXY_PROVIDER=jwt`, the
  JWT algorithm, header, issuer, audience and `AUTH_PROXY_JWT_PUBLICKEYFILE`, and
  `HUB_ADMINEMAILS=lever-admin@lever.local`), which override the settings file. It also writes the
  same keys to the jail's `~/.scion/settings.yaml` for readers, and removes any `oidc_login` block
  (scion builds its OAuth login whenever that block is present, whatever the auth mode). An agent
  that rewrites `settings.yaml` through scion's workstation routes therefore changes nothing the hub
  enforces, and causes no restart: the scion-server step restarts the hub only when the running
  hub's env, its `--dev-auth=false` or its web-assets argv differ from what lever starts it with, or
  when the public key file changed or an `oidc_login` block was removed.
- **`invite_only`.** The hub admits only `lever-admin@lever.local` and users that exist (invited or
  active). An assertion for any other address is refused; `lever doctor` proves it by signing one
  for a random address.

**The identities.**

| Identity | Scion role | Holds | Used for |
|---|---|---|---|
| `lever-admin@lever.local` | admin, through `admin_emails` | no credential at rest | governance as the host CLI: roles, bindings, ceilings, invites, suspensions |
| `lever-controller@lever.local` | member; project owner and `lever-controller` on the project | the controller PAT | minting and revoking its own PATs; owns the project and every agent |
| each remote login's hub user | member; `lever-remote` or `lever-remote-contact`, plus a ceiling | nothing (the proxy keeps the session host-side) | the remote web UI, see [remote access](/remote-access/) |

The `hub-identity` apply step converges these on the live hub on every `lever apply`, `lever up` and
`lever reload`. It never lists or stops containers and runs while agents run. It demotes every admin
other than `lever-admin`, by role and by super-admin binding. On an instance an older lever brought
up, the controller is scion's dev user (the row an older lever minted its tokens as), renamed to
`lever-controller@lever.local` and demoted to member; it keeps the project, the PAT and every agent.

**Workstation routes.** Lever runs the hub non-hosted, so scion's `/api/v1/system/*` routes accept
any authenticated loopback caller, an agent token included. Before scion `e8160a10`, one of them
renamed scion's dev user; from `e8160a10` on that route (and `workstation-settings`, which can turn
on gcloud credential injection) needs `hub.config.update`, which an agent token does not hold, and
the stack e2e proves an agent's writes to both refused. The registry and runtime routes still take
an agent token; lever sets both back as `lever-admin` at every apply. On a fresh instance there is
no dev user, so the rename is a no-op. On a migrated one, with an older scion, an agent can rename
the controller, between applies or in a loop. Lever therefore identifies the controller by user ID,
through its PAT, and needs its address only to mint or revoke a controller token; then it re-pins
the address and asserts the controller together, a bounded number of times, and fails naming the
cause if a loop keeps winning. The next apply re-pins the address, and if the agent had taken
`lever-admin@lever.local` for it, lever restores the controller's address with the controller PAT
and waits out the hub's 60-second email cache before it asserts the admin again. None of this can
hold up a revocation: a removed remote login is refused at the proxy before any apply step, and
suspended at the hub right after lever-admin is asserted. The proxy also refuses a remote login
whose hub user is not the one lever recorded, so a rename costs at most the login it aims at. See
[credentials](/security-model/credentials/) for the controller's sender label.

**Why the controller stays scion's dev user.** On a migrated instance that row owns the project,
created every agent and is the root of every agent's attested ancestry, and scion grants an agent's
owner and ancestry by user ID. Scion can transfer a project's ownership but not an agent's, so
moving the controller to a fresh, unrenamable user would cut every existing agent off from it. The
rename route is contained instead, as above.

Every Scion lifecycle call (start/stop/suspend/resume/message — issued by the host-side
capability broker on the manager's behalf, and by `lever` itself for attach/msg/stop) is
authenticated with a **controller PAT**: a Scion hub token scoped to exactly
`agent:manage,agent:attach,agent:message,project:read,project:update`, the read-only
`skill:read,skill:list,template:read,template:list,harness_config:read,harness_config:list`,
plus `agent:lifecycle` and `inbox:read` when the jail's scion knows each scope (scion `f7155ecb`
and later gate start, stop, suspend and resume of an existing agent on `agent:lifecycle`; newer
scions gate listing notifications on `inbox:read`; an older hub rejects an unknown scope). A
newer hub gives an agent a token creates or resumes only a role the token's scopes cover, and the
baseline role's `project:read` covers the skill, template and harness-config reads, so without
them the PAT could not start or resume a baseline agent (`agent:attach` is
load-bearing — the `agent:manage` alias alone 403s on `start`, since scion gates the lifecycle
actions on `agent:attach`; `agent:message` is what newer scions gate a token's message sends on,
and the alias did not expand to it when older tokens were minted; `project:update` is required
because the shared-dirs endpoint used for the scratchpad removal in §4.1 and the project-settings
route used for the role ceiling in §4.4 both gate on project update, and `agent:manage` does not
expand to it). A PAT acts as its scopes on top of its issuer's live bindings, so the controller's
member role plus project ownership and `lever-controller` is all it can ever do. It is:

- **Minted by the controller, while agents run.** `lever apply` asserts
  `lever-controller@lever.local` and asks the hub for the token (`POST /api/v1/auth/tokens`). A PAT
  can neither mint another token nor invite a user.
- **Persisted host-side only**, `0600`, under `.lever-state/` — never written into the mounted
  tree, never set as a container environment variable or Scion hub secret, so there is no path by
  which an agent inside the jail can read it.
- **Recorded, and re-minted only for a reason.** Beside each token lever keeps a record
  (`controller.pat.json`) of the scopes it asked for, the scopes the hub granted, and the expiry.
  Tokens are minted for 360 days (scion's maximum is a year), and `lever apply` re-mints only when
  the record says it must: no token on disk, no record (a token minted by a lever older than
  records, whose scopes and expiry nobody can vouch for), a scope set that differs from the one this
  lever mints, or fewer than 30 days of life left. A re-mint saves the new token and record first,
  then revokes the superseded token by the id the record kept (best effort, reported if it fails).
  The broker reads the PAT file on every use, and a hub `401` on a call made with a token the file
  no longer holds is retried once with the new one, so a rotation needs no broker restart. An open
  `lever attach` keeps streaming across a rotation; a new attach uses the new token. `lever doctor`'s
  `hub tokens` row applies the same test and names the reason, and its `hub identities` row fails
  if the controller holds a token lever did not record. Every apply then revokes, as the
  controller, each of the controller's tokens that lever neither recorded nor holds (PATs an older
  lever minted before it kept records), and prints their ids. It tells its own tokens by the
  records and by the prefix the hub lists of each token against the secrets it holds, never revokes
  one it holds or a `lever ssh` token still in its life, and revokes nothing when it cannot read its
  own records, when the controller assertion does not resolve to the recorded controller, or when
  the hub's list does not show the controller token lever holds. Nothing else touches the token: it
  persists across `stop`→`up`, and only `lever destroy` clears it.
- **Tied to one hub database.** A hub whose database is new (the VM recreated out of band, the
  database moved aside) no longer lists the `lever-controller` role whose ID lever recorded. Lever
  re-bootstraps on its own only on positive evidence: that role is absent from a role list the hub
  answered, **and** `lever-admin`'s row was created by this run. Anything less fails closed and names
  the repair, `lever up --reset-hub-identity`.
- **Injected only into lever's own host-side Scion client calls**, as the `SCION_HUB_TOKEN`
  environment variable, by the capability broker and by `lever attach`/`lever msg`/`lever stop`.
  It is handed to the guest on the child's stdin (or, for the interactive attach, through a 0600
  file under the run user's `XDG_RUNTIME_DIR`), never on the host command line, so `ps` on the
  host does not show it.
- **Not the only host-side PAT.** A second, read-only host-side PAT
  (`agent:read,agent:list,project:read`) is minted by the controller in the same way when
  `remote.enabled: true`, and revoked when it is turned off; it is also stored under `.lever-state/`
  and never enters the jail. The remote proxy sends it only on GETs of the agent list (the chat
  page and the contact fence); every remote request a person makes rides that person's own hub
  session. See [remote access](/remote-access/#the-remote-pat).

Two things need more than the controller's authority, and `lever-admin` does them:

- **The runtime broker link.** scion links a runtime broker to a project only for a caller allowed
  to update that broker (its owner or a super-admin), which no hub token can be, so the controller
  PAT's hub link links none and no agent could start. scion links its co-located broker itself
  when hub-identity registers the project; before every hub link and on every apply, `lever apply`
  checks with the controller PAT that the project lists a broker, and when it lists none it links
  the jail's own broker as `lever-admin` (`POST /api/v1/projects/<id>/providers`, with the tree's
  jail path), refusing when the hub lists any number of brokers but one.
- **Four scion experiments kept off.** On a scion with hub-wide experiments, every apply's
  hub-identity step writes explicit `false` overrides for `hub.conduit` (an inbound shell, sftp and
  port path into every agent, reaching the jail loopback and any Unix socket), `hub.artifacts`
  (hub-held publishing with unauthenticated share links), `hub.chat_thread_groups` (project-wide
  chat groups that agents can create and rename, and that project members with read access see) and
  `federation.project_visitors` (users of external issuers acting in projects, and credentials for
  peer hubs), as `lever-admin` through scion's admin experiments route. It never writes `true`,
  except for `hub.conduit` when the operator opts into [`lever ssh`](/ssh/) on a scion with the
  hub's conduit controls, and only after reading lever's conduit policy back from the running hub
  (lever's gateway port denied to every tunnel and forward, SSH socket forwarding off); it leaves
  every other override as it is. All four are off by scion's default; the explicit override
  keeps them off if a later scion changes the default. lever records what it wrote, and which
  experiments it looked for, in `.lever-state/hub-experiments.json`; on a hub without the route (an
  older scion) it records "not applicable". Before and after the write lever asks the hub whether
  one is on: `GET /api/v1/conduit/grant-keys`, `GET /api/v1/artifacts?mine=1` and
  `GET /api/v1/thread-groups` answer 404 exactly while their experiment is off. The federation
  experiment has no such route (its one route is `POST /api/v1/auth/federation-token`, which also
  needs the hub's OIDC provider, which lever does not configure), so the admin route's answer after the write
  is the proof. One found on is pinned off and the hub restarted, because conduit's relay starts
  only with the hub; one still on after that fails the apply. scion has no API to write the
  artifacts section's own `enabled` switch, so the experiment is the one lever pins. `lever
  doctor`'s `scion conduit and artifacts` row fails while one with a probe route is on, and warns
  while they are off only by scion's default or when an older lever wrote the record and never
  looked for one lever pins now. The remote proxy refuses the thread-group routes for every login,
  and the federation token route as a credential mint.
- **Scion `d34ffbc1`'s new experiments are kept off the same way.** `hub.triggers` (webhook
  endpoints and triggers agents can author, an unauthenticated signed ingress at
  `/api/v1/hooks/<id>` that messages agents, and actions that start new agents outside the broker;
  `GET /api/v1/hooks/<zero id>` answers 404 exactly while it is off), `hub.github_triggers`,
  `hub.exec_schedules` (commands run in an agent's container from a schedule or trigger, with no
  LLM turn, once conduit is on), `hub.exec_containers` (user-started shell containers in the
  jail), `hub.system_one` (model-provider calls from the hub process), `hub.code_span_mentions` (an
  inline code span such as `` `@name` `` becomes a mention, which would route a contact's message to
  another agent), `hub.agent_secrets` (secrets an agent writes for itself, delivered above every
  secret lever sets, and forms that ask a user to type a secret in), `lifecycle.idle_suspend` (the
  hub suspends idle agents on its own schedule and wakes a hub-suspended one for any sender, past
  the broker's wake checks) and `hub.suspend_screen` (the agent's last terminal screen stored in the
  hub database at every hub-run suspend; this one is on by scion's default, so lever's pin turns it
  off). It also pins off `hub.message_policies`, `web.decisions`, `hub.runtime_orphan_reconcile`
  (the hub removing containers it takes for orphans), `hub.reincarnate_harness_switch` and
  `hub.custom_default_user_role`. The remote proxy also refuses the trigger and secret-request routes
  and the other new routes behind these experiments.

`lever doctor`'s `hub authentication` row tests the effective configuration by behaviour, since
file contents cannot prove it: an assertion signed by another key and one for another instance get
`401`, `lever-admin` is an admin, the hub offers no OAuth or OIDC login, the running hub carries
lever's auth env and no dev auth, no scion dev token exists in the jail, and the jail's public key
is the host key's.

The result: even a fully compromised worker or manager container has no hub credential beyond
scion's own per-agent token, which grants none of the following. It cannot register a project,
request an arbitrary mount, or list/attach to another agent. All of that is host-side-only, gated by the controller PAT, and, for
dispatch specifically, further gated by the config-declared subdirectory per [§5.4](/security-model/config-trust/).

### 4.3 Per-agent network namespace: a private loopback per agent

Mount and hub-authority isolation would still leave a network gap if all agents shared one network
namespace. Each agent runs an in-container **gateway proxy** on `127.0.0.1:8462`
([agent identity](/agent-identity/)) that holds that agent's mTLS client leaf and is itself
unauthenticated — it trusts whoever connects on its loopback. If agents shared the jail's network
namespace (`--network=host`), that loopback would be jail-wide: a compromised worker could `POST` to
the manager's `127.0.0.1:8462` and be authenticated to the broker **as the manager**, with no
credential — full cross-agent impersonation.

Lever closes this by giving every agent its **own** network namespace. Agents run under rootless
podman's **pasta** networking (lever does not pass `--network=host`, and its drop-in sets
`default_rootless_network_cmd = "pasta"`, since podman 4.9, on a VM created on Ubuntu 24.04,
otherwise uses slirp4netns), so each container's `127.0.0.1` is private to that container; one agent cannot reach another's gateway proxy at all
(a second container's `curl` to a co-resident agent's `127.0.0.1:8462` is refused).

Two properties a shared namespace would give for free are preserved without it:

- **Hub reachability.** The agent's Scion runtime connects to the jail-local hub on loopback. With a
  private netns the hub is not directly reachable, so lever maps `169.254.1.2` to the jail's
  loopback in a guest `containers.conf.d` drop-in and has podman resolve `host.containers.internal`
  to it. A pasta with `--map-host-loopback` (passt 2024-08 and later, so Ubuntu 26.04's) gets that
  flag; an older one (Ubuntu 24.04's passt, on a VM created on 24.04, which rejects the flag) gets
  a link-local container address with `169.254.1.2` as its gateway, which pasta maps to the host
  loopback (`--map-gw`). `lever apply`
  refuses a guest whose pasta can do neither, and `lever doctor`'s **agent network** row fails on
  any agent container not running pasta. Scion's auto-computed container hub endpoint
  (`host.containers.internal:PORT` for podman) then reaches the jail-loopback hub across the netns
  boundary. The mapping exposes the jail's loopback to each agent, minus the per-agent gateways,
  which stay private to each container. Nothing
  unauthenticated should bind the jail loopback beyond the hub and the allowlisted local tools; the
  broker's control surfaces are host-side, not on the jail loopback.
- **Egress containment.** The allowlist is enforced on the jail's `OUTPUT` chain. pasta's userspace
  egress re-emerges as `OUTPUT` traffic in the jail netns, so it still traverses `LEVER_EGRESS`
  unchanged — a per-agent bridge/`FORWARD` topology would bypass it, but pasta does not
  (private-range probes from inside an agent netns hit the chain's DROP rules).

Escape hatch: setting `LEVER_FORCE_HOST_NETWORK=1` on the host restores `--network=host` for
debugging — this reopens the shared-loopback gap above and is not isolation-safe.

### 4.3.1 The hub's workstation routes

Because agents reach the jail-loopback hub (§4.3), they reach its **workstation** routes too.
scion runs the hub non-hosted, so `/api/v1/system/*` is enabled, and that route group is gated
only by workstation mode plus a loopback-origin check: it runs **no permission check**, and every
agent holds a valid hub identity (its own agent token). §4.2 covers the one that touches lever's
identities, `PUT /api/v1/system/identity`, which renames scion's dev user: absent on a fresh
instance, the member controller on a migrated one. Remote logins cannot land on it: each login's
hub user is invited and recorded before its first sign-in, so the dev user cannot take that
address, and the proxy keeps a session only when it belongs to exactly the recorded hub user,
never to scion's dev user. `lever apply` never records, binds, caps or suspends lever's own hub
users for a login.

What still holds: lever does not stop an agent from calling these routes. It cannot, since they
are plain loopback HTTP that agents must reach for the rest of the hub API, and an egress rule
cannot single out one HTTP path. The route group has other writers that run no permission check
either (the workstation settings, image registry, runtime, init, image pull and build, and
`fs/mkdir` routes). The durable fix is upstream: a permission check on scion's `/api/v1/system/*`
mutating routes, or running the hub in hosted mode (which removes `system/*` entirely).

The image registry route is the one to watch, since it changes where agent images come from.
scion rewrites a bare image name such as `myorg/lever-claude` to the registry it holds, and
this scion keeps that registry in the hub database, which outranks `settings.yaml` and applies at
the next agent start without a restart. An agent can set it to a registry it controls. lever
therefore starts every agent on a name with a registry host, `lever.local/<name>`, which scion
never rewrites: whatever the registry says, the agent runs the image `lever apply` loaded into the
jail. No registry by that name exists, and the jail's podman refuses any pull from it, so a
missing image fails instead of asking the network. As defence
in depth, every `lever apply` also sets the registry back to `lever.local` as lever-admin. An agent
record created by an older lever keeps its bare image name, and the registry still applies to it
at each start: under lever's pin a record on `x` runs `lever.local/x`, the image lever loaded, or
fails to start when there is none, but an agent can change the registry before the next apply.
A record with no image runs the template's or harness's default name, which is rewritten the
same way. So lever refuses to start either kind. `lever up`, `lever apply`, a conversation
restore or new conversation and the broker's re-enrol refuse to keep or resume such a manager and
say to run `lever up --fresh --keep-conversation`. The broker refuses to resume, start, heal or
wake such a worker, for the manager, the operator (`lever worker resume`, `start`, `new-session`,
autostart) and the chat page alike, and says to run `lever worker recycle NAME`. `lever doctor`'s
`manager image` row fails while any such record exists, and recreating them clears it.

The runtime route can likewise switch the hub's container runtime, which fails agent starts
until the next `lever apply` sets it back to podman as lever-admin; doctor's `hub container
runtime` row fails meanwhile. That pin stores the hub's profiles in its database, which from then
on outranks `profiles:` in the jail's `settings.yaml`. The workstation settings route can turn on
`auto_inject_gcloud_adc`, which makes scion copy the jail user's gcloud credentials into every
agent it starts; the jail holds none, and every apply removes the key.

The project marker `<tree>/.scion` is in the agents' reach too. lever never takes a symlink
there for the marker and removes one before `scion init`, but scion's broker, starting an agent,
writes a missing marker through a dangling symlink: an agent can make the jail user create a
small file of fixed content at a path it chooses. That needs a scion change.

### 4.4 The hub enforces the role ceiling too

Every agent lever creates is stamped `--role baseline` ([config `agent_role`](/reference/config/)),
which bounds the scopes in the agent's own hub token. On its own that stamp is the only bound: a
create issued by anything other than lever — a manager holding a stolen controller PAT, a scion
path lever does not drive — could ask for `full`. So `register-project` also writes the project's
**maximum and default agent role** to the same role, through scion's project-settings route (gated
on `project:update`), and verifies the hub kept it. The hub then refuses any create above the
ceiling regardless of who asks. `lever doctor`'s `agent role ceiling` row reads it back; an unset
ceiling is a finding, because scion reads unset as `full`.

**Residual.** This closes the isolation gap between workers, and between an agent and the hub
itself. It does not change the manager's own trust position: the manager legitimately mounts the
whole tree (§4.1), so a compromised manager can still read and write everything the instance keeps
there, including the knowledge base and every worker's subdirectory, that is an inherent cost of
giving the manager whole-tree oversight ([§7](/security-model/compromise/)), not a gap in the
worker-isolation model above. No automated live gate exercises this guarantee yet; see
[validation](/security-model/validation/).

### 4.5 Opt-in shared folders

`shared_folders` lets the operator give some agents one common directory. Each entry names a tree
folder, its **writers** (mount it read-write) and its **readers** (mount it read-only), and every
agent that has access mounts it at `/shared/<name>`. With no entry the rest of §4.1 holds unchanged:
a worker mounts only its own subdirectory. What an entry changes, and what still holds:

- **It is a deliberate write path between agents.** Content a writer puts in the folder is read,
  and may be run, by every reader and by the manager. A compromised writer can therefore steer
  them, for instance by publishing a changed script that a reader runs next. That is the point of
  the feature, so lever does not filter the contents; the boundary is who may write. Name as
  writers only agents you would trust with that reach, and keep the manager's own tools out of a
  folder a worker writes unless that is the intent.
- **Nobody else can write it.** A reader's mount is read-only (the runtime enforces it), and an
  unlisted worker does not mount the folder at all. The manager, which mounts the whole tree, gets
  the folder read-only at its tree path unless it is a writer, using the `manager.read_only`
  mechanism ([§5.1.1](/security-model/config-trust/)).
- **Nobody can move it or swap it for a link.** The guest resolves a bind source through symbolic
  links, and the tree is agent-writable, so a folder replaced by a link would mount whatever the
  link names. Only the manager can reach the folder's path in the tree (a worker sees the folder
  only as its own mount point), so the manager pins the folder itself (read-only, or read-write
  when it writes it), every directory between the tree root and the folder, and every worker
  `dir`: a mount point cannot be renamed or removed. lever also walks each folder's path from the
  tree root on the host and refuses a missing folder or a link on the path: before the manager is
  started or resumed by `lever apply`/`lever up`, and before every worker start and resume. And
  while any folder is configured, the broker starts or resumes a worker (any worker: a worker dir
  the manager swapped for a link to a folder would mount it read-write) only after reading the
  manager container's mounts (its configuration and, for a running manager, the kernel's mount
  table from the guest side) and finding its whole tree plan in place; a manager created before the
  current plan lacks pins and must be recreated first. A manager with no container, or a stopped
  one whose configured mounts hold the plan, passes: it cannot act while it is down.
- **Old names are refused.** A hard link made before the folder was shared would let its other
  name edit a file inside the folder. Right before it creates the manager, `lever apply` refuses a
  file in a folder with a link count higher than its names inside the folder, and a directory in
  it that it cannot read (such a directory could hide an old name that readers can still open). New links across the boundary cannot be made from a
  container: a hard link between two mounts fails (`EXDEV`). Links that stay inside the folder,
  and symbolic links inside it, are the writers' business: a link resolves in the container that
  follows it, which reaches only what that agent already sees.
- **Host-written places stay out.** A folder may not be, or lie in, `.lever` (the manager's
  enrolment ticket), the state directory or `.lever-files`, nor overlap a worker `dir`, another
  folder or a `manager.read_only` entry. `remote.labels_file` may not lie in a folder a worker
  writes.
- **No host code runs from it.** Config load refuses a broker tool whose program or script lies in
  a shared folder; unlike `manager.read_only`, no shared folder excuses one, since a worker may
  write it.
- **Access changes need fresh agents, and stale access is refused.** The broker holds the plan it
  read at start; `shared_folders` and `manager.read_only` are in its config hash, so `lever apply`
  restarts it after a change and the checks below compare against the new plan, never a stale one.
  scion keeps a record's mounts for life. A worker created under an older plan could get back access the config has withdrawn on
  its next resume, so the broker reads the worker's hub record before every resume (a dispatch, an
  operator wake, the re-enrolment healer's bounce) and refuses one that holds a shared mount the
  plan does not grant, or grants only read-only. While any folder is configured, a record it
  cannot read is refused too (with "retry", not a purge): a record outlives the config that made
  it. Once the list is empty the broker still refuses a record that holds any `/shared` mount, but
  only when the read succeeds: an instance with no folder lets a resume or a re-enrolment heal go
  on when the hub read fails, so a removed folder's leftover record can be resumed during a hub
  outage. Purge such workers when you remove the last folder. The fix is to discard the record (`lever worker purge`, or the
  manager's `agent recycle` for a recyclable worker).
- **Replace the contents, not the folder.** A mount stays on the directory that existed when the
  container started. `lever doctor` reads each running reader's mounts from the guest (the kernel's
  mount table and the directory's device and inode, never a program in the container) and fails
  when one covers another directory than the host has now.

**Residual risks.** A running agent keeps its mounts until it is recreated: withdrawing access is
complete only after the purge (or, for the manager, `lever up --fresh`), and the manager's own
resume is not refused, only reported by doctor. The checks before a mount and the mount itself are
not atomic; the pins are what close that window, which is why workers wait for them. Writers'
mounts are checked by configuration only, since a writer is meant to write.

### 4.6 Spawned agents

An agent spawned from a template is a worker in every check above: it mounts only its own
`<dir_root>/<name>`, holds only its template's grants, and is started and resumed through the same
broker checks. What spawning adds, and how it is held:

- **The spawner chooses only the task, a label and a memory size.** The size must be one of the
  template's `memory_sizes`, matched by value; the broker refuses a spawn body with any other
  field, picks the name itself and builds the spec from the template alone. A template the manager
  may spawn from may obtain and delegate only what the manager obtains (config load enforces it),
  so the manager cannot mint an agent wider than itself.
- **Names are never reused.** Authority is checked by certificate CN, and certificates and tokens
  live up to 24 hours. A destroyed agent's name is revoked for good and kept as a tombstone in
  `.lever-state/agents.json`; a revocation that is not saved stops the destroy, and the broker
  re-asserts the revocation of every destroyed name at start.
- **The manager cannot touch a spawned agent's workspace.** Each template's `dir_root` is mounted
  read-only into the manager, whatever `manager.read_only` says, with its ancestors pinned. A
  workspace is created at runtime, after the manager's mounts are fixed, so no per-agent pin could
  hold it; read-only, the manager can neither swap one for a link between the broker's check and
  the agent's mount (which would mount a protected directory read-write in an agent the manager
  briefs), nor write a spawned agent's files or settings. The broker also creates each workspace
  exclusively with no link on its path, records its device and inode, and refuses a resume of a
  workspace that is no longer that directory. An agent whose template's `dir_root` changed since
  its spawn is driven no more and holds no grants (only destroy reaches it, by that device and
  inode). A manager created before templates were configured lacks the read-only mount: while it
  runs, the broker refuses every worker and spawned agent start and resume, `lever apply` and
  `lever doctor` say so, and `lever up --fresh --keep-conversation` recreates it.
- **Destroy reads the workspace without following it.** It archives and removes only through a
  handle on that directory, so nothing outside the workspace is read or removed. The archive walk
  descends only into what its own `Lstat` found to be a directory, through a handle checked to
  be that directory, so a link swapped in meanwhile is stored as a link and never followed; files
  are opened with no link at their leaf; special files are skipped; directories are read in
  batches, and nesting and entries are bounded.
- **The host's disk and memory stay bounded.** An archive starts only with 2 GiB free in the
  state directory and stops (keeping the workspace) below 1 GiB; a conversation backup leaves
  2 GiB free. One conversation capture runs at a time in the broker, so destroying many agents at
  once does not hold many conversations in memory.
- **Destroy cuts authority before it deletes.** Grants are dropped and the name revoked before the
  workspace and the hub record are touched; a spawn or destroy that a restart interrupted holds no
  grants and is no worker until the broker settles it.
- **The manager reaches only its own.** It drives, messages, lists, destroys and reads the inbox
  of only the agents it spawned, and only while their template still lists `manager` in
  `spawners` (a template that drops the manager may since have been given wider grants). Its
  answers name no host path.
- **Only the operator's web logins reach them.** The remote proxy lists, wakes and links spawned
  agents for operator logins only, from the broker's registry over the host-only operator socket;
  a contact's lists name configured agents only, and the broker itself refuses a contact-tier wake
  of a spawned agent. A spawned agent never takes the place of a configured agent of the same
  name in the web page.
- **An ssh key spawns only within its forced command.** `lever agent remote-verb` is the key's
  `authorized_keys` command, so the caller never picks the program, only the words it parses.
  Those words have a fixed grammar: spaces split them, and any other whitespace, control or
  formatting character refuses the line. They never reach a shell, a flag parser or a path, and
  the brief goes on stdin, bounded. The key spawns from its listed templates only, is the
  recorded creator (`ssh:<name>`, which no agent or manager name can equal, since those hold no
  colon), and lists and drives only its own agents. The broker itself refuses that caller's
  suspend, resume or destroy of another's agent and a destroy without a backup, audits its
  spawns, suspends, resumes and destroys under its name, and rate-limits its spawns apart from
  the manager's. The caller gets a fixed sentence per refusal, never the broker's text or a host
  path, and at most 4 of its commands run at once, each for at most 10 minutes. Within those
  bounds the key holds the operator's authority over the socket: treat it like a deploy key for
  those templates.
- **The agents loop only suspends.** Whether an agent waits at its prompt is what the agent
  reports to the hub, so the broker's once-a-minute loop acts on it only in ways that harm no one
  else: it suspends that agent (keeping its conversation) under the agent's lock after reading the
  hub again, and the remote proxy sends operator logins a content-free "waiting" push, bounded
  per login and agent. The loop never stops, starts or destroys an agent. Its daily backup reads
  the agent's conversation from the guest, as destroy does, into the host-only state directory.
- **The message queue widens no one's reach.** A message to an agent that is not running (for
  example one `idle_suspend` put to sleep) is queued by the broker in the host-only state directory
  (0700 directories, 0600 files written by rename, outside every agent mount), with a per-recipient
  count and byte cap checked before anything is written, in separate pools for the operator's
  notes, the manager's messages and the other agents', half of the agents' pool the most one agent
  may hold. An agent refused for a full pool learns no counts, and an agent's answer counts only its
  own queued messages. Notices to senders about their queued messages go out from one background
  sender with a bounded queue, never while a queue or worker lock is held, so a slow hub cannot hold
  up a send or a destroy. The send is authorized as a live send is, and again at
  delivery: a sender that was revoked, purged or recycled since, or that the policy no longer lets
  reach the recipient, or a recipient that is a new life of its name (destroyed, purged or recycled
  since), gets nothing delivered. Delivery is a live send, made at delivery time with the sender's
  own marker and kind recorded in the sent ledger, so `message_verify` answers exactly the tier a
  live send would; the line saying the message waited is lever's, under the marker, and the sender's
  text cannot forge it. A message is sent at most once: one a broker crash interrupted is dropped,
  never repeated. `--wake` only starts a resume its sender could already make (the operator, or the
  manager for a worker it drives; never a worker, never the manager), admitted against the running
  caps and memory budget like any other resume.

- **Spawners stay apart.** A `delegate` to `template:<name>` held by the manager reaches only the
  agents the manager spawned, and one held by a spawned agent only its own spawner's. With
  `worker_to_worker`, a spawned agent messages and is messaged by only the agents of its own
  spawner. So the operator's, the manager's and each ssh key's agents form separate groups, joined
  only through the manager (whom every agent may message) or a configured worker's delegate.
  A restore puts the old agent's transcripts in the new workspace, inside the tree, where the
  manager reads them through its read-only view of `dir_root`: restoring an ssh key's or the
  operator's agent shows the manager that agent's earlier conversation, tool output included.
- **The memory budget is a resource guard.** Every spawn, start and resume is admitted against
  `broker.memory_budget` by the memory limits the broker recorded (never a size an agent reports),
  but which agents count is read from the hub's phases, which an agent can report about itself.
  An agent that lies its way out of the count can let more start than the VM holds; each
  container's own `resources.memory` still bounds it. A refusal names only agents its reader may
  reach: the manager sees declared workers and its own spawned agents and a count of the rest; a
  chat login, an ssh key and a spawned agent see the sizes alone. The totals (`budget`, `used`,
  `free`) are summed over every agent and shown to the manager and to ssh keys.
- **A restore grants nothing new.** `lever agent restore` (and an ssh key's `restore`, of its own
  agents only) is a spawn: the same admission, caps, rate limits and memory budget, a new name,
  and only its template's grants. What the archive holds is the destroyed agent's own output and
  is read as hostile: only directories and regular files are written (links, hard links and
  devices are skipped), with no setuid bits, through a handle on the new agent's own directory
  that spawn just created, each file created exclusively; an absolute or `..` name, a duplicate,
  or an archive past 200,000 entries, 8 GiB or 256 levels refuses the whole restore before a
  name is drawn. The template's skill is written fresh, never taken from the archive. A restore into another
  template leaves out every path through a name that configures an agent, at any depth and in any
  case (`.claude`, `.mcp.json`, `CLAUDE.md`, `CLAUDE.local.md`, `.claude.json`, `.lever`, `.envrc`,
  and every git repository, `.git` and `.gitmodules`), which would otherwise run with the new
  template's grants; its task says the transcript's requests are not the operator's. Names that
  differ only in case, or are not valid UTF-8, refuse the archive, since a case-insensitive host
  would merge them. The transcripts never carry such a name, in any template. A
  key's restores are rate-limited before an archive is read. The old
  transcript lands in the new workspace (in the tree, which the manager can read), and the agent
  is told it is a record of earlier work, not instructions. Archives stay host-only (0700
  directory, 0600 files) and never enter the tree except through a restore.
- **Archives and backups are bounded.** Conversation backups and archives stay in the host-only state
  directory: daily backups keep the newest 10 per agent, a destroy moves its backup into the
  agent's archive, and `lever agent gc` (or `archive-delete`, one at a time) deletes the archives
  and backups of agents destroyed long enough ago, keeping only their names (still revoked, never
  drawn again). The retired names and the revocations grow by one short entry per spawn, bounded
  by the spawn quotas.

**Residual risks.**

- An operator-only template with grants wider than the manager's holds them for whatever it is
  asked: its brief comes from whoever spawns it (the operator, or an ssh key allowed that
  template). Keep such templates' grants as narrow as their work allows.
- A configured worker's `delegate` to `template:<name>` reaches every agent of that template,
  whoever spawned it: that is the operator's word in config.
- Until `lever agent gc` runs, destroyed agents' archives and backups stay; the spawn quotas and
  the disk floors above bound how fast they grow.
- All spawners share one memory budget with no reservation. A spawner (the manager, or an ssh
  key) that keeps its agents running at their largest sizes can leave too little for others, so
  the operator's workers and agents, and other keys', are refused `memory-busy` until it suspends
  some; only the operator can suspend another's agents. Bound each template with `max_running`
  and its `memory_sizes`, so that what the manager's and each key's templates can hold at once
  leaves room for the rest, or set `broker.memory_budget` with that in mind.
