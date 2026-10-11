---
title: CLI
nav_order: 3
---
# CLI reference

Lever ships two binaries with user-facing commands: **`lever`**, the host control plane you run
from an instance root, and **`lever-manager`**, the in-jail orchestration CLI the manager agent
runs inside its container. (The other binaries have no operator-facing commands: `lever-agent`,
baked into agent images and run by the container pre-start hook, and the capability-aware MCP
tools the broker launches and supervises when a `broker.tools:` entry names them in its `command`:
`lever-tool-db` (the reference tool), [`lever-tool-github`](/github-tool/) and
[`lever-tool-fizzy`](/fizzy-tool/). Those tools take their settings as flags in that `command`.)

All `lever` commands read `./lever.yaml` from the current directory when the config argument is
omitted — there is no walk-up discovery, so run them from the instance root.

## `lever` — host control plane

### Everyday lifecycle

| Command | What it does |
|---|---|
| `lever up [config]` | Bring the application up *if needed* (create jail, provision scion, start the manager) **and attach** the manager's TTY. Idempotent: re-running resumes a suspended manager and re-attaches — same conversation, even across a `lever stop` or host reboot. `--fresh` starts a new manager thread: the bring-up deletes any existing record once the hub is up and creates the manager anew — also after a `lever stop`, when the hub is down at the start of `up` and the record cannot be seen yet (the discard is announced). Because a record keeps the image it was created with, `--fresh` is also how a changed `manager.image` reaches the manager. `--no-attach` brings up without taking your terminal. The everyday entry point. |
| `lever attach [name]` | Attach your TTY to the manager (default) or a named worker. Strictly passive: fails fast with "run `lever up` first" if the jail isn't up — it never provisions. Detach with `Ctrl-b d`. |
| `lever msg send "…" --to NAME` | Host-side fire-and-forget note to the manager (use the app name) or a declared worker — no attach needed. The note goes through the running broker's operator socket, which records it (the agent verifies it as an `operator-note`) before it sends it; with no broker running the command refuses. The note lands as the agent's next user turn; it acts on it unattended and the exchange waits in the scrollback for your next attach. `--interrupt` injects it ahead of the agent's next turn. |
| `lever reload [config]` | Apply config changes (new worker, tool, or grant) to a **running** instance without a VM power cycle: stops the broker, re-runs the idempotent apply on the current config, spawns a fresh broker. The manager container keeps running, so its conversation is preserved and your TTY isn't taken. Needed because the broker reads `lever.yaml` only at startup — a plain re-`apply` keeps the old broker. |
| `lever stop` | Power the jail off but **keep its disk** — the daily "done for the day". Suspends the manager (conversation preserved) and every running worker, stops the host broker; a later `lever up` powers it back on and resumes the same session. Installed runtimes and scion state persist. `--machine`/`--backend` target another jail (the broker is then not stopped). |
| `lever destroy` | Full teardown: delete the isolated machine and everything in it. Targets `lever-<name>` from config; override with `--machine`. `lever down` is a deprecated alias. |
| `lever worker purge NAME` | Discard a declared worker's scion record and staged bootstrap ticket so it can be re-dispatched fresh with a new task — a worker's task is fixed at creation, so `lever-manager agent start` against an existing worker returns 409. Never touches the worker's workspace (its work product); requires `--force`. Host-side (operator) command. The in-jail manager cannot purge, but it can recycle a worker the config marks `recyclable: true` (`lever-manager agent recycle`, which runs the same teardown). |

### Setup and diagnosis

| Command | What it does |
|---|---|
| `lever init` | Scaffold/refresh the framework operator skills (SKILL.md) into the instance tree — `lever-operator` at the tree root, `lever-agent` in each declared worker dir — plus a marked reference block in the tree-root CLAUDE.md. Hash-guarded: files you've edited are left alone with a warning (`--force` overwrites); `--check` reports staleness without writing (non-zero exit); `--adopt` records your customizations as an accepted baseline so doctor and `--check` treat them as OK — later drift past that baseline still fails doctor (tamper detection: agents can write these files, the baseline lives host-side). Re-run after upgrading lever or adding a worker. |
| `lever doctor` | Run real health checks — broker alive and serving, every declared tool backend reachable/resolvable (external servers dialed, supervised commands resolved on the supervisor PATH), the agent image's baked Claude Code version (read from the host Docker store, or from the `image_tar` archive when the image ships as one), the lever release the image's in-jail binaries were built from matching the host `lever` (the image's `lever_version` label; an image without the label is not checked), the manager record's image matching `manager.image` (a resume keeps the record's image; a drift points at `lever up --fresh`), agent telemetry in the jail's scion settings matching `scion.telemetry` and the manager's `SCION_TELEMETRY_ENABLED` agreeing with it (a manager started before the change points at `lever stop && lever up`), manager credential file presence/size/mode, no stray `.mcp.json` in the tree, usable Go toolchain, scion project-registration consistency, operator-skills scaffold current, operator directives configured (signer key count, admin socket present), remote-access proxy when enabled (pid/listen, PAT presence + mode, an end-to-end `GET /healthz` through the proxy), the hub tokens' records (scope set current, more than 30 days of life left — a token without a record fails), the remote web role recorded for every allowed user when remote access is on, no shared directory on the project, every agent record carrying a stored role that scion's migration did not grandfather, and the project's agent-role ceiling set to the role lever stamps. Further rows cover the manager's liveness and activity, each agent's hub token, certificate and network mode, worker ticket mounts and tree bootstraps, the workers the manager may recycle (*worker recycle*, never a failure), the dev-auth window, the guest clock and guest DNS, the node toolchain, `manager.read_only` mounts (*manager read-only paths*), each agent's `shared_folders` mounts (*shared folders*), `nested_virt` (*nested virt*), the remote proxy's exposure, and the ledgers and settings behind the chat page (*sent ledger*, *verified chat*, *agent messages*, *files*, *push*, *chat labels*). Each failure prints a specific fix hint. Exits non-zero on any failure. `--machine`/`--backend` run the profile away from an instance root. |
| `lever apply [config]` | Headless bring-up: runs the full ordered plan (jail → broker → images → init-machine → config-registry → bootstrap-token (a throwaway dev-auth hub mints the controller PAT) → scion-server (dev-auth off) → remote-proxy (only when `remote.enabled: true`) → credential (only when `manager.credential_file` is set) → register-project (one registration for the tree) → agent-template → mint-manager-bootstrap → start-manager) with no attach. `--dry-run` prints the plan and exits with no side effects (a `load-image` step names the archive it streams from when `image_tar` is set). Images come from the host Docker store, or from a `docker save` archive named by `image_tar` (no host Docker needed). The non-interactive half of `up`, for scripts and scheduled runs. |
| `lever provision` | Low-level: provision the jail only (create the isolated machine, install runtimes + scion, apply egress rules). `--machine` (default `lever-jail`), `--tree` (required), `--backend`, `--allow-port` (repeatable). Idempotent; rarely needed directly — `up`/`apply` call it for you. |
| `lever backends` | List the containment backends (orbstack, lima) and the guarantees each declares — the matrix config validation checks your `backend:` choice against. |

### Broker operations

| Command | What it does |
|---|---|
| `lever broker serve [config]` | Run the capability broker + first-party tools in the foreground (normally `up`/`apply` daemonize it for you — this is for debugging and supervised setups). |
| `lever broker revoke <agent> [config]` / `lever revoke <agent>` | Revoke one agent on the running broker: its capability tokens stop verifying immediately. |
| `lever broker bump-epoch [config]` | Revoke **all** outstanding tokens at once by raising the epoch floor. |
| `lever acceptance [config]` | Bring up a real jail and drive the six live capability/egress acceptance checks — delegated-read, three scope-envelope denials (a disallowed table, a dropped narrowing filter, a worker self-minting an un-granted cap), egress-refused (allowlisted broker port reachable, admin port blocked), and revocation (a token stops working after `bump-epoch`) — the merge gate for capability-layer changes. |
| `lever version` / `lever --version` | Print the version: the release (`X.Y.Z`), plus the build's provenance in parentheses when there is one: the commit (first 12 hex digits, `-dirty` for an uncommitted tree) for a local build, the module version for a `go install …@vX` build. A build with no VCS stamp (a build in a git worktree, where Go records none) and a dirty build add `bin <hash>`, the first 12 hex digits of the binary's SHA-256, e.g. `X.Y.Z (bin 3f2a91c04e7b)`. `apply` restarts the broker and the remote proxy when this string changes. |

### Exit codes

`lever` exits `0` on success and `1` on an error, with two exceptions from `lever up` and
`lever apply` that a script can tell apart by the code (never by the error text, which can carry
text from the jail):

| Code | Meaning |
|---|---|
| `3` | lever could not resume the manager. The record and its conversation are kept. Run `lever up` again (a transient failure clears) and `lever doctor` for the cause; `lever up --fresh` discards the conversation. |
| `4` | The hub refused the resume (a token without the lifecycle scope, or a phase that cannot be resumed now). The manager is kept. Do **not** answer this with `--fresh`. |

`lever doctor` and `lever init --check` exit `1` when a check fails.

### Operator directives

Authenticated delivery of an operator-signed action to a target agent — see the `operator:` block
in the [config reference](/reference/config/) for the signer trust anchor and expiry defaults, and
[security-model.md](/security-model/) for the verification mechanism. Like most `lever`
commands, `directive` subcommands resolve `[CONFIG]` from an explicit argument or `./lever.yaml`
in the current directory.

| Command | What it does |
|---|---|
| `lever directive send <agent> (--instruction TEXT \| --action JSON) [--expires DUR] [--key PATH] [--not-before RFC3339] [CONFIG]` | Sign and submit a directive to `<agent>` (the manager's app name, or a declared worker) over the broker's host-only admin socket. `--instruction`/`--action` are mutually exclusive, exactly one required. `--expires` defaults to `operator.directive_expiry`, capped by `operator.directive_expiry_max`. `--key` defaults to `operator.signing_key`. Prints the exact statement bytes it signs, for operator review, before sending. `<agent>` may also be the literal `manager`. A `--not-before` more than 2 minutes in the future is refused before the broker is contacted (the broker enforces the same limit). Exits non-zero when the target is not enrolled or not running, or when the notice does not reach the agent (the broker then revokes the directive). See [operator directives](/operator-directives/). |
| `lever directive list [--state active\|consumed\|revoked\|invalidated\|expired] [--key PATH] [CONFIG]` | List directives and their state. |
| `lever directive revoke <id> [--key PATH] [CONFIG]` | Revoke a directive by id. |
| `lever directive selftest [--key PATH] [CONFIG]` | Round-trip a self-signed test directive (sign → verify) against the configured `allowed_signers`, to catch misconfiguration before it's needed for real. |

### Remote access

Exposes the Scion hub web UI to a Tailscale tailnet through a host-side, credential-injecting
reverse proxy — see the `remote:` block in the [config reference](/reference/config/) for the
options, and the [remote access guide](/remote-access/) for the accepted security posture and full
setup (including the one `tailscale serve` command lever asks you to run). Like most `lever`
commands, `remote` subcommands resolve `[CONFIG]` from an explicit argument or the current
directory.

| Command | What it does |
|---|---|
| `lever remote serve [CONFIG]` | Run the proxy in the foreground. Normally `up`/`apply` daemonize this for you when `remote.enabled: true` (`lever stop` stops it alongside the rest of the instance); this is for debugging. Refuses to start with remote access disabled. Prints the remote-settings warnings (a non-loopback `bind`, `trust_forwarded_host`) to stderr, which is `.lever-state/remote.log` when daemonized. |
| `lever remote status [CONFIG]` | Proxy liveness (pid + listening on its bind address), the identity header, the `tailscale serve` command to run (loopback bind only), any remote-settings warnings, the serve URL from `base_url`, and whether the remote PAT is present — never its value. |

## `lever-manager` — in-jail orchestration

Run by the manager agent inside its container (baked into the agent image, on `PATH`). Every call
is authenticated by the broker over mTLS and validated against the instance config — the manager
can only reach workers the operator declared.

| Command | What it does |
|---|---|
| `lever-manager agent start NAME --task "…"` | Dispatch a declared worker that has **no** existing record. The broker resolves the worker's image and workspace from the config host-side; `--task` is the only routine flag and fixes the worker's task at creation; it defaults to `"Read your context, then begin."`, so `agent start` never sends an empty task. Against an **existing** worker `agent start` returns HTTP 409 (its task can't be changed in place) — use `agent resume` to re-run its original task, `msg send` to give a running worker new work, or, to start it fresh with a new task, `agent recycle` it (a `recyclable` worker) or ask the operator to `lever worker purge` it. Confirms the worker is live before reporting success. |
| `lever-manager agent recycle NAME --task "…"` | Discard a worker's record and start it fresh with a new task, in one call: the same teardown as `lever worker purge` (the scion record and the staged ticket; **never the workspace**), then the same start as `agent start`. The worker's old conversation is lost. Only for a worker with `recyclable: true` in the config (403 otherwise), and only for one that is suspended, stopped or in phase `error` (409 otherwise, nothing deleted: `agent stop` it first); a worker with no record is simply started. Refused with 503 while another start, resume, stop, wake or heal of the worker is under way, and with 429 (and `Retry-After`) within a minute of the worker's last recycle. Every recycle is in the broker audit log (`op=worker`, `recycle NAME`, the task's size and its first 60 bytes only). `--task` defaults as for `agent start`. |
| `lever-manager agent list` | List worker agents and their phases. |
| `lever-manager agent stop NAME` / `suspend NAME` / `resume NAME` | Worker lifecycle, broker-routed. Suspend keeps the container (cheap resume); stop removes it but keeps the record. Resume answers when the worker is live (it recovers an `error`-phase record, and is a no-op for a running one); a worker must be running before `msg send` reaches it (409 otherwise). |
| `lever-manager msg send "…" --to NAME` | Message a running agent. `NAME` is `<name>` or `agent:<name>` for a worker, or `user:manager` to reach the manager (the form taught to workers). `--interrupt` injects the message ahead of the recipient's next turn. Routing is identity-derived and default-deny; a refused send names the addresses the caller may use. |
| `lever-manager msg recipients` | List the addresses the calling agent may pass to `msg send --to`, one per line, as the broker's policy has them: the manager sees itself and every declared worker; a worker sees `user:manager`, itself, and the other workers only when `messaging.worker_to_worker` allows them. Read-only. |
| `lever-manager msg list` | Read the typed agent-event inbox; `--worker <name>` reads a worker's inbox (manager only). `--all` includes already-read events. Each event's message starts with `worker-reported:` (the worker's own text, sanitized and cut to 1 KiB); a status the hub does not produce shows as `UNRECOGNISED`. |
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
