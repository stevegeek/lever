---
title: Getting started
nav_order: 2
---
# Getting started

This walks you from nothing to a running lever application, a **manager** agent that dispatches
work to a **worker** (an agent scoped to its own subdirectory), all inside a jail that contains the
whole stack. We use the bundled
[`examples/hello-worker`](https://github.com/stevegeek/lever/tree/main/examples/hello-worker) as the
worked example.

## What you'll end up with

```
your machine
└── OrbStack isolated machine  "lever-hello-worker"   (the jail)
    ├── rootless podman + scion hub (loopback)
    ├── manager container        ← edits your tree in place, dispatches workers
    └── worker container         ← runs the dispatched task
```

The jail is the security boundary. Your project tree is bind-mounted **in place**, so agents edit
the real files, there's no copy or sync. See [security-model.md](/security-model/) for what
the jail does and doesn't protect.

## Prerequisites

- **macOS on Apple Silicon** with [OrbStack](https://orbstack.dev) running. (OrbStack is the
  validated backend today.)
- **Go 1.26+** — both to build the binaries and on `PATH` at runtime: `lever apply`/`lever up`
  cross-compile the pinned Scion engine into the jail, so they shell out to `go`. If you manage Go
  with a version manager (asdf, mise), make sure the **real toolchain** is resolvable, not just a
  shim — a shim that isn't initialised in the non-interactive sub-process fails with
  `resolve go toolchain (is go on PATH?): exit status 126`. The fix is to put the actual toolchain
  bin on `PATH`, e.g. `export PATH="$HOME/.asdf/installs/golang/1.26.4/go/bin:$PATH"` (adjust the
  version); `go version` should print from that path.
- **The agent image** `scionlocal/lever-claude:<arch>` on your host Docker (images are tagged by
  architecture — `:arm64` / `:amd64` — so one host can cross-build both without clobbering; a tagless
  `manager.image` in the config auto-resolves to the jail's arch). `lever apply` loads it into the
  jail; it can't be pulled from inside (egress is locked down). If you don't have it yet, build it —
  one command once you have scion's base image:

      make lever-image                    # builds for your host arch (default arm64)
      # make lever-image LEVER_IMAGE_ARCH=amd64   # cross-build for an amd64 server

  This cross-compiles the in-jail binaries and builds `scionlocal/lever-claude:<arch>` FROM scion's
  stock `scion-claude:<arch>`. See [building the agent image](#1a-build-the-agent-image) for the
  scion-base prerequisite and how instances extend the image. Confirm with
  `docker images | grep scionlocal/lever-claude`.
- **A Claude OAuth token** in a file (mint with `claude setup-token`) for this subscription demo.
  Point `manager.credential_file` at it. Use a least-privilege token; in subscription mode it is
  projected into the agent containers (see the security doc).

## 1. Install the binaries

```sh
cd /path/to/lever_to
make all
```

This builds the host binary:

- **`lever`** (host control plane) → `~/.local/bin/lever` (make sure that's on your `PATH`).

The in-jail orchestration binary, **`lever-manager`**, isn't built here, it's baked into your agent
image. `make lever-image-bins` cross-compiles `lever-manager` (alongside `lever-agent` and
`lever-tool-db`) into your image build context (`LEVER_IMAGE_CTX` in the Makefile), and your
Dockerfile `COPY`s them to `/usr/local/bin`, so it's already on `PATH` inside the manager and worker
containers when they boot.

Verify: `lever version`.

## 1a. Build the agent image

`examples/hello-worker` (and every instance) runs the agent image
`scionlocal/lever-claude:<arch>`. Build the generic one in a single command:

```sh
make lever-image
```

This cross-compiles the in-jail binaries into `image/lever-claude/`, syncs the pre-start hook, and
runs `docker build` to produce `scionlocal/lever-claude:<arch>` (the local `scionlocal/` registry
prefix is what scion loads without a pull).

**Claude Code version:** the image bakes Claude Code at an explicit pin (`ARG
CLAUDE_CODE_VERSION` in the Dockerfile) and disables the in-container auto-updater — agents never
self-update; upgrades happen by rebuild. To bump: edit the ARG (or pass `--build-arg
CLAUDE_CODE_VERSION=X.Y.Z`), rebuild the image, `lever apply`, and power-cycle the manager
(`lever stop && lever up` — the conversation is preserved on OrbStack). Don't rely on the scion
base image's copy: it installs claude unpinned, so it's whatever was current when that base was
last rebuilt.

Cross-build for another arch with `LEVER_IMAGE_ARCH=amd64` (it builds `FROM scion-claude:amd64` and
tags the output `:amd64`).

**One prerequisite: scion's base image, arch-tagged.** lever-claude builds `FROM scion-claude:<arch>`,
scion's stock Claude harness image. Build it once from a scion checkout:

```sh
git clone https://github.com/GoogleCloudPlatform/scion
cd scion && image-build/scripts/build-images.sh --target harnesses
```

`--target harnesses` builds `scion-claude` (among the harness images); if you're starting from
nothing, `--target all` builds the whole base chain first. That leaves `scion-claude:latest` in your
local Docker store — **tag it for its arch** so `make lever-image` finds it:

```sh
docker tag scion-claude:latest scion-claude:arm64   # or :amd64, matching the image's real arch
```

(The build script tells you exactly this if the arch-tagged base is missing.) See scion's
`image-build/` for the full story — scion owns this step.

**Extending the image for your instance.** The generic image is deliberately minimal — scion's
harness plus lever's binaries and boot hook, nothing else. If your agents need a language toolchain
or a project CLI, write a small instance Dockerfile `FROM lever-claude:<arch>` that adds it, point
`manager.image` (and any worker `image:`) at your tag, and — importantly — if your added layer does
root-level work under `/home/scion`, end it by re-running `RUN chown -R scion:scion /home/scion` and
`USER scion`. The jail runs rootless podman, where a root-owned home is unwritable by the agent and
silently breaks its boot hook. Keep instance-specific tooling in that layer, not in the framework
image.

## 2. Look at the instance

`examples/hello-worker` is a complete, minimal instance. The **root** holds the config and boot
prompt (host-only); only the **`workspace/`** subdir is bind-mounted into the jail:

```
hello-worker/             # instance root, run `lever` here; NOT mounted
├── lever.yaml           # the config
├── manager.md           # the manager's boot prompt (host-only)
└── workspace/           # tree: the bind-mounted subdir (agents edit this)
    └── workers/
        └── worker/      # the worker's workspace
```

```yaml
# examples/hello-worker/lever.yaml
name: hello-worker
backend: orbstack
tree: workspace          # a confined SUBDIR; the root itself is never mounted
scion:
  version: 37a54a8e      # pin a scion commit; fetched + cross-compiled into the jail
  # NOTE: dispatching workers needs Scion's `--workspace-subdir` subtree-isolation
  # feature (fork branch feat/per-agent-workspace-subpath), not yet in this
  # pinned commit. Until it's upstreamed, build from the fork instead: swap the
  # `version:` line for `source: /path/to/scion` checked out on that branch.
# api-key is the secure default (the real key never enters the container) but
# needs a Console API key. This demo opts into subscription (your Claude OAuth
# token), so egress stays open and the token is projected to the agents.
broker:
  llm_auth: subscription
manager:
  image: scionlocal/lever-claude
  prompt_file: manager.md   # resolved at the root (host-only), not inside the mount
  credential_file: ~/.scion/oauth-token  # YOU supply this: your Claude OAuth token (0600)
  allow_ports: []
workers:
  - name: worker
    dir: workers/worker      # relative to tree, i.e. workspace/workers/worker
```

The `credential_file` is the one thing you add: point it at a least-privilege
Claude OAuth token (mint with `claude setup-token`). In subscription mode its
contents are projected into the agent containers, so keep it `0600`.

The config and prompt live at the root, *outside* the mount, so a compromised agent can't rewrite
them. See [config-reference.md](/reference/config/) for every key.

There's nothing more to stage: the image you built in [step 1a](#1a-build-the-agent-image) already
has `lever-manager` baked in at `/usr/local/bin`, so it's on `PATH` the moment the manager container
boots.

## 3. Preview the bring-up plan (no side effects)

Run `lever` from the **instance root** (where `lever.yaml` lives, there's no walk-up discovery):

```sh
cd examples/hello-worker
lever apply --dry-run
```

You'll see the ordered plan:

```
  jail-up                 /…/hello-worker/workspace
  broker-up
  load-image              scionlocal/lever-claude:arm64
  init-machine
  config-registry
  bootstrap-token         /…/hello-worker/workspace
  scion-server
  credential              ~/.scion/oauth-token
  register-project        /…/hello-worker/workspace
  mint-manager-bootstrap  /…/hello-worker/workspace
  start-manager           hello-worker
```

`bootstrap-token` mints the controller PAT that drives every later scion verb: a **throwaway**
dev-auth-on hub on a fixed jail-internal port (48080) no agent ever learns, registers the instance project,
mints a token scoped to exactly `agent:manage,agent:attach,project:read`, persists it `0600` under
`.lever-state/`, then is killed. `scion-server` then starts the **real** hub with `--dev-auth=false`
— agents never see an admin-open hub. `credential` (shown here because this example sets
`credential_file`) stages the manager's Claude OAuth token. `register-project` replaces the old
per-agent registration: it's the **one** `scion init`/`hub link` for the whole instance — the
manager and every worker are agents inside it, not separate projects. Workers themselves aren't
started here; the manager dispatches them on demand once it's up (step 6).

## 4. Scaffold the operator skills (`lever init`)

Lever ships SKILL.md files that teach your agents how to operate inside the
jail — the capability flow (mint via `lever-capability`, attach the token as
`_capability` on every gated call), messaging, and worker dispatch. Scaffold
them into your instance tree:

```sh
lever init
```

This writes `.claude/skills/lever-operator/` at the tree root (the manager
discovers it there), `.claude/skills/lever-agent/` inside each declared worker
directory, and adds a marked reference block to your tree-root `CLAUDE.md`.
The files are yours: they're plain markdown in your tree, stamped with the
lever version they came from.

Re-run `lever init` after upgrading lever or adding a worker — unmodified
files are refreshed in place, while files you've edited are left alone with
a warning (`--force` overwrites them). `lever init --check` reports staleness
without writing, and `lever doctor` includes the same check.

## 5. Bring it up

```sh
lever up
```

`lever up` creates the jail if needed (isolated machine → rootless podman → cross-compiled scion →
egress allowlist), loads the image, mints the controller PAT and registers the one instance project
(manager and workers alike are agents inside it), starts the manager, and hands you its terminal.
**First boot takes ~10-15 minutes** (runtimes + a multi-GB image load); after that it's fast.

`up` is idempotent: re-running it resumes a suspended manager and re-attaches. Detach with
`Ctrl-b d` (the manager is left suspended; the next `lever up` resumes the same conversation).
`--fresh` starts a new manager thread; `--no-attach` brings up without taking your terminal.

To reattach without re-provisioning, run `lever attach` from the instance root, in a separate
terminal. It hands your TTY straight to the manager and is strictly passive: if the jail isn't up it
fails fast with "run `lever up` first" rather than starting anything.

If something looks wrong, `lever doctor` runs real health checks (broker alive, external tool
backends reachable, the manager credential file's presence/size/mode, scion project-registration
consistency, and the operator-skills scaffold from `lever init` being present and current) and
prints a fix hint per failure.

## 6. Dispatch a worker (inside the manager session)

You're now talking to the manager agent. It drives workers with the in-jail `lever-manager` binary
(baked into the image, already on `PATH`). A dispatch looks like:

```sh
lever-manager agent start worker --task "Write a haiku to haiku.md"
```

Notes:
- **`worker` is the worker's configured name**, not a path or a bare scion slug. The command is a
  thin client of the capability broker: the broker authenticates the manager, validates the name
  against the config, and starts the worker host-side (with operator identity) — as an agent in the
  same single instance project as the manager, mounted at its own `workers/worker/` subdir rather
  than the manager's whole tree.
- **No `--image` needed**, the broker resolves the worker's image from the config (it inherits the
  manager image here). An explicit `--image` overrides.

Watch progress and relay events:

```sh
lever-manager watch --events-file events.jsonl &   # appends scion events to a file you can tail
lever-manager agent list        # phases of running agents
lever-manager msg list          # typed inbox (input-needed, completion, …)
lever-manager msg send "answer" --to worker
```

`msg` and `watch` are thin mTLS clients of the broker (`/msg/send`, `/msg/list`), not of scion
directly: an in-container scion CLI call has no hub credential to authenticate with in the first
place (dev-auth is off and no agent holds the controller PAT), so the broker (host-side, operator
identity) is what actually routes the message. `--to` takes
`agent:<name>`, a bare `<name>`, or the alias `user:manager` (routes to the manager agent; scion's
own user-messaging is container-only, so no other `user:*` form is broker-routable). Routing is
identity-derived and default-deny: the manager may message any declared worker and read any inbox
(`msg list --worker <name>`); a worker may message the manager and, by default, other workers too
(disable worker→worker with `broker.messaging.worker_to_worker: false`).

To eyeball a worker's session directly instead of polling events, run `lever attach worker` from
your host (another terminal, instance root): it hands your TTY to that worker the same way
`lever up` hands you the manager's; omit the name to attach to the manager.

You can also message an agent from the host without attaching at all: `lever msg send "…" --to
worker` (or `--to hello-worker` for the manager) is fire-and-forget — the note lands in the agent's
session as its next user turn, it acts on it unattended, and the exchange is waiting in the
scrollback the next time you attach. `--interrupt` injects it ahead of the agent's next turn.

When `worker` finishes, the file it wrote (`workers/worker/haiku.md`) is there on your host, it was
mounted in place.

## 7. Give an agent an MCP server (the various ways)

Agents get real power from MCP (Model Context Protocol) servers — calendars, search, a database,
your own tools. But an MCP server runs on your host, and the jail's whole point is that a
prompt-injected agent *can't* reach your host freely. So "attach an MCP server" is really "poke a
controlled hole." lever gives you two ways to do it, with a clear trade-off.

### Approach A — ambient (`allow_ports` + `.mcp.json`)

The simplest path: run the server on a host loopback port, open that one port through the jail's
egress allowlist, and hand the agent an `.mcp.json` pointing at it.

```yaml
# lever.yaml
manager:
  allow_ports: [3200]         # open exactly this host-loopback port to the jail
```

```json
// workspace/.mcp.json  (inside the mounted tree, so the agent's harness reads it)
{ "mcpServers": { "mytool": { "type": "http", "url": "http://host.orb.internal:3200" } } }
```

The agent now reaches `mytool` directly. This is easy and fine for a **trusted, read-only** server,
but understand what it is: an **ambient** grant. Any agent in the jail can hit that port with no
per-call check, and the port is a standing hole in the egress allowlist for as long as it's listed.
There is no capability, no per-agent scoping, and no audit.

### Approach B — brokered (capability-gated, recommended)

Register the server as a **broker tool** instead. The capability broker (host-side, holds no agent
trust) fronts it behind its mTLS gateway at `/mcp/<name>/`; an agent reaches it **only with a
capability token the broker mints**, bound to that agent's identity. No `allow_ports` hole, no
hand-authored `.mcp.json` — `lever-agent boot` (baked into the image) discovers registered tools via
the broker's `/tools` and runs `claude mcp add /mcp/<name>/` for each. You get per-agent scoping and
an audit trail, and closing the ambient hole means a compromised agent can't reach the port at all
except through a capability it was granted. (The full model — enrolment, tokens, delegation,
revocation — is described in [capabilities.md](/capabilities/).)

There are **two kinds** of broker tool:

**External tool** (`external: true`) — front a server that is *already running* (you start it; the
broker does **not** spawn it). This is the right choice for third-party or desktop-app servers —
e.g. anything driving macOS apps via AppleScript, where the OS ties Automation permission to your
login session and a broker-forked child would lose it. Bind it on host **loopback** (a literal
`127.0.0.1`/`[::1]`; the gateway proxies host-side, so a non-loopback backend would let the agent
reach your LAN *through* the broker). Pick a capability grain:

```yaml
broker:
  tools:
    # fine: only the listed operations are callable; arguments can be pinned.
    - name: devonthink
      external: true
      backend: 127.0.0.1:3302
      operations:
        - {name: search}
      allowed_values: {database: [work, personal]}
    # coarse: one wildcard capability admits the server's WHOLE surface.
    - name: things3
      external: true
      gate: coarse
      backend: 127.0.0.1:3300
```

- `gate: fine` (the default) — enumerate `operations`; a token authorises one operation, and
  `allowed_values` pins arguments. Use for anything sensitive.
- `gate: coarse` — one wildcard grant (`op: "*"`) admits every call the server exposes. Simplest;
  wholesale trust. The gateway audits the real operation either way.

**First-party (captool) tool** — a tool the broker **supervises** as a subprocess (you give it a
`command`), written with the captool SDK so it re-verifies the capability itself and the gateway
forwards the token to it. Use this when you're *writing* the tool and want it capability-aware with
the tightest control:

```yaml
broker:
  tools:
    - name: db
      command: [lever-tool-db]     # the broker launches + supervises this
      backend: 127.0.0.1:3201       # the loopback address it listens on
      operations:
        - {name: read}
      allowed_values: {table: [users, orders]}
```

### Granting access — who may use which tool

A registered tool is inert until an agent is granted a capability for it. Grants are per-identity and
default-deny:

```yaml
manager:
  obtain:
    - {tool: calendar, op: "*"}                     # the manager may use calendar itself
  delegate:
    - {tool: devonthink, op: search, to: [worker]}  # …and may hand worker this at dispatch
workers:
  - name: worker
    dir: workers/worker
    obtain:
      - {tool: db, op: read}                         # worker may use db.read — nothing else
```

- **`obtain`** — the agent can self-mint a capability for the listed `{tool, op}`.
- **`delegate`** — the manager can mint a token *bound to a named recipient* worker at dispatch time
  (an attenuated hand-off).
- Absence of a grant = no access, and a grant for one agent can't be replayed by another (the token
  is identity-bound). `op: "*"` is honoured **only** for a `gate: coarse` tool, so a wildcard can
  never widen a `fine` one.

### Which should I use?

| | Ambient (A) | Brokered — external (B) | Brokered — first-party (B) |
|---|---|---|---|
| Best for | quick, trusted, read-only | an existing/third-party server (esp. desktop-app/AppleScript) | a tool you're writing |
| Broker spawns it? | no | no (you run it) | yes (supervised) |
| Per-agent scoping / audit | no | yes | yes |
| Egress hole | standing (`allow_ports`) | none | none |
| Capability required per call | no | yes (token stripped) | yes (token forwarded) |

Start with **A** to get moving; move a server to **B** when you want it scoped per agent, audited,
and off the ambient allowlist. See [config-reference.md](/reference/config/) for every key and
[security-model.md](/security-model/) for what the gate does and doesn't protect.

## 8. Detach, stop, or destroy

Three levels, from lightest to heaviest:

| | detach | `lever stop` | `lever destroy` |
|---|---|---|---|
| What happens | leave your TTY (`Ctrl-b d`) | power the jail machine off | delete the jail machine |
| Manager state | suspended, in memory | suspended, then the VM halts | gone |
| Disk (image, containers) | untouched | untouched | deleted |
| Host broker | still running | stopped | stopped, staged state cleared |
| Resume with | `lever up` / `lever attach` | `lever up` (powers back on, **same conversation**) | `lever up` (full re-provision) |

```sh
lever stop
```

`stop` best-effort suspends the manager (skipped if the jail isn't reachable — a halted machine is
still stoppable), stops the host broker, then powers the jail machine off. The disk — including the
manager's session — is preserved: the next `lever up` powers the machine back on, re-arms the broker,
and **resumes the same manager conversation** (the suspended agent record survives the power-off and
scion relaunches the session from the persistent agent home). No reinstall, no re-registration.

```sh
lever destroy
```

`destroy` removes the jail machine named `lever-<name>` (derived from the config, run it inside the
instance, or pass `--machine`). Your tree on disk is untouched; only the jail goes away, along with
its staged runtime state. The next `lever up` re-provisions it from scratch. (`lever down` still
works as a deprecated alias of `destroy`.)

## Where to go next

- [config-reference.md](/reference/config/), every config key, defaults, conventions.
- [security-model.md](/security-model/), trust boundaries, the threat model, and the
  credential flow.
- `examples/two-agents-comms` and `examples/multi-project`, richer topologies.
