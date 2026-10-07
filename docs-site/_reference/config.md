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
  version: 63d5d65d                  # pin a scion commit; fetched + cross-compiled into the jail
manager:
  image: scionlocal/lever-claude
  # image_tar: images/lever-claude.tar   # optional; ship the image as a docker archive, no host docker needed
  model: claude-opus-5               # optional; alias or model ID for `scion start --model`
  prompt_file: prompt.md             # boot TASK (first user turn, keep it short); resolved at the ROOT (host-only, outside the mount)
  instructions_file: manual.md       # standing instructions -> the agent's CLAUDE.md, via a file, never argv; same resolution
  read_only: [tools]                 # tree dirs the MANAGER sees read-only (e.g. code the host also runs); create-time only
  # Host ports the jail may reach DIRECTLY (an MCP server you run yourself).
  # Never a first-party tool's backend port (3201 below): the jail would dial
  # the tool past the broker, and config load rejects it.
  allow_ports: [3101]
broker:
  llm_auth: api-key                  # the default; no real key in any container
  api_key_file: ~/.secrets/anthropic-key   # 0600; injected host-side by the /llm proxy
  tools:
    - name: db
      command: [lever-tool-db]       # supervised subprocess the broker proxies to
      backend: 127.0.0.1:3201        # loopback address it listens on (injected as -backend); reachable from the jail only via the broker
      operations:
        - { name: read, params: [query, table] }  # optional: declared arg names — mint rejects constraints on any other key
      allowed_values:
        table: [users, orders]       # a db capability may only be pinned to these tables
workers:
  - name: scratch
    dir: workers/scratch              # relative to tree (i.e. workspace/workers/scratch)
    # image: <ref>                   # optional; defaults to manager.image
    # image_tar: <path>             # optional; the archive shipping THIS worker's image (needs image:)
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
| `nested_virt` | bool | no | `false` | **Lima on an x86_64 (Intel/AMD) Linux host only.** The manager container gets `/dev/kvm`, so the manager can run KVM guests (e.g. Lima, to test lever itself) inside its container. Workers and the hub do not get the device. Apply refuses on a host that is not x86_64, or when the host's KVM `nested` module parameter (`kvm_intel`/`kvm_amd`) is off, and installs a udev rule (`/dev/kvm` mode 0666 in the guest). The manager gets the device as a `/dev/kvm` bind-mount volume on its scion record. The jail VM must be created with it (create-time `vmOpts.qemu.cpuType: host`); an existing VM without `/dev/kvm` needs a recreate (back up the conversation, `lever destroy`, `lever up`). The volume is create-time only: an existing manager gets or drops the device only on a fresh create (back up the conversation, then `lever up --fresh`); `lever stop` + `lever up` keeps the old volumes, and apply warns. Turning it off removes the udev rule and sets the device back to mode 0660. Apply also removes the podman drop-in `20-lever-kvm.conf` that earlier builds wrote (it gave every container the device; workers created while it was there keep the device until they are recreated). Doctor row *nested virt*. **Security:** exposes the host kernel's nested-virtualization code to the jail guest, and the manager's `/dev/kvm` ioctl surface to the guest kernel; use it only for dev instances and keep the host kernel patched. Rejected on OrbStack and on darwin. |
| `cpus` | int | no | Lima default | **Lima only** — guest vCPUs (1-256). Create-time only: apply warns when an existing VM has a different value. |
| `memory` | string | no | Lima default | **Lima only** — guest memory, e.g. `24GiB` (minimum 2GiB). Create-time only: apply warns when an existing VM has a different value. |

### `scion`

Provide **at most one** of `version`, `source` or `binary` (they are mutually exclusive). Omit all three to rely on a scion already present in the jail.

lever requires a recent scion; the examples pin a supported commit. An unsupported scion fails at
`lever apply` with an error that names the minimum. Do not work around a `value must be
base64-encoded` error by encoding the credential yourself; upgrade the pin.

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `version` | string | no | - | A scion module version/commit (a commit hash like `63d5d65d`, or a `vX.Y.Z` tag) fetched via the Go module system and cross-compiled into the jail at provision time. **No vendored source tree**, this is the recommended way to pin Scion. Requires a Go toolchain on the host — plus node >= 20 + npm when [`remote.enabled`](#remote), which builds the hub's web UI from the same fetched module (see below). |
| `source` | path | no | - | Path to a local scion source checkout, cross-compiled into the jail at provision time (for local Scion development). Relative paths resolve against the config file's directory. Requires a Go toolchain on the host, but no module fetch. Also needs node >= 20 + npm when [`remote.enabled`](#remote); the web UI is rebuilt whenever you edit `web/`. |
| `binary` | path | no | - | Path to an **already-built linux scion binary**, installed into the jail as-is. Unlike the two above it needs **no Go toolchain, no module cache and no egress** on the machine hosting the jail — and no node, since lever builds no web UI for it: with [`remote.enabled`](#remote) the hub serves whatever assets that binary embeds (upstream's `make all` embeds them), or its "Web UI Not Available" page if it embeds none — build it on a workstation and ship it. Relative paths resolve against the config file's directory. lever checks the ELF header against the guest's architecture before installing, so a wrong-arch build fails immediately instead of as `exec format error` at manager start. **Its integrity is yours to guarantee**: unlike `version`, no module-proxy checksum stands behind it. |
| `agent_role` | string | no | - | Overrides the role lever stamps on every `scion start`: `none`, `readonly`, `baseline` or `full`. **You do not need to set it.** Empty means lever picks `baseline` whenever the installed scion supports `--role` (lever probes the binary for the flag); scion's own default for an unspecified role is `full` (agent create, lifecycle and project-secret-read). `readonly` cannot heartbeat, so a live agent cannot run on it. `full` grants exactly the hub authority the jail model exists to withhold. Naming a role on a scion that has no `--role` flag is a hard error, never a silent downgrade. The same role is written as the project's maximum and default agent role on the hub, so the hub refuses a create above it whoever asks (see [security model §4.4](/security-model/worker-isolation/)). |
| `telemetry` | enum | no | `off` | Agent telemetry posture (`off` \| `scion-default`), written into the jail's `~/.scion/settings.yaml`. `off` writes `telemetry.enabled: false`, so every agent starts with `SCION_TELEMETRY_ENABLED=false`. `scion-default` leaves the key to scion and to you. See [agent telemetry](#agent-telemetry-sciontelemetry) below. |

Whichever mode you use, lever records the installed binary's sha256 in the jail and skips the copy when it already matches, so an unchanged scion is not re-streamed on every `lever up`.

#### Agent telemetry (`scion.telemetry`)

Since scion#1792, sciontool telemetry is on by default, and cloud export is on by default too.
lever configures no cloud destination, so the OTLP receiver that `sciontool init` starts in each
agent container refuses to start. Every `sciontool hook` call still tries to export to
`127.0.0.1:4317` in the container. The call then waits out a 10 s export timeout and a 5 s
shutdown timeout. Claude Code fires 8 hook events, and Pre/PostToolUse fire on every tool call.
A trivial turn took more than two minutes.

- **`off` (the default).** `lever apply` writes this top-level block into the jail's
  `~/.scion/settings.yaml`, and keeps all other keys and comments:

  ```yaml
  telemetry:
    enabled: false
  ```

  If the block has other keys, lever keeps them and sets only `enabled: false`. An explicit
  `enabled: true` is overridden, because this key is the switch. (`scion config set` refuses this
  key, so lever edits the file.) scion turns the block into `SCION_TELEMETRY_ENABLED=false` for
  every agent that it starts.
- **`scion-default`.** lever does not manage the key. It removes only the exact block that `off`
  wrote, and leaves a block with more keys unchanged. Use this mode only when you configure scion
  telemetry yourself. A cloud destination adds egress and a credential in the jail, so lever does
  not manage it.

**The setting applies when an agent starts.** The runtime broker reads the file at each agent
start. A running container keeps the env that it started with, and the hub reads the block only at
its own startup. When apply changes the file, it logs the change and restarts nothing. Run
`lever stop && lever up` to apply the change to the hub and the manager. The manager
conversation is kept. The `scion telemetry` row of `lever doctor` shows the setting in the jail. It
also shows the `SCION_TELEMETRY_ENABLED` value that the manager container started with, and it
fails when the two do not agree.

**lever has no `local` mode.** With cloud export off (`SCION_TELEMETRY_CLOUD_ENABLED=false`),
the in-container receiver starts on `127.0.0.1:4317`/`4318` with no new egress. But the receiver
drops every span and log that it accepts, because sciontool has no local file or console sink. The
`telemetry.local` settings produce env vars that no sciontool code reads. Also, scion's Claude
provisioner refuses an enabled telemetry block that names no cloud provider or endpoint. A local
mode would thus add work to every hook and give no data.

**With [`remote.enabled`](#remote), `version:` and `source:` additionally need node >= 20 + npm on
the host.** Neither carries built web assets, so lever builds the hub's web UI host-side from the
same source tree and stages it into the guest; `binary:` is exempt. Cache location, sizes, and
failure modes are in the [remote access guide](/remote-access/#2-make-sure-the-host-has-node).

### `manager`

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `image` | string | **in practice** | - | Container image for the manager agent. Also the default image for workers that don't set their own (see `workers[].image`). `lever apply` loads this image into the jail's container runtime. Validated for safe ref characters. Without it, agents can't start. |
| `image_tar` | path | no | - | A docker archive (`docker save` output) that ships `image`. When set, `lever apply` streams this file straight into the jail's container runtime and never touches the host docker store — a deploy host needs no docker at all, and the "already loaded" skip compares the archive's config digest with the jail's image ID. The archive must carry `image`'s tag (a mismatch is a named error; a multi-image archive is fine); `image` cannot be digest-pinned. Same resolution and confinement as `prompt_file`: instance root, host-only, **outside** the mounted `tree` (enforced), since the archive is the code the agent runs. `lever doctor` reads the baked Claude Code version from the archive instead of docker. Add it to `.gitignore` when the root is a repository. |
| `model` | string | no | - | The LLM the manager runs on, passed to `scion start --model`: a scion alias (`small`, `medium`, `large`, `extra-large`/`xl`) or an explicit model ID (e.g. `claude-opus-5`). Also the default model for workers that don't set their own (see `workers[].model`). Omit to let the pinned scion resolve the model itself. **Create-time only:** `scion resume` has no `--model`, so changing this does not re-point an agent that already exists — only a freshly created one. Editing *only* this key also leaves a **running broker** untouched, so workers that inherit it keep the old model until the broker restarts (see *Worker model inheritance* below). |
| `prompt_file` | path | no | - | A file whose contents become the manager's boot task — the agent's **first user turn**. Resolved at the instance **root** (host-only, **outside** the mount, so an agent can't rewrite its own next boot prompt). Must be a confined relative path (no `..`, not absolute). Omit to start with scion's default task. **Keep it short:** the task travels on scion's command line, where tmux caps the whole start command at 16 KiB, so `lever apply` refuses a prompt over a 15 KiB budget (bytes plus 3 per apostrophe) by name. Must resolve **outside** the mounted `tree` (enforced). Standing instructions belong in `instructions_file`. |
| `instructions_file` | path | no | - | A file whose contents become the manager's **standing instructions** — who it is and how it must operate — delivered through scion's `agent_instructions` channel into the agent's **user-level** `~/.claude/CLAUDE.md` (a managed block, beside any project `CLAUDE.md` in the tree), never on the command line, so it has no size ceiling of the kind `prompt_file` has (it is capped at 512 KiB, half of scion's 1 MiB hub-request limit). Same resolution and confinement as `prompt_file`: instance root, host-only, and **outside** the mounted `tree` (enforced). Must not begin with `file://`, which scion would read as a file reference. **Create-time only:** scion stages the text when it provisions the agent and re-projects that staged copy on every later start (an agent that edits its own managed block gets it back at the next start), but an edited file reaches only a freshly created agent, not one being resumed. |
| `read_only` | list of path | no | `[]` | Tree directories the **manager** container sees **read-only**, each relative to `tree` with forward slashes (e.g. `assistant/tools`). The manager otherwise mounts the whole tree read-write, so keep anything the **host** also runs (an operator CLI, scripts) under one of these: an agent that could rewrite such code would run it on the host the next time you do. Each entry is bind-mounted read-only over itself, and each directory between the tree root and an entry is bind-mounted read-write over itself: a mount point cannot be renamed or removed, so the agent cannot move the protected directory away and create a writable one at its host path. **While it is set, every `workers[].dir` and its ancestors are pinned the same way in the manager**, and must be reached through real directories only (apply creates a missing worker dir; the broker refuses a worker path through a symlink or over an entry), so the manager cannot swap a worker dir for a link to a protected one. **It protects the entry directories only:** whatever the host code loads from **outside** an entry (a `require` path that reaches out, a `Gemfile` or `.bundle/config` beside it, the tree's `.git` hooks and config when you run `git` there, `mise`/`asdf` files) is still agent-writable; move it inside an entry or do not trust it. Must be clean relative paths (no `..`, `./`, trailing slash or `.`, and none of `$ ~ : ,`, which scion or the runtime would interpret), with no duplicates and no entry inside another, compared case-insensitively. **No `workers[].dir` may equal, contain or lie inside an entry** (enforced): a worker mounts its own dir read-write and never sees these mounts. `lever apply` refuses to start the manager unless every entry is a real directory reached through **no symbolic link** and holds no symlink that points outside it. On a case-insensitive host, a case alias of a protected path inside the container (`TOOLS`) reaches the same read-only mount (verified on OrbStack, not yet on Lima). Must be ASCII (APFS treats the composed and decomposed spellings of an accented name as one directory); while the list is set, worker dirs must be ASCII too. A protected directory must hold **no file with more than one hard link** and no symbolic link that does not resolve inside it (apply refuses both). The hard-link rule also catches a pnpm `node_modules` (hard-linked from its store) and the objects of a `git clone --local`: keep such things out of protected directories. **Edit protected directories in place; never replace the directory itself.** The read-only mount covers the directory that existed when the manager was created: replacing it on the host (`rm -rf` and recreate, a rename-based deploy, a `git checkout` that removes and re-adds it) leaves the mount on the old directory, and the manager can write the new one until the next fresh create. `lever doctor` and `lever apply` probe this live for the entries (a write test, as root, inside the running manager). **The pins are checked by inspect only:** no non-mutating live test of a pin is known to be reliable (the container's mount table still lists a mount whose directory the host replaced), so a worker dir or an entry's ancestor recreated on the host can lose its pin, unreported, until the next fresh create. **Never recreate worker dirs or the parents of protected directories on the host.** **Create-time only:** scion keeps a record's mounts for life, so a new or edited list reaches only a freshly created manager — `lever up --fresh` for an existing one, which **discards the manager's conversation** (back it up first). `lever apply` warns, and `lever doctor`'s *manager read-only paths* row fails, when the running manager lacks a mount or can write an entry. **Removing an entry, or a worker while the list is set, also needs `--fresh`**, and so does removing the whole list: the record goes on mounting the old directory, read-only for an entry, and **the directory must stay on the host until the fresh create**. If it is gone, podman cannot recreate the container and the next resume fails (`statfs …: no such file or directory`); `lever apply` and `lever up` refuse before any resume (a forced resume of an `error` phase too) and name the directory; they read the mounts off the manager container or, when it has none, off the hub record: recreate it (empty is enough), or back up the conversation and run `lever up --fresh`. `lever doctor`'s *manager read-only paths* row warns about a mount the config dropped and fails on one whose directory is gone, even with `read_only` unset, reading the hub record too when the manager has no container. See [security model §5.1.1](/security-model/config-trust/). |
| `credential_file` | path | no | - | A file whose contents are set as the `CLAUDE_CODE_OAUTH_TOKEN` Hub secret and **projected into agent containers**. Relative paths resolve against the config file's directory; `~/` is expanded. Read at apply time with a **permission check (rejected unless the mode is `0600`-tight — no group or other bits) and size cap**. **Its contents reach every agent, point it only at a real, least-privilege, `0600` credential.** Rejected if it resolves inside the mounted `tree` (an agent could read it there, even under `read_only`). See [security-model.md](/security-model/). |
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
| `image_tar` | path | no | inherited with `image` | The archive shipping this worker's own `image`, as for `manager.image_tar`. Requires `image`; a worker with no `image` runs the manager image and so ships in the manager's archive. One image ref may come from only one archive. |
| `model` | string | no | `manager.model` | The LLM this worker runs on. Set it to give a worker a different capability/cost point; omit to inherit the manager's model (and, if that is unset too, scion's own default). Resolved host-side from config, so the manager cannot choose it when it asks for a worker to be started. Create-time only, as for `manager.model`. |
| `instructions_file` | path | no | - | This worker's standing instructions, as for `manager.instructions_file`. **Not inherited** from the manager — the manager's manual describes orchestration authority a worker must not hold — so a worker without it gets no lever instructions. The path is config-resolved on the host (the manager cannot choose it); the contents are read at each fresh dispatch, so an edit reaches the next newly created worker without a broker restart. The task a manager sends with a dispatch is subject to the same 15 KiB budget as `prompt_file`; the broker refuses an oversized one with 413 and the reason, whatever the worker's phase. |
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
| `api_key_file` | path | **yes for `api-key`** | - | A file holding the real Anthropic Console key. Read host-side by the broker `/llm` proxy and injected into the upstream request; **never enters a container.** Must be **`0600`** (rejected otherwise), mirroring `credential_file`, and must resolve **outside** the mounted `tree` (enforced). See [providing the Console key](#providing-the-console-key). |
| `llm_upstream` | string (URL) | no | `https://api.anthropic.com` | Overrides the `/llm` proxy target, e.g. to route through an LLM proxy that speaks the Anthropic Messages API. **Operator-set only, never client-controlled** (no SSRF: the broker always streams to this one fixed host); it still injects the real Console key host-side and strips the inbound capability token first. |
| `jail_port` | int | no | `8443` | mTLS port the in-jail agents reach the broker on (allowlisted in the egress rules). Defaults to 8443; set an explicit port only to run several instances' brokers on one host at once. |
| `admin_port` | int | no | `8444` | **Loopback-only** unauthenticated admin port (`/register`, `/revoke`, `/bump-epoch`, `/bootstrap`, `/epoch`); bind is rejected if non-loopback. Defaults to 8444. |
| `grant_ttl` | duration | no | `24h` | Capability token lifetime. A backstop only: the per-call epoch/revocation check is the real cut, so a session-scale TTL is safe (and must outlive the 12h renew cycle). |
| `ticket_ttl` | duration | no | `10m` | Lifetime of a one-time enrolment ticket (the manager-bootstrap and agent-enrol tickets minted at apply). Short by design; only needs to outlive container boot. |
| `manager_identity` | string | no | `manager` | The capability CN the manager enrols under (its certificate identity at the broker), distinct from its Scion agent slug (`name`). |
| `auto_reenrol` | enum | no | `all` | Which agents the broker auto-heals after a **natural** mTLS-leaf lapse (`all` \| `manager` \| `off`). When an agent's short-lived client leaf ages out while renewal couldn't run (host asleep, instance idle), the broker's handshake verification proves the presented cert is its own CA's, valid in every way but time — never a revoked or foreign identity — then re-stages a fresh one-use enrolment ticket and bounces the agent so boot re-enrols (conversation preserved via resume). Attempts are audited (`op=reenrol`) and bounded (10&nbsp;min cooldown, 3 per burst). `lever revoke` always wins: revoked identities are never healed. The same mode governs the broker's **hub-token watch**: every 5&nbsp;min it reads each running agent's scion hub token in its container and runs `scion reset-auth` for one that expired (sciontool's refresh timer stands still while the host sleeps), at most once per agent per 15&nbsp;min, audited as `op=hub-token`; a worker busy with a lifecycle verb is skipped until the next pass, and a record created before scion#1089 (grandfathered to the full role, or storing none) is refused, as is any reset when no pre-role guard is wired. The new token carries the agent's own stored role. |
| `tools` | list of `{name, command, backend, operations, allowed_values, external, gate, allow_non_loopback}` | no | `[]` | First-party / brokered tools registered for capability minting. `command` launches the supervised subprocess; `backend` is the loopback address it listens on (injected as `-backend`); `operations` are the `{name}` verbs — each may declare `params` (its argument names): when set, a capability mint whose constraint keys fall outside `params` ∪ `caveat_param` keys is rejected at mint time with an accurate error, instead of minting an over-narrowed token that fails closed only at call time (a typo'd key otherwise surfaces as a baffling `constraint not satisfied` far from the mistake); `caveat_param` (map of constraint key → request argument name, for non-identity mappings such as `table: schema.table`) is an optional declared guard: at registration the broker checks the tool-shipped caveat-param map equals it and rejects the tool (403) otherwise; its keys also count as valid constraint keys for the `params` check. Omit to accept whatever the tool ships; `allowed_values` restricts a constraint key to a permitted set (e.g. `table: [A, B]`), enforced at mint. With `external: true` the broker FRONTS an already-running host MCP server instead of spawning one: no `command`, `backend` is the server's own listen address (`host:port[/path]`, literal loopback IP unless `allow_non_loopback: true`), and the tool registers third-party — the broker enforces the rules and strips the capability before proxying. lever ships three first-party tools for `command`: `lever-tool-db` (the reference tool), `lever-tool-github` and `lever-tool-fizzy`. Their settings (credential file, repos or board, state dir) are flags in `command`, not `lever.yaml` keys: see the [github tool](/github-tool/) and [fizzy tool](/fizzy-tool/) guides. **Config load refuses host paths in `command` that resolve inside `tree`:** the program (when given as a path), the value of `-fizzy` and an interpreter's script (`ruby`, `python3`, `sh` and similar: the first argument, or the absolute paths in a `sh`/`bash` `-c` line), unless it lies under a `manager.read_only` entry in no worker `dir`; and always the values of `-app-key`, `-token-file` and `-state`. **No other flag or argument is checked** (`-tree`, `-dsn` and `-csv` are deliberately not): lever cannot tell what an unknown flag names, so keep your own tool's secrets and programs outside the tree yourself — a best-effort guard, not a sandbox. A path with a `..` component is refused. A tool whose program relies on `read_only` starts only once the broker has seen the running manager hold the read-only mounts (it retries every 30 s; the reason is in the tool's log). Every tool runs with the instance root as its working directory, and relative paths resolve there; a bare command name resolves on the supervisor's fixed `PATH`. See [security model §5.5](/security-model/config-trust/). |
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
| `allowed_signers` | path | no | - | An `ssh-keygen` `allowed_signers` file listing keys trusted to sign directives, under the fixed principal `operator@<instance-name>`. A **confined path under the instance dir**, host-only — keep it **out of** the mounted `tree:`, so a compromised jailed agent can neither read it nor alter the trust anchor (enforced: rejected if it resolves inside `tree`). Live-editable: the broker shells out to `ssh-keygen -Y verify` per call, so a key added/removed here takes effect with no restart. |
| `signing_key` | path | no | - | Default private key `lever directive send` signs with (`--key` overrides it per call). A **host path, not confined** to the instance dir — keep it outside the tree entirely (enforced: rejected if it resolves inside `tree`), and gitignore it. |
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
(chat, transcript, xterm.js attach) through an authenticating front (`tailscale serve` by default, or
another front that sets a verified identity header, such as exe.dev), injecting the operator's hub web session
host-side so the client never holds a credential. The whole block is optional and disabled by default — see the [remote
access guide](/remote-access/) for the accepted security posture, setup steps, and repair.

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `enabled` | bool | no | `false` | Turns the proxy on. Loads on both backends (`orbstack` and `lima`); the Lima path is not yet live-validated (see the [guide](/remote-access/#lima)). A release build of lever (or one built with `make install`) embeds the guest's login forwarder, so it needs **no Go toolchain**; a lever built with plain `go build`/`go install` cross-compiles the forwarder at apply time and needs Go — with `scion.binary:` (the only mode that does not need Go for scion) that is checked at config load. Also requires **node >= 20 + npm on the host** with [`scion.version`](#scion) or `scion.source`: neither carries built web assets, so lever builds the UI host-side and stages it into the guest. `binary:` is exempt, and serves only the UI its binary embeds (upstream's `make all`). `lever doctor` checks it. |
| `port` | int | no | `8445` | Port the proxy binds (on `bind`). Must not collide with the broker's `jail_port`/`admin_port`, with `login_port`, or with `8446` (the host port the jail's login forwarder is mirrored onto on OrbStack) — all rejected at config load. Also rejected if listed in `manager.allow_ports`: the proxy trusts the identity header because only the front can reach its listener, and an allow_ports hole would let the jail reach it directly. Give each remote-enabled instance on the host its own port: the proxies reach their own jails, so only the host listeners are shared ground. |
| `login_port` | int | no | `8447` | **Host** loopback port the local OIDC provider binds — the login path that gets the browser a hub session with no external identity provider and no dev-auth. The port the HUB dials is a different, fixed one inside the guest (`8446`, no config key): Scion validates `issuer_url` at hub startup and refuses to start unless it is loopback, so a forwarder listens there and carries the bytes to this port. The two must differ because the container runtime mirrors a guest listener onto the host at the same number — one number for both halves left the provider unable to bind. Rejected at config load if it collides with `port`, the broker's listeners, or `8446` (the guest forwarder's host mirror). A second remote-enabled instance needs its own value. See the [guide](/remote-access/#how-the-browser-is-logged-in). |
| `base_url` | string (URL) | **yes when `enabled`** | - | The front's public origin, as an absolute `https://` URL with its port when not 443: the tailnet serve hostname (`https://myhost.tailxxxx.ts.net`), or another front's (`https://myvm.exe.xyz:8445`). **Rejected at config load if empty while `enabled: true`**: the proxy matches every request's `Origin` against the host this resolves to, and an empty `base_url` would fail closed on every request — a proxy that could never serve anything, so lever refuses to load that config at all. It configures **only the proxy**; lever deliberately never passes it to the hub as `--base-url` (see the [guide](/remote-access/#the-tailnet-url-never-reaches-the-hub)). |
| `allowed_users` | list of string | no | `[]` | Optional identity pinning: when non-empty, only these logins (matched exactly against the `identity_header` value the front sets) may reach the proxy; a request carrying that header twice, or a comma-joined value, is refused. Empty ⇒ anyone who gets through the front. Each entry must be one login: blank entries, commas, whitespace and control characters are rejected at config load, and so are entries that differ only in case (scion lowercases emails, so they would be one hub user). It also decides the identity the login path asserts to the hub, so the hub's user row names the operator who actually connected — with it unset, a placeholder identity is used instead. An entry with `@` is the hub email as-is; one without (a front's user id) becomes `<id>@id.lever.local` and may use only letters, digits and `. _ + -`. An entry may also be an object, `{login, tier, agents, see, files}`: `tier: contact` makes the login a [contact](/remote-access/#contacts-chat-only-logins) that may message only the agents in `agents:` (required). `see:` (optional, contact only) lists agents the contact may only see on the [chat page](/remote-access/#the-chat-page) — name, label and state, no history, no input. Config load refuses a `see:` name that is not the manager or a configured worker; a name in both `agents:` and `see:`; `see:` on an operator entry. `files: false` (bool, contact or operator, default `true`; read only while `files.enabled`) gives the login no [file exchange](/remote-access/#files-in-the-chat): its file routes answer 404 as with files off, its page shows no paperclip or Files panel, and no agent can share with it. For a contact this is enforced (the contact fence allows nothing else); for an operator it only hides the chat page's file feature and is not access control, since an operator has the whole hub (workspace file API, WebDAV, terminal). |
| `identity_header` | string | no | `Tailscale-User-Login` | The request header the authenticating front puts the verified login in, e.g. `X-ExeDev-Email` behind exe.dev. Read for `allowed_users` and for the identity asserted to the hub; stripped before forwarding. **The front must overwrite it, never append to a client-sent value, and only the front may reach the listener** — confirm both for your provider. Rejected at config load if it is not a header name, or if it is a header the browser or any hop sets, or one scion reads as an identity: `Authorization`, `Cookie`, `Host`, `Origin`, `Forwarded`, `X-Forwarded-For`/`-Host`/`-Proto`/`-Port`, `X-Real-Ip`, `Sec-*`, `X-Scion-*`, `X-Forwarded-User-*`, `X-Api-Key`, hop-by-hop headers, Tailscale's user-chosen `Tailscale-User-Name`/`Tailscale-User-Profile-Pic`, any name containing `_`, and a few more. See the [guide](/remote-access/#non-tailscale-fronts). |
| `bind` | string (IP) | no | `127.0.0.1` | Address the proxy listens on. Loopback by default. A front that dials the host's external interface needs a non-loopback address; **anything that can reach it can then set the identity header to any login**, so the host firewall must admit only the front. Accepted only as an IP literal the jail's egress rules drop in every posture (RFC 1918, CGNAT `100.64/10`, IPv4 link-local, IPv6 ULA) — a public address would be reachable from the jail under open egress. IPv6 link-local is refused (it needs an interface zone to listen on). With the default `identity_header`, tailnet addresses (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`) are refused: any tailnet peer could connect directly and forge `Tailscale-User-Login` — use `tailscale serve`. `0.0.0.0`/`::` needs `allow_wildcard_bind`, and is refused outright with the default `identity_header` (it listens on the tailnet address too). A non-loopback bind prints a warning on every `apply`/`up`/`remote serve` and shows as a `lever doctor` warning row. The login provider always stays on loopback. |
| `allow_wildcard_bind` | bool | no | `false` | Acknowledges `bind: 0.0.0.0` or `"::"`: every host address, public ones included. Rejected when `bind` is not a wildcard. |
| `trust_forwarded_host` | bool | no | `false` | Make the proxy's `Host` check (its DNS-rebinding defence) read `X-Forwarded-Host`, when a request carries exactly one and its `Host` is an IP address, instead of `Host` — for a front that rewrites `Host` to an IP address the default refuses. A rebind sends a name in `Host`, so it is still refused. **Off by default**: anything that reaches the listener directly can set that header. Warned about like a non-loopback `bind`. Not needed when the front sends `Host` as `base_url`'s host, or — with a specific `bind` address — as the address it dials (`127.0.0.1:<port>`, or `<bind>:<port>`). With `bind: 0.0.0.0`/`::` no dialled address is admitted, so a front that rewrites `Host` to one needs this setting (or must pass `base_url`'s host through). |
| `landing` | string | no | `console` | What the remote origin opens on. `console`: the hub's own web UI. `chat`: lever's chat page — the proxy serves it under `/lever/` and redirects `/` to it. An operator sees the manager and every worker and may message all of them, and the hub's web UI stays reachable from the page (its **Console** link) and by its own paths. A contact sees its `agents:` (to message) and `see:` (view only), gets no Console or Terminal link, and every page navigation of a contact redirects to the chat page (no contact landing page, no hub web UI shell); the contact fence's hub rules are unchanged. A message to an asleep worker wakes it (see the [guide](/remote-access/#waking-a-worker)). `chat` needs at least one operator login in `allowed_users` (rejected at config load without one): with no verified login, no message from the page would verify. See the [guide](/remote-access/#the-chat-page). |
| `labels_file` | string (tree-relative path) | no | - | A JSON file the manager writes, `{"<agent name>": "<label>"}`, whose labels the chat page shows after each agent's name. Whoever can write this file sets the text contacts see beside each true agent name, the manager's row included: by default that is the manager (and anything else that can write that part of the tree). Rejected at config load when absolute, unclean, containing `..`, or not under `tree`; when equal to or inside any `workers[].dir` (that worker would write it); or when inside `.lever` or the state directory (host-written files). Read on the host through the tree, cached at most 10 s: a regular file of at most 16 KiB with no symbolic link on its path; unknown names are ignored; each label loses control and format characters, has its spaces collapsed, and is cut to 60 characters (dropped when empty). A missing file means no labels; a bad one means no labels and a `chat labels` warning row in `lever doctor`. See the [guide](/remote-access/#labels). |
| `agent_messages.enabled` | bool | no | `false` | Lets an agent start a message to a contact that lists it, under a limit and with a host record, and makes the proxy show a contact only the agent messages that record holds (history, events, DM previews, unread counts). The operator's view is unchanged. Needs at least one `tier: contact` entry in `allowed_users` (rejected at config load without one), and scion `b3562fb1` (2026-08-28) or later, where the hub sets a message's sender itself: a `scion.version` pseudo-version older than that is rejected at config load; for a bare commit hash, a tag or `scion.binary` lever cannot tell, and the `agent messages` row of `lever doctor` warns (it fails on an older `scion.source` checkout). Off, nothing changes. See the [guide](/remote-access/#messages-agents-start). |
| `agent_messages.follow_up_after` | duration | no | `24h` | How long after a message an agent started (with no answer from the contact) the agent may send its one reminder. `0` or unset is the default; otherwise `1h` to `720h`. A message from the contact resets the count. |
| `agent_messages.max_chars` | int | no | `4000` | The longest message, in characters, an agent may send a contact. `0` or unset is the default; otherwise `1` to `16000` (the hub's own limit). |
| `push.enabled` | bool | no | `false` | Web Push notifications for the chat page: a login's devices show "New message from <agent>" when an agent writes while the page is closed. Needs `landing: chat` (rejected at config load without it). Off, the proxy has no push routes, no service worker and no hub streams, and its config stamp is unchanged. See the [guide](/remote-access/#notifications). |
| `push.subject` | string | when `push.enabled` | - | The VAPID contact the push services see for this sender (RFC 8292): `mailto:<local>@<domain>` or an absolute `https://` URL, printable ASCII, at most 200 bytes. A change restarts the proxy at the next `lever apply`. |
| `push.test_hosts` | list of `127.0.0.1:<port>` | no | - | TEST ONLY: the loopback addresses of a fake push service for lever's end-to-end test. The proxy admits them only when `LEVER_PUSH_TEST_HOSTS` in its environment names the same addresses; either one alone stops `lever remote serve`. Needs `push.enabled`. `lever doctor` fails while it is set. Never set it for a real instance. |
| `files.enabled` | bool | no | `false` | Files in the chat: a login uploads one file at a time to an agent it may message, and downloads the files that agent shares with it (broker `share_file`). Needs `landing: chat` (rejected at config load without it). While it is on, no worker `dir` and no `manager.read_only` entry may overlap `.lever-files` (compared case-insensitively). Off, the proxy has no file routes, `share_file` refuses with `off` and `contact_files` returns an empty answer with a note, and the skills and config stamps are unchanged. See the [guide](/remote-access/#files-in-the-chat). |
| `files.max_bytes` | int | no | `26214400` (25 MiB) | The largest file, in bytes, for an upload and a share. `0` or unset is the default; otherwise `1` to `104857600` (100 MiB). |
| `files.extensions` | list of strings | no | `pdf, xlsx, xlsm, xls, csv, docx, doc, png, jpg, jpeg, txt, zip` | The accepted file types: lowercase letters and digits, 1 to 10, no dot, each once. `html`, `htm`, `xhtml`, `shtml`, `svg`, `js`, `mjs` and `xml` are refused (active content). Office types that can carry macros (`xlsm`, `xls` and `doc` in the defaults) are allowed on purpose; the `files` row of `lever doctor` names them (open them with macros off). |
| `files.uploads` | bool | no | `true` | `false`: no login can upload (the upload route answers `403 uploads-off` before it reads the body; the page shows no paperclip). Uploads already made stay listed and downloadable by their owner, and `contact_files` still lists them. A change restarts the proxy at the next `lever apply` and changes the skills (`lever init` first). |
| `files.shares` | bool | no | `true` | `false`: `share_file` answers `shares-off`, and a share already made stays listed but its download answers `403 shares-off` (the page shows it without a link). A change restarts the broker and the proxy at the next `lever apply` and changes the skills (`lever init` first). |

```yaml
remote:
  enabled: true
  base_url: "https://myhost.tailxxxx.ts.net"   # required whenever enabled — see above
  # port: 8445                                 # optional; default shown
  # login_port: 8447                           # optional; the local OIDC provider's HOST port
  # allowed_users: ["you@github"]              # optional identity pinning
  # identity_header: Tailscale-User-Login      # optional; another front's verified-login header
  # bind: 127.0.0.1                            # optional; a private address only for a front that dials it
  # trust_forwarded_host: false                # optional; see above before turning on
  # landing: console                          # optional; chat = open on lever's chat page (the login's agents)
  # labels_file: workers/labels.json          # optional; manager-written labels for the chat page
  # push:                                     # optional; chat page notifications (needs landing: chat)
  #   enabled: true
  #   subject: mailto:you@example.com
  # files:                                    # optional; files in the chat (needs landing: chat)
  #   enabled: true
```

## Conventions & derived values

- **Machine name:** `lever-<name>`. `up`/`apply`/`stop`/`destroy`/`doctor` all agree on this, derived
  from the config (override on `stop`/`destroy`/`doctor`/`worker purge` with `--machine`). `lever down` is a
  deprecated alias of `destroy`.
- **Worker model inheritance:** a worker with no `model:` runs on `manager.model`; with neither
  set, lever passes no `--model` and the pinned scion resolves the model. The model is fixed when
  an agent is created (`scion resume` takes no `--model`), so editing it only affects agents
  created afterwards. The broker resolves the inheritance once, when it starts, and the hash that
  decides whether `lever apply` restarts the broker covers `broker:`/`workers:`/`scion:` but
  deliberately not `manager:` — so an edit to `manager.model` alone reaches a newly created worker
  only after the broker restarts for another reason. Setting `workers[].model` explicitly is the
  direct route. This is the same trade-off `image:` inheritance already makes.
- **Worker image inheritance:** a worker with no `image:` runs on `manager.image` (and ships in
  `manager.image_tar`, when set). The bring-up plan loads every distinct image once (deduped),
  from its archive when the config names one, else from the host docker store. At dispatch the capability broker reads the config
  directly and supplies each worker's resolved image, so `agent start NAME` takes no image argument.
  The manager names a configured worker; the broker resolves paths, image and credentials host-side.
- **In-place mounts:** the `tree` subdir (and each `workers[].dir` within it) is bind-mounted into the
  jail so agents edit the real host files. There is no copy/sync step.
- **Path resolution base:** relative `tree`/`prompt_file`/`instructions_file`/`scion.source`/`credential_file` resolve
  against the **config file's directory**, never the shell's current directory.

## See also

- [Getting started](/getting-started/), build a working instance from scratch.
- [Security model](/security-model/), trust boundaries, threat model, and the credential flow
  these keys drive.
