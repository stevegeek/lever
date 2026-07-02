---
title: Getting started
nav_order: 2
---
# Getting started

This walks you from nothing to a running lever application, a **manager** agent that dispatches
work to a **grove** (project agent), all inside a jail that contains the whole stack. We use the
bundled [`examples/hello-grove`](https://github.com/lever-to/lever/tree/main/examples/hello-grove) as the worked example.

## What you'll end up with

```
your machine
└── OrbStack isolated machine  "lever-hello-grove"   (the jail)
    ├── rootless podman + scion hub (loopback)
    ├── manager container        ← edits your tree in place, dispatches groves
    └── worker container         ← runs the dispatched task
```

The jail is the security boundary. Your project tree is bind-mounted **in place**, so agents edit
the real files, there's no copy or sync. See [security-model.md](/security-model/) for what
the jail does and doesn't protect.

## Prerequisites

- **macOS on Apple Silicon** with [OrbStack](https://orbstack.dev) running. (OrbStack is the
  validated backend today.)
- **Go 1.26+** to build the binaries.
- **A manager container image** on your host Docker, e.g. `scionlocal/lever-claude:latest`. `lever
  apply` loads this image into the jail; it can't be pulled from inside (egress is locked down).
  Confirm with `docker images | grep scionlocal/lever-claude`.
- **A Claude OAuth token** in a file (mint with `claude setup-token`) for this subscription demo.
  Point `manager.credential_file` at it. Use a least-privilege token; in subscription mode it is
  projected into the agent containers (see the security doc).

## 1. Install the binaries

```sh
cd /path/to/lever_to
make all
```

This builds two binaries:

- **`lever`** (host control plane) → `~/.local/bin/lever` (make sure that's on your `PATH`).
- **`lever-manager`** (in-jail orchestration) → `$LEVER_INSTANCE/vendor/bin/lever-manager`,
  cross-compiled for the jail's linux/arm64. It must land **inside the bind-mounted `tree:`** so the
  manager can run it in the container, so `LEVER_INSTANCE` points at the **tree directory** (the
  `tree:` subdir), not the instance root.

Set `LEVER_INSTANCE` to the bind-mounted tree (the default is a neutral placeholder). For a different
instance, point it at that instance's tree:

```sh
make lever-manager-linux LEVER_INSTANCE=/path/to/your/instance/workspace
```

Verify: `lever version`.

## 2. Look at the instance

`examples/hello-grove` is a complete, minimal instance. The **root** holds the config and boot
prompt (host-only); only the **`workspace/`** subdir is bind-mounted into the jail:

```
hello-grove/             # instance root, run `lever` here; NOT mounted
├── lever.yaml           # the config
├── manager.md           # the manager's boot prompt (host-only)
└── workspace/           # tree: the bind-mounted subdir (agents edit this)
    ├── vendor/bin/      # staged lever-manager (must be inside the tree)
    └── groves/
        └── worker/      # the grove's workspace
```

```yaml
# examples/hello-grove/lever.yaml
name: hello-grove
backend: orbstack
tree: workspace          # a confined SUBDIR; the root itself is never mounted
scion:
  version: 666333f9      # pin a scion commit; fetched + cross-compiled into the jail
# api-key is the secure default (the real key never enters the container) but
# needs a Console API key. This demo opts into subscription (your Claude OAuth
# token), so egress stays open and the token is projected to the agents.
broker:
  llm_auth: subscription
manager:
  image: scionlocal/lever-claude:latest
  prompt_file: manager.md   # resolved at the root (host-only), not inside the mount
  credential_file: ~/.scion/oauth-token  # YOU supply this: your Claude OAuth token (0600)
  allow_ports: []
groves:
  - name: worker
    dir: groves/worker      # relative to tree, i.e. workspace/groves/worker
```

The `credential_file` is the one thing you add: point it at a least-privilege
Claude OAuth token (mint with `claude setup-token`). In subscription mode its
contents are projected into the agent containers, so keep it `0600`.

The config and prompt live at the root, *outside* the mount, so a compromised agent can't rewrite
them. See [config-reference.md](/reference/config/) for every key.

Stage the in-jail binary into the **tree** (`workspace/`) before bringing it up, so it lands inside
the bind-mount the manager can see:

```sh
make lever-manager-linux LEVER_INSTANCE="$PWD/examples/hello-grove/workspace"
```

## 3. Preview the bring-up plan (no side effects)

Run `lever` from the **instance root** (where `lever.yaml` lives, there's no walk-up discovery):

```sh
cd examples/hello-grove
lever apply --dry-run
```

You'll see the ordered plan:

```
  jail-up                 /…/hello-grove/workspace
  broker-up
  load-image              scionlocal/lever-claude:latest
  init-machine
  config-registry
  scion-server
  register-manager        /…/hello-grove/workspace
  register-grove          /…/hello-grove/workspace/groves/worker
  mint-manager-bootstrap  /…/hello-grove/workspace
  start-manager           hello-grove
```

## 4. Bring it up

```sh
lever up
```

`lever up` creates the jail if needed (isolated machine → rootless podman → cross-compiled scion →
egress allowlist), loads the image, registers the manager + groves, starts the manager, and hands
you its terminal. **First boot takes ~10-15 minutes** (runtimes + a multi-GB image load); after that
it's fast.

`up` is idempotent: re-running it resumes a suspended manager and re-attaches. Detach with
`Ctrl-b d` (the manager is left suspended; the next `lever up` resumes the same conversation).
`--fresh` starts a new manager thread; `--no-attach` brings up without taking your terminal.

## 5. Dispatch a grove (inside the manager session)

You're now talking to the manager agent. It drives groves with the in-jail `lever-manager` binary.
A dispatch looks like:

```sh
vendor/bin/lever-manager agent start worker --task "Write a haiku to haiku.md"
```

Notes:
- **`worker` is the grove's configured name**, not a path or a bare scion slug. The command is a
  thin client of the capability broker: the broker authenticates the manager, validates the name
  against the config, and starts the grove host-side (with operator identity) so it mounts its own
  `groves/worker/` workspace rather than the manager's whole tree.
- **No `--image` needed**, the broker resolves the grove's image from the config (it inherits the
  manager image here). An explicit `--image` overrides.

Watch progress and relay events:

```sh
vendor/bin/lever-manager watch --events-file events.jsonl &   # appends scion events to a file you can tail
vendor/bin/lever-manager agent list        # phases of running agents
vendor/bin/lever-manager msg list          # typed inbox (input-needed, completion, …)
vendor/bin/lever-manager msg send "answer" --to worker
```

When `worker` finishes, the file it wrote (`groves/worker/haiku.md`) is there on your host, it was
mounted in place.

## 6. Give an agent an MCP server (the various ways)

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
except through a capability it was granted.

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
groves:
  - name: worker
    dir: groves/worker
    obtain:
      - {tool: db, op: read}                         # worker may use db.read — nothing else
```

- **`obtain`** — the agent can self-mint a capability for the listed `{tool, op}`.
- **`delegate`** — the manager can mint a token *bound to a named recipient* grove at dispatch time
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

## 7. Tear down

```sh
lever down
```

`down` removes the jail machine named `lever-<name>` (derived from the config, run it inside the
instance, or pass `--machine`). Your tree on disk is untouched; only the jail goes away. The next
`lever up` re-provisions it.

## Where to go next

- [config-reference.md](/reference/config/), every config key, defaults, conventions.
- [security-model.md](/security-model/), trust boundaries, the threat model, and the
  credential flow.
- `examples/two-agents-comms` and `examples/multi-project`, richer topologies.
