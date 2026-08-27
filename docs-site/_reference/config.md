---
title: Configuration
nav_order: 1
---
# Config reference, `lever.yaml`

A **lever application** is described by a single config file. It declares the manager agent and
the workers (project agents) it orchestrates, plus how the jail is built around them.

The canonical filename is **`lever.yaml`**, placed at the **instance root**. The root holds the
config and the boot prompt and is **NOT** mounted; only a `tree:` **subdirectory** is bind-mounted
into the jail. This keeps the config out of the agent-writable mount, a compromised agent can't
rewrite the config the host trusts on the next bring-up.

Config is resolved from the **current directory only**, there is deliberately **no walk-up
discovery**. Run `lever` from the instance root (where `lever.yaml` lives), or pass an explicit
config path. A `lever.yaml` planted in a parent directory can therefore never be picked up. See
[security-model §5](/security-model/config-trust/).

## Layout

```
my-instance/             <- instance root: run `lever` here; NOT mounted
  lever.yaml             <- the config
  prompt.md              <- boot prompt (host-only)
  workspace/             <- tree: the bind-mounted subdir (agents edit this)
    workers/...
```

`lever-manager`, the in-jail orchestration binary, isn't staged in the tree, it's baked into the
agent image (`make lever-image-bins` + your Dockerfile's `COPY … /usr/local/bin/lever-manager`), so
it's already on `PATH` when the manager container boots.

## Minimal config

`tree:` is required and must be a confined subdirectory. **`api-key` is the default LLM-auth mode**,
so a real Console key (`broker.api_key_file`, `0600`) is required unless you opt into `subscription`:

```yaml
name: myapp
backend: orbstack
tree: workspace
manager:
  image: scionlocal/lever-claude
broker:
  api_key_file: ~/.secrets/anthropic-key   # required by the default api-key mode
```

To use your Claude OAuth token instead (the real token is projected into the agents), opt into
subscription and drop `api_key_file`:

```yaml
name: myapp
backend: orbstack
tree: workspace
broker:
  llm_auth: subscription
manager:
  image: scionlocal/lever-claude
  credential_file: ~/.scion/oauth-token    # 0600; projected to the agents
```

## Providing the Console key

`api_key_file` must hold a real **Anthropic Console key** (`sk-ant-...`), created at
[console.anthropic.com](https://console.anthropic.com) → API Keys. That's a different credential
from the Claude subscription OAuth token used in `subscription` mode (`manager.credential_file`,
above) — a Console key is billed per-token via the API, not tied to your Claude.ai session.

**Local host.** Write it straight to the `api_key_file` path at `0600`, without it ever touching
your shell history or argv:

```sh
umask 077 && mkdir -p ~/.secrets
read -rs key && printf '%s' "$key" > ~/.secrets/anthropic-key && unset key
```

`read -rs` reads the key from stdin without echoing it or putting it in a command line;
`umask 077` means the file `>` creates lands `0600` immediately, satisfying the permission check
below. (Any editor works too, as long as you set the mode afterwards: `chmod 600
~/.secrets/anthropic-key`.)

**Remote host.** Pipe the key over `ssh` so it never touches the remote shell history or a
world-readable temp file:

```sh
pbpaste | ssh user@remote-host 'umask 077; mkdir -p ~/.secrets; cat > ~/.secrets/anthropic-key'
```

The key travels over the ssh channel only, never as an argument or a remote shell variable;
`umask 077` on the remote side is what makes the file `cat` writes land `0600` before `lever
apply`'s permission check sees it. Swap `pbpaste` for however you get the key into your local
pipe (`cat ~/.secrets/anthropic-key`, a password manager's CLI, etc).

**Rotation.** Overwrite the file (keep it `0600`) and re-run `lever apply`/`lever up` — the key is
read once when the broker process starts, not per request, so a broker already running keeps using
the key it started with until it's restarted.

## Full example

```yaml
name: assistant                      # instance identity -> jail machine "lever-assistant"
backend: orbstack                    # containment backend
tree: workspace                      # bind-mounted SUBDIR (the root is not mounted)
egress: closed                       # seal the jail to the broker only (api-key instances only)
scion:
  version: e82a2a08                  # pin a scion commit; fetched + cross-compiled into the jail
manager:
  image: scionlocal/lever-claude
  model: claude-opus-5               # optional; alias or model ID for `scion start --model`
  prompt_file: prompt.md             # boot task; resolved at the ROOT (host-only, outside the mount)
  allow_ports: [3101, 3201]          # host tool ports the jail may reach
broker:
  llm_auth: api-key                  # the default; no real key in any container
  api_key_file: ~/.secrets/anthropic-key   # 0600; injected host-side by the /llm proxy
  tools:
    - name: db
      command: [lever-tool-db]       # supervised subprocess the broker proxies to
      backend: 127.0.0.1:3201        # loopback address it listens on (injected as -backend)
      operations:
        - { name: read, params: [query, table] }  # optional: declared arg names — mint rejects constraints on any other key
      allowed_values:
        table: [users, orders]       # a db capability may only be pinned to these tables
workers:
  - name: scratch
    dir: workers/scratch              # relative to tree (i.e. workspace/workers/scratch)
    # image: <ref>                   # optional; defaults to manager.image
    # model: <alias|id>             # optional; defaults to manager.model
    obtain:
      - { tool: db, op: read }       # this worker may obtain db/read capabilities
security:                            # optional image policy (both default off)
  allowed_image_registries: [scionlocal]   # only run images from these registries/namespaces
  require_image_digest: false              # true -> every image must be @sha256:-pinned
```

For subscription mode instead: drop `egress`, set `broker.llm_auth: subscription`, remove
`api_key_file`, and add `manager.credential_file` (your OAuth token, projected to the agents).

## Keys

### Top level

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `name` | string | **yes** | - | Instance identity. The jail machine is named `lever-<name>` and the manager's Scion agent slug is `<name>`. (Its capability identity at the broker is separate: `broker.manager_identity`, default `manager`.) Must match `^[a-z0-9][a-z0-9-]{0,62}$` (it becomes a machine name and a shell token). |
| `backend` | string | **yes** | - | Containment backend (the jail substrate). `orbstack` and `lima` are the *implemented* values today; any other name is **rejected at load** rather than silently substituted. Run `lever backends` for the guarantee matrix. See [containment backends](/reference/backends/). |
| `tree` | path | **yes** | - | A **confined relative subdirectory** of the instance root, bind-mounted **in place** into the jail (agents edit these real files live). Must not be `.` (the root itself is never mounted), absolute, or contain `..`. Must not itself be a git repository (a `.git` entry directly in the tree is rejected at load); a plain subdirectory inside a larger git repo is fine, ancestors are not checked. |
| `scion` | object | no | - | Where the Scion engine comes from (see below). |
| `manager` | object | **yes** | - | The manager agent (see below). |
| `workers` | list | no | `[]` | Project agents the manager orchestrates (see below). |
| `egress` | enum | no | `open` | Jail outbound network posture (`open` \| `closed`), applied jail-wide and **independent of `llm_auth`**. `open`: LAN and non-allowlisted host ports dropped, public internet reachable. `closed`: catch-all DROP so the jail reaches **only** the broker port; requires a uniformly `api-key` instance. See [security-model §2.2](/security-model/jail/). |
| `broker` | object | no | - | The host-side capability broker: LLM-auth mode, API-key file, registered tools (see below). |
| `security` | object | no | - | Optional image policy: registry allowlist and digest pinning (see below). |
| `operator` | object | no | - | Optional operator-directives config: signer trust anchor, signing key, expiry policy (see below). Unset ⇒ directives disabled. |
| `remote` | object | no | - | Optional remote-access proxy config: expose the hub web UI to a Tailscale tailnet (see below). Unset/`enabled: false` ⇒ remote access disabled. |
| `disk` | string | no | `24GiB` | **Lima only** — guest disk size (e.g. `24GiB`, `40GiB`). Lima's own omitted-disk default is 100GiB, a grow-only qcow2 that can wedge a smaller host; lever caps it at a conservative `24GiB` by default. Applied only at jail **creation**; resizing an existing jail needs `limactl disk resize` or a recreate. Ignored by the OrbStack backend, which manages its own disk. |

### `scion`

Provide **at most one** of `version`, `source` or `binary` (they are mutually exclusive). Omit all three to rely on a scion already present in the jail.

lever requires a recent scion; the examples pin a supported commit. An unsupported scion fails at
`lever apply` with an error that names the minimum. Do not work around a `value must be
base64-encoded` error by encoding the credential yourself; upgrade the pin.

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `version` | string | no | - | A scion module version/commit (a commit hash like `e82a2a08`, or a `vX.Y.Z` tag) fetched via the Go module system and cross-compiled into the jail at provision time. **No vendored source tree**, this is the recommended way to pin Scion. Requires a Go toolchain on the host — plus node >= 20 + npm when [`remote.enabled`](#remote), which builds the hub's web UI from the same fetched module (see below). |
| `source` | path | no | - | Path to a local scion source checkout, cross-compiled into the jail at provision time (for local Scion development). Relative paths resolve against the config file's directory. Requires a Go toolchain on the host, but no module fetch. Also needs node >= 20 + npm when [`remote.enabled`](#remote); the web UI is rebuilt whenever you edit `web/`. |
| `binary` | path | no | - | Path to an **already-built linux scion binary**, installed into the jail as-is. Unlike the two above it needs **no Go toolchain, no module cache and no egress** on the machine hosting the jail — and no node, since lever builds no web UI for it: with [`remote.enabled`](#remote) the hub serves whatever assets that binary embeds (upstream's `make all` embeds them), or its "Web UI Not Available" page if it embeds none — build it on a workstation and ship it. Relative paths resolve against the config file's directory. lever checks the ELF header against the guest's architecture before installing, so a wrong-arch build fails immediately instead of as `exec format error` at manager start. **Its integrity is yours to guarantee**: unlike `version`, no module-proxy checksum stands behind it. |
| `agent_role` | string | no | - | Overrides the role lever stamps on every `scion start`: `none`, `readonly`, `baseline` or `full`. **You do not need to set it.** Empty means lever picks `baseline` whenever the installed scion supports `--role` (lever probes the binary for the flag); scion's own default for an unspecified role is `full` (agent create, lifecycle and project-secret-read). `readonly` cannot heartbeat, so a live agent cannot run on it. `full` grants exactly the hub authority the jail model exists to withhold. Naming a role on a scion that has no `--role` flag is a hard error, never a silent downgrade. |

Whichever mode you use, lever records the installed binary's sha256 in the jail and skips the copy when it already matches, so an unchanged scion is not re-streamed on every `lever up`.

**With [`remote.enabled`](#remote), `version:` and `source:` additionally need node >= 20 + npm on
the host.** Neither carries built web assets, so lever builds the hub's web UI host-side from the
same source tree and stages it into the guest; `binary:` is exempt. Cache location, sizes, and
failure modes are in the [remote access guide](/remote-access/#2-make-sure-the-host-has-node).

### `manager`

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `image` | string | **in practice** | - | Container image for the manager agent. Also the default image for workers that don't set their own (see `workers[].image`). `lever apply` loads this image into the jail's container runtime. Validated for safe ref characters. Without it, agents can't start. |
| `model` | string | no | - | The LLM the manager runs on, passed to `scion start --model`: a scion alias (`small`, `medium`, `large`, `extra-large`/`xl`) or an explicit model ID (e.g. `claude-opus-5`). Also the default model for workers that don't set their own (see `workers[].model`). Omit to let the pinned scion resolve the model itself. **Create-time only:** `scion resume` has no `--model`, so changing this does not re-point an agent that already exists — only a freshly created one. Editing *only* this key also leaves a **running broker** untouched, so workers that inherit it keep the old model until the broker restarts (see *Worker model inheritance* below). |
| `prompt_file` | path | no | - | A file whose contents become the manager's boot task. Resolved at the instance **root** (host-only, **outside** the mount, so an agent can't rewrite its own next boot prompt). Must be a confined relative path (no `..`, not absolute). Omit to start with scion's default task. |
| `credential_file` | path | no | - | A file whose contents are set as the `CLAUDE_CODE_OAUTH_TOKEN` Hub secret and **projected into agent containers**. Relative paths resolve against the config file's directory; `~/` is expanded. Read at apply time with a **permission check (rejected unless the mode is `0600`-tight — no group or other bits) and size cap**. **Its contents reach every agent, point it only at a real, least-privilege, `0600` credential.** See [security-model.md](/security-model/). |
| `allow_ports` | list of int | no | `[]` | Host tool ports the jail may reach over the host alias (`host.orb.internal`). **This opens a host-loopback port to the jailed agent** — the egress allowlist is the only thing standing between the guest and whatever is listening there, so list only ports you intend the agent to reach (host-side MCP servers, etc). The broker's admin port (`broker.admin_port`, default `8444`) is rejected at config load if listed here — it is unauthenticated and meant to be reachable only from the host loopback, never from the jail. |
| `llm_auth` | enum | no | inherits `broker.llm_auth` (or `api-key`) | `api-key` (this agent holds only a `capability(llm)` token; the broker injects the real key) or `subscription` (the OAuth token is projected to this agent). **The whole instance must be uniform**: mixing `api-key` and `subscription` across manager/workers is rejected at config load (see [security-model §6.1](/security-model/credentials/)). |
| `obtain` | list of `{tool, op}` | no | `[]` | Capabilities this agent may self-obtain from the broker. `api-key` agents are auto-granted `obtain: [{tool: llm, op: generate}]`. |
| `delegate` | list of `{tool, op, to: [...]}` | no | `[]` | Capabilities this agent may mint *bound to another agent* (`to`) to hand off. A delegated token is strictly narrower than what the delegator holds. |

### `workers[]`

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `name` | string | **yes** | - | Worker identity (its agent slug / hub project name). |
| `dir` | path | **yes** | - | Worker directory, **relative to `tree`** and inside it. Mounted in place, so files the worker writes appear on the host. Must not be absolute or escape the tree (`..`). |
| `image` | string | no | `manager.image` | Container image for this worker. Set it to give a worker a different toolchain; omit to inherit the manager image (the common single-image case). `lever apply` loads **each distinct** image into the jail. |
| `model` | string | no | `manager.model` | The LLM this worker runs on. Set it to give a worker a different capability/cost point; omit to inherit the manager's model (and, if that is unset too, scion's own default). Resolved host-side from config, so the manager cannot choose it when it asks for a worker to be started. Create-time only, as for `manager.model`. |
| `llm_auth` | enum | no | inherits `broker.llm_auth` | Same as `manager.llm_auth`, per worker, but the instance-uniform rule still applies. |
| `obtain` / `delegate` | list | no | `[]` | Same shape and meaning as `manager.obtain` / `manager.delegate`. |

### `security`

Opt-in image policy applied to `manager.image` and every worker image. Both default off, so existing
configs are unaffected until you turn them on.

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `allowed_image_registries` | list of string | no | `[]` (off) | An image is allowed only if it equals, or is prefixed by `<entry>/`, one of these entries, a registry host and/or namespace prefix (e.g. `scionlocal`, `ghcr.io/myorg`). Matched on whole path components (`scionlocal` allows `scionlocal/x` but not `scionlocalevil/x`). Empty ⇒ any registry. Stops a config from running an image from an untrusted source (the image is the code that runs as the manager/worker, with the projected credential). |
| `require_image_digest` | bool | no | `false` | When `true`, every image must be pinned by **content digest** (`…@sha256:<hex>`) rather than a mutable tag like `:latest`. Guarantees you run exactly the bytes you vetted (a tag can be re-pointed to different content later). |

> **Note:** these are enforced at config-load (host side), so they bound what `lever apply` loads and
> what the manager/workers declare. `lever-manager agent start` takes no image argument; the broker
> resolves each worker's image from the validated config.

### `broker`

The host-side capability broker (outside the jail) that holds the real credential and mints
CN-bound, short-lived capability tokens. See [security-model §6](/security-model/credentials/).

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `llm_auth` | enum | no | `api-key` | Instance-wide default LLM-auth mode (`api-key` \| `subscription`), inherited by manager/workers that don't set their own. `api-key` keeps the real key host-side (the broker injects it); `subscription` projects the OAuth token into the agents. **The effective set must be uniform**, a mixed instance is a hard config error. Egress is a **separate** knob (top-level `egress:`), not implied by this mode. |
| `api_key_file` | path | **yes for `api-key`** | - | A file holding the real Anthropic Console key. Read host-side by the broker `/llm` proxy and injected into the upstream request; **never enters a container.** Must be **`0600`** (rejected otherwise), mirroring `credential_file`. See [providing the Console key](#providing-the-console-key). |
| `llm_upstream` | string (URL) | no | `https://api.anthropic.com` | Overrides the `/llm` proxy target, e.g. to route through an LLM proxy that speaks the Anthropic Messages API. **Operator-set only, never client-controlled** (no SSRF: the broker always streams to this one fixed host); it still injects the real Console key host-side and strips the inbound capability token first. |
| `jail_port` | int | no | `8443` | mTLS port the in-jail agents reach the broker on (allowlisted in the egress rules). Defaults to 8443; set an explicit port only to run several instances' brokers on one host at once. |
| `admin_port` | int | no | `8444` | **Loopback-only** unauthenticated admin port (`/register`, `/revoke`, `/bump-epoch`, `/bootstrap`, `/epoch`); bind is rejected if non-loopback. Defaults to 8444. |
| `grant_ttl` | duration | no | `24h` | Capability token lifetime. A backstop only: the per-call epoch/revocation check is the real cut, so a session-scale TTL is safe (and must outlive the 12h renew cycle). |
| `ticket_ttl` | duration | no | `10m` | Lifetime of a one-time enrolment ticket (the manager-bootstrap and agent-enrol tickets minted at apply). Short by design; only needs to outlive container boot. |
| `manager_identity` | string | no | `manager` | The capability CN the manager enrols under (its certificate identity at the broker), distinct from its Scion agent slug (`name`). |
| `auto_reenrol` | enum | no | `all` | Which agents the broker auto-heals after a **natural** mTLS-leaf lapse (`all` \| `manager` \| `off`). When an agent's short-lived client leaf ages out while renewal couldn't run (host asleep, instance idle), the broker's handshake verification proves the presented cert is its own CA's, valid in every way but time — never a revoked or foreign identity — then re-stages a fresh one-use enrolment ticket and bounces the agent so boot re-enrols (conversation preserved via resume). Attempts are audited (`op=reenrol`) and bounded (10&nbsp;min cooldown, 3 per burst). `lever revoke` always wins: revoked identities are never healed. |
| `tools` | list of `{name, command, backend, operations, allowed_values, external, gate, allow_non_loopback}` | no | `[]` | First-party / brokered tools registered for capability minting. `command` launches the supervised subprocess; `backend` is the loopback address it listens on (injected as `-backend`); `operations` are the `{name}` verbs — each may declare `params` (its argument names): when set, a capability mint whose constraint keys fall outside `params` ∪ `caveat_param` keys is rejected at mint time with an accurate error, instead of minting an over-narrowed token that fails closed only at call time (a typo'd key otherwise surfaces as a baffling `constraint not satisfied` far from the mistake); `caveat_param` (map of constraint key → request argument name, for non-identity mappings such as `table: schema.table`) is an optional declared guard: at registration the broker checks the tool-shipped caveat-param map equals it and rejects the tool (403) otherwise; its keys also count as valid constraint keys for the `params` check. Omit to accept whatever the tool ships; `allowed_values` restricts a constraint key to a permitted set (e.g. `table: [A, B]`), enforced at mint. With `external: true` the broker FRONTS an already-running host MCP server instead of spawning one: no `command`, `backend` is the server's own listen address (`host:port[/path]`, literal loopback IP unless `allow_non_loopback: true`), and the tool registers third-party — the broker enforces the rules and strips the capability before proxying. |
| `messaging` | object | no | `worker_to_worker: true` | Routing policy for broker-routed typed messaging (`/msg/send`, `/msg/list`; see [architecture.md](/architecture/)). `worker_to_worker` (bool) permits worker→worker sends; it's a pointer under the hood so unset ⇒ **allowed**, an explicit `false` denies it for a stricter hub-and-spoke model. Recipients themselves aren't a config key, they're resolved from the caller's mTLS identity: the manager may message any declared worker and read any inbox (`msg list --worker <name>`); a worker may always message the manager and read only its own inbox. |

#### External MCP servers (`external: true`)

An **external tool** is a host MCP server the broker *fronts but does not spawn* — it keeps
running as your own user-session process (launchd, a terminal, however you run it), which is
what keeps macOS Automation/TCC grants intact for AppleScript-driven servers. The broker
registers it from config at boot, exposes it at `/mcp/<name>/` on its mTLS listener, and
proxies to `backend`. Jailed agents therefore reach it **only through a capability** — no
`manager.allow_ports` hole, no hand-authored `.mcp.json`.

Per-tool capability grain, `gate`:

- **`fine` (default):** only the MCP tools listed under `operations` are callable; a token
  must name the specific operation, and `allowed_values` can pin arguments.
- **`coarse`:** one wildcard grant — `{tool: <name>, op: "*"}` — admits **every** MCP call
  the server exposes (declare no `operations`). The wildcard is honored *only* for a
  `gate: coarse` tool: the broker chooses which capability to require, so a `"*"` token
  can never widen a `fine` tool. The audit log records the real MCP tool called either way.

```yaml
broker:
  tools:
    - name: devonthink            # fine: only search is callable, database pinnable
      external: true
      backend: 127.0.0.1:3302
      operations:
        - {name: search}
      allowed_values:
        database: [work, personal]
    - name: things3               # coarse: whole surface behind one wildcard grant
      external: true
      backend: 127.0.0.1:3300
      gate: coarse
    - name: qmd                   # the server mounts its MCP endpoint under a path
      external: true
      backend: 127.0.0.1:3101/mcp
      gate: coarse
workers:
  - name: agent-y
    dir: workers/agent-y
    obtain:
      - {tool: devonthink, op: search}   # Y: devonthink search ONLY
manager:
  obtain:
    - {tool: things3, op: "*"}           # manager: full things3
```

`backend` must be a **literal loopback IP** (`127.0.0.1` / `[::1]`; hostnames are rejected):
the broker proxies host-side, so a non-loopback backend would let a jailed agent reach
another host *through the broker*, bypassing the jail's LAN-drop egress. If you truly need
that, set `allow_non_loopback: true` on the tool — an explicit, per-tool opt-in.

Liveness is yours: the broker does not restart an external server; if it is down, calls to
the tool return 502.

### `operator`

Optional host-side config for **operator directives** — authenticated delivery of an
operator-signed action to a named agent, verified by the broker against a trust anchor you
control (see the `lever directive` command group in the [CLI reference](/reference/cli/)). The
whole block is optional: with `allowed_signers` unset, directives are **disabled** (the channel
is simply not configured, not half-configured).

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `allowed_signers` | path | no | - | An `ssh-keygen` `allowed_signers` file listing keys trusted to sign directives, under the fixed principal `operator@<instance-name>`. A **confined path under the instance dir**, host-only — keep it **out of** the mounted `tree:`, so a compromised jailed agent can neither read it nor alter the trust anchor. Live-editable: the broker shells out to `ssh-keygen -Y verify` per call, so a key added/removed here takes effect with no restart. |
| `signing_key` | path | no | - | Default private key `lever directive send` signs with (`--key` overrides it per call). A **host path, not confined** to the instance dir — keep it outside the tree entirely, and gitignore it. |
| `directive_expiry` | duration | no | `10m` | Default directive lifetime when `lever directive send` is called without `--expires`. |
| `directive_expiry_max` | duration | no | `24h` | Hard cap on directive lifetime; a `--expires` (or `directive_expiry`) asking for more is rejected. |

```yaml
operator:
  allowed_signers: operator_allowed_signers   # ssh-keygen allowed_signers file, confined to the instance dir
  signing_key: /abs/path/to/operator_key      # default signing key for `lever directive send`; NOT confined
  directive_expiry: 10m                       # optional; default 10m
  directive_expiry_max: 24h                   # optional; default 24h
```

The operator principal is fixed as `operator@<instance-name>`, one principal per instance. Key
posture (where to keep the key, multi-key break-glass, no agent forwarding) is in
[security model §11.5](/security-model/operator-directives/); setup and usage are in the
[operator directives guide](/operator-directives/).

### `remote`

Optional config for **`lever remote`**: a host-side reverse proxy that exposes the Scion hub web UI
(chat, transcript, xterm.js attach) to a Tailscale tailnet, injecting a dedicated PAT so the client
never holds a credential. The whole block is optional and disabled by default — see the [remote
access guide](/remote-access/) for the accepted security posture, setup steps, and repair.

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `enabled` | bool | no | `false` | Turns the proxy on. Also needs a **Go toolchain on the host** — it cross-compiles the guest's login forwarder at apply time; with `scion.binary:` (the only mode that did not already need one) a missing `go` is rejected at config load. Requires `backend: orbstack` — rejected at config load on any other backend, because the Lima path is not live-validated yet. This is not a reachability limit: the proxy dials the hub through the jail, so no guest→host forwarding is involved. Also requires **node >= 20 + npm on the host** with [`scion.version`](#scion) or `scion.source`: neither carries built web assets, so lever builds the UI host-side and stages it into the guest (`binary:` is exempt). `lever doctor` checks it. |
| `port` | int | no | `8445` | Host loopback port the proxy binds. Must not collide with the broker's `jail_port`/`admin_port`, with `login_port`, or with `8446` (the host port the jail's login forwarder is mirrored onto) — all rejected at config load. Also rejected if listed in `manager.allow_ports`: the proxy trusts the `Tailscale-User-Login` header because only `tailscale serve` can reach its loopback listener, and an allow_ports hole would let the jail reach it directly. Give each remote-enabled instance on the host its own port: the proxies reach their own jails, so only the host listeners are shared ground. |
| `login_port` | int | no | `8447` | **Host** loopback port the local OIDC provider binds — the login path that gets the browser a hub session with no external identity provider and no dev-auth. The port the HUB dials is a different, fixed one inside the guest (`8446`, no config key): Scion validates `issuer_url` at hub startup and refuses to start unless it is loopback, so a forwarder listens there and carries the bytes to this port. The two must differ because the container runtime mirrors a guest listener onto the host at the same number — one number for both halves left the provider unable to bind. Rejected at config load if it collides with `port`, the broker's listeners, or `8446` (the guest forwarder's host mirror). A second remote-enabled instance needs its own value. See the [guide](/remote-access/#how-the-browser-is-logged-in). |
| `base_url` | string (URL) | **yes when `enabled`** | - | The tailnet serve hostname, as an absolute `https://` URL, e.g. `https://myhost.tailxxxx.ts.net`. **Rejected at config load if empty while `enabled: true`**: the proxy matches every request's `Origin` against the host this resolves to, and an empty `base_url` would fail closed on every request — a proxy that could never serve anything, so lever refuses to load that config at all. It configures **only the proxy**; lever deliberately never passes it to the hub as `--base-url` (see the [guide](/remote-access/#the-tailnet-url-never-reaches-the-hub)). |
| `allowed_users` | list of string | no | `[]` | Optional identity pinning: when non-empty, only these Tailscale login names (matched against the `Tailscale-User-Login` header `tailscale serve` sets) may reach the proxy. Empty ⇒ anyone reachable through the tailnet. A blank entry is rejected at config load. It also decides the identity the login path asserts to the hub, so the hub's user row names the operator who actually connected — with it unset, a placeholder identity is used instead. |

```yaml
remote:
  enabled: true
  base_url: "https://myhost.tailxxxx.ts.net"   # required whenever enabled — see above
  # port: 8445                                 # optional; default shown
  # login_port: 8447                           # optional; the local OIDC provider's HOST port
  # allowed_users: ["you@github"]              # optional identity pinning
```

## Conventions & derived values

- **Machine name:** `lever-<name>`. `up`/`apply`/`stop`/`destroy`/`doctor` all agree on this, derived
  from the config (override on `stop`/`destroy`/`doctor` with `--machine`). `lever down` is a
  deprecated alias of `destroy`.
- **Worker model inheritance:** a worker with no `model:` runs on `manager.model`; with neither
  set, lever passes no `--model` and the pinned scion resolves the model. The model is fixed when
  an agent is created (`scion resume` takes no `--model`), so editing it only affects agents
  created afterwards. The broker resolves the inheritance once, when it starts, and the hash that
  decides whether `lever apply` restarts the broker covers `broker:`/`workers:`/`scion:` but
  deliberately not `manager:` — so an edit to `manager.model` alone reaches a newly created worker
  only after the broker restarts for another reason. Setting `workers[].model` explicitly is the
  direct route. This is the same trade-off `image:` inheritance already makes.
- **Worker image inheritance:** a worker with no `image:` runs on `manager.image`. The bring-up plan
  loads every distinct image once (deduped). At dispatch the capability broker reads the config
  directly and supplies each worker's resolved image, so `agent start NAME` takes no image argument.
  The manager names a configured worker; the broker resolves paths, image and credentials host-side.
- **In-place mounts:** the `tree` subdir (and each `workers[].dir` within it) is bind-mounted into the
  jail so agents edit the real host files. There is no copy/sync step.
- **Path resolution base:** relative `tree`/`prompt_file`/`scion.source`/`credential_file` resolve
  against the **config file's directory**, never the shell's current directory.

## See also

- [Getting started](/getting-started/), build a working instance from scratch.
- [Security model](/security-model/), trust boundaries, threat model, and the credential flow
  these keys drive.
