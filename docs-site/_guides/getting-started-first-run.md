---
title: "First run"
nav_order: 2.2
parent: Getting started
permalink: /getting-started/first-run/
---
Part of [getting started](/getting-started/). Steps keep their original numbering.

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

There's nothing more to stage: the image you built in
[step 1a](/getting-started/install/#1a-build-the-agent-image) already
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
- **No `--image` needed** — the broker resolves the worker's image from the config (it inherits the
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

When `worker` finishes, the file it wrote (`workers/worker/haiku.md`) is there on your host; it was
mounted in place.

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
