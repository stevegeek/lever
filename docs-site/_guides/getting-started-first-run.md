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
  version: e82a2a08      # pin a scion commit; fetched + cross-compiled into the jail
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

You'll see a `backend: <profile summary>` line, then the ordered plan:

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
  agent-template          /…/hello-worker/workspace
  mint-manager-bootstrap  /…/hello-worker/workspace
  start-manager           hello-worker
```

`bootstrap-token` mints the controller PAT that drives every later scion verb, through a throwaway
dev-auth-on hub that is killed before any agent exists; `scion-server` then starts the real hub with
`--dev-auth=false` (see [security model §4.2](/security-model/worker-isolation/)). `credential`
(present because this example sets `credential_file`) stages the manager's Claude OAuth token.
`register-project` is the one `scion init`/`hub link` for the whole instance; the manager and every
worker are agents inside it. Workers are not started here; the manager dispatches them on demand
(step 6).

## 4. Scaffold the operator skills (`lever init`)

Lever ships SKILL.md files that teach your agents how to operate inside the
jail — the capability flow (mint via `lever-capability`, attach the token as
`_capability` on every gated call), messaging, and worker dispatch. Scaffold
them into your instance tree:

```sh
lever init
```

This writes `.claude/skills/lever-operator/` at the tree root, `.claude/skills/lever-agent/`
inside each declared worker directory, and a marked reference block in your tree-root `CLAUDE.md`.
The files are plain markdown in your tree, stamped with the lever version they came from.

Re-run `lever init` after upgrading lever or adding a worker. Edited files are left alone with a
warning; `--force`, `--check` and `--adopt` are described in the
[CLI reference](/reference/cli/#setup-and-diagnosis).

## 5. Bring it up

```sh
lever up
```

`lever up` runs the plan from step 3 (creating the jail if needed), starts the manager, and hands
you its terminal. **First boot takes ~10-15 minutes** (runtimes + a multi-GB image load); after
that it is fast.

`up` is idempotent: re-running it resumes a suspended manager and re-attaches. Detach with
`Ctrl-b d`; the manager is left suspended and the next `lever up` resumes the same conversation.
`--fresh` starts a new manager thread; `--no-attach` brings up without attaching.

To reattach without re-provisioning, run `lever attach` from the instance root in a separate
terminal. It is strictly passive: if the jail is not up it fails with "run `lever up` first".

If something looks wrong, run `lever doctor`; each failing check prints a fix hint (the full check
list is in the [CLI reference](/reference/cli/#setup-and-diagnosis)).

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
- There is no `--image` flag: the broker resolves the worker's image from the config
  (`workers[].image`, else the manager image).

`agent start` is for a fresh worker only; a worker's task is fixed at creation, so `agent start`
against an existing record returns HTTP 409. Use `lever-manager agent resume|suspend|stop NAME` for
lifecycle, and `lever worker purge NAME` (host) to discard the record before starting it again with
a new task.

Watch progress and relay events:

```sh
lever-manager watch --events-file events.jsonl &   # appends scion events to a file you can tail
lever-manager agent list        # phases of running agents
lever-manager msg list          # typed inbox (input-needed, completion, …)
lever-manager msg send "answer" --to worker
```

`msg` and `watch` are mTLS clients of the broker (`/msg/send`, `/msg/list`), not of scion
directly; no agent holds a hub credential ([architecture §4](/architecture/)). `--to` takes
`agent:<name>`, a bare `<name>`, or `user:manager` for the manager. Routing is identity-derived and
default-deny; the policy is under `broker.messaging` in the [config reference](/reference/config/).

To see a worker's session directly, run `lever attach worker` from your host (another terminal,
instance root). To message an agent from the host without attaching, use `lever msg send "…" --to
worker` (or `--to hello-worker` for the manager); the note lands as the agent's next user turn.

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

`stop` suspends the manager (best-effort), stops the host broker, then powers the jail off; the
next `lever up` resumes the same manager conversation. `destroy` removes the jail machine
`lever-<name>`; your tree on disk is untouched, and the next `lever up` re-provisions from scratch.
See the [CLI reference](/reference/cli/#everyday-lifecycle).
