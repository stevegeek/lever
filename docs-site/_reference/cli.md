---
title: CLI
nav_order: 3
---
# CLI reference

Lever ships two binaries with user-facing commands: **`lever`**, the host control plane you run
from an instance root, and **`lever-manager`**, the in-jail orchestration CLI the manager agent
runs inside its container. (A third binary, `lever-agent`, is baked into agent images and run
automatically by the container pre-start hook — it has no operator-facing surface.)

All `lever` commands read `./lever.yaml` from the current directory when the config argument is
omitted — there is no walk-up discovery, so run them from the instance root.

## `lever` — host control plane

### Everyday lifecycle

| Command | What it does |
|---|---|
| `lever up [config]` | Bring the application up *if needed* (create jail, provision scion, start the manager) **and attach** the manager's TTY. Idempotent: re-running resumes a suspended manager and re-attaches — same conversation, even across a `lever stop` or host reboot. `--fresh` starts a new manager thread (deletes the old record); `--no-attach` brings up without taking your terminal. The everyday entry point. |
| `lever attach [name]` | Attach your TTY to the manager (default) or a named worker. Strictly passive: fails fast with "run `lever up` first" if the jail isn't up — it never provisions. Detach with `Ctrl-b d`. |
| `lever msg send "…" --to NAME` | Host-side fire-and-forget note to the manager (use the app name) or a declared worker — no attach needed. The note lands as the agent's next user turn; it acts on it unattended and the exchange waits in the scrollback for your next attach. `--interrupt` injects it ahead of the agent's next turn. Strictly passive like `attach`. |
| `lever reload [config]` | Apply config changes (new worker, tool, or grant) to a **running** instance without a VM power cycle: stops the broker, re-runs the idempotent apply on the current config, spawns a fresh broker. The manager container keeps running, so its conversation is preserved and your TTY isn't taken. Needed because the broker reads `lever.yaml` only at startup — a plain re-`apply` keeps the old broker. |
| `lever stop` | Power the jail off but **keep its disk** — the daily "done for the day". Suspends the manager (conversation preserved), stops the host broker; a later `lever up` powers it back on and resumes the same session. Installed runtimes and scion state persist. |
| `lever destroy` | Full teardown: delete the isolated machine and everything in it. Targets `lever-<name>` from config; override with `--machine`. `lever down` is a deprecated alias. |

### Setup and diagnosis

| Command | What it does |
|---|---|
| `lever init` | Scaffold/refresh the framework operator skills (SKILL.md) into the instance tree — `lever-operator` at the tree root, `lever-agent` in each declared worker dir — plus a marked reference block in the tree-root CLAUDE.md. Hash-guarded: files you've edited are left alone with a warning (`--force` overwrites); `--check` reports staleness without writing (non-zero exit). Re-run after upgrading lever or adding a worker. |
| `lever doctor` | Run real health checks — broker alive and serving, external tool backends reachable, manager credential file presence/size/mode, no stray `.mcp.json` in the tree, usable Go toolchain, scion project-registration consistency, operator-skills scaffold current — each failure printing a specific fix hint. Exits non-zero on any failure. `--machine`/`--backend` run the profile away from an instance root. |
| `lever apply [config]` | Headless bring-up: runs the full ordered plan (jail → broker → images → init-machine → config-registry → bootstrap-token, a throwaway dev-auth hub mints the controller PAT → scion-server, dev-auth off → credential → register-project, one registration for the tree → mint-manager-bootstrap → start-manager) with no attach. `--dry-run` prints the plan and exits with no side effects. The non-interactive half of `up`, for scripts and scheduled runs. |
| `lever provision` | Low-level: provision the jail only (create the isolated machine, install runtimes + scion, apply egress rules). `--machine`, `--tree`, `--allow-port`. Idempotent; rarely needed directly — `up`/`apply` call it for you. |
| `lever backends` | List the containment backends (orbstack, lima) and the guarantees each declares — the matrix config validation checks your `backend:` choice against. |

### Broker operations

| Command | What it does |
|---|---|
| `lever broker serve [config]` | Run the capability broker + first-party tools in the foreground (normally `up`/`apply` daemonize it for you — this is for debugging and supervised setups). |
| `lever broker revoke <agent> [config]` / `lever revoke <agent>` | Revoke one agent on the running broker: its capability tokens stop verifying immediately. |
| `lever broker bump-epoch [config]` | Revoke **all** outstanding tokens at once by raising the epoch floor. |
| `lever acceptance [config]` | Bring up a real jail and drive the six live capability/egress acceptance checks — delegated-read, three scope-envelope denials (a disallowed table, a dropped narrowing filter, a worker self-minting an un-granted cap), egress-refused (allowlisted broker port reachable, admin port blocked), and revocation (a token stops working after `bump-epoch`) — the merge gate for capability-layer changes. |
| `lever version` | Print the version. |

## `lever-manager` — in-jail orchestration

Run by the manager agent inside its container (baked into the agent image, on `PATH`). Every call
is authenticated by the broker over mTLS and validated against the instance config — the manager
can only reach workers the operator declared.

| Command | What it does |
|---|---|
| `lever-manager agent start NAME --task "…"` | Dispatch a declared worker with a task. The broker resolves the worker's image and workspace from the config host-side; `--task` is the only routine flag. Starting an existing suspended/stopped worker resumes it. |
| `lever-manager agent list` | List worker agents and their phases. |
| `lever-manager agent stop NAME` / `suspend NAME` / `resume NAME` | Worker lifecycle, broker-routed. Suspend keeps the container (cheap resume); stop removes it but keeps the record. |
| `lever-manager msg send "…" --to NAME` | Message a running agent. `NAME` is a worker name, or `user:manager` to reach the manager (the form taught to workers). Routing is identity-derived and default-deny. |
| `lever-manager msg list` | Read the typed agent-event inbox; `--worker <name>` reads a worker's inbox (manager only). |
| `lever-manager watch --events-file PATH` | Bridge scion agent events (state changes, `input-needed`) into a file, appending as they arrive (`--interval` seconds between polls, default 5). The manager tails this to get live worker notifications. |
| `lever-manager version` | Print the version. |

## Inside agent containers

Two more surfaces exist inside every agent container, wired up automatically at boot:

- **`lever-capability`** — an MCP tool (not a shell command) the agent's harness calls to mint
  capability tokens: `request {tool, op}` returns a token to pass as the `_capability` argument on
  gated tool calls; `delegate` mints a token bound to another agent. The scaffolded operator
  skills ([`lever init`](#setup-and-diagnosis)) teach agents this flow.
- **`lever-agent`** — the boot/enrolment binary the pre-start hook runs (key generation, broker
  enrolment, MCP registration, token renewal). Not for interactive use.
