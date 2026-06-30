---
title: Configuration
nav_order: 1
---
# Config reference — `lever.yaml`

A **lever application** is described by a single config file. It declares the manager agent and
the groves (project agents) it orchestrates, plus how the jail is built around them.

The canonical filename is **`lever.yaml`**, placed at the **instance root**. The root holds the
config and the boot prompt and is **NOT** mounted; only a `tree:` **subdirectory** is bind-mounted
into the jail. This keeps the config out of the agent-writable mount — a compromised agent can't
rewrite the config the host trusts on the next bring-up.

Config is resolved from the **current directory only** — there is deliberately **no walk-up
discovery**. Run `lever` from the instance root (where `lever.yaml` lives), or pass an explicit
config path. A `lever.yaml` planted in a parent directory can therefore never be picked up. See
[security-model.md](/security-model/) §5.

## Layout

```
my-instance/             <- instance root: run `lever` here; NOT mounted
  lever.yaml             <- the config
  prompt.md              <- boot prompt (host-only)
  workspace/             <- tree: the bind-mounted subdir (agents edit this)
    groves/...
    vendor/bin/lever-manager
```

## Minimal config

`tree:` is required and must be a confined subdirectory:

```yaml
name: myapp
backend: orbstack
tree: workspace
manager:
  image: scionlocal/lever-claude:latest
```

## Full example

```yaml
name: assistant                      # instance identity → jail machine "lever-assistant"
backend: orbstack                    # containment backend
tree: workspace                      # bind-mounted SUBDIR (the root is not mounted)
scion:
  version: 666333f9                  # pin a scion commit; fetched + cross-compiled into the jail
manager:
  image: scionlocal/lever-claude:latest
  prompt_file: prompt.md             # boot task; resolved at the ROOT (host-only, outside the mount)
  credential_file: ~/.scion/oauth-token            # credential projected to agents (keep 0600)
  allow_ports: [3101, 3102, 3305]                  # host tool ports the jail may reach
groves:
  - name: scratch
    dir: groves/scratch              # relative to tree (i.e. workspace/groves/scratch)
    # image: <ref>                   # optional; defaults to manager.image
security:                            # optional image policy (both default off)
  allowed_image_registries: [scionlocal]   # only run images from these registries/namespaces
  require_image_digest: false              # true → every image must be @sha256:-pinned
```

## Keys

### Top level

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `name` | string | **yes** | — | Instance identity. The jail machine is named `lever-<name>`; the manager agent's slug is `<name>`. Must match `^[a-z0-9][a-z0-9-]{0,62}$` (it becomes a machine name and a shell token). |
| `backend` | string | **yes** | — | Containment backend. `orbstack` is the validated backend today; `linux-docker` is reserved (not yet implemented). |
| `tree` | path | **yes** | — | A **confined relative subdirectory** of the instance root, bind-mounted **in place** into the jail (agents edit these real files live). Must not be `.` (the root itself is never mounted), absolute, or contain `..`. |
| `scion` | object | no | — | Where the Scion engine comes from (see below). |
| `manager` | object | **yes** | — | The manager agent (see below). |
| `groves` | list | no | `[]` | Project agents the manager orchestrates (see below). |

### `scion`

Provide **at most one** of `version` or `source` (they are mutually exclusive). Omit both to rely on a scion already present in the jail.

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `version` | string | no | — | A scion module version/commit (a commit hash like `666333f9`, or a `vX.Y.Z` tag) fetched via the Go module system and cross-compiled into the jail at provision time. **No vendored source tree** — this is the recommended way to pin Scion. Requires a Go toolchain on the host. |
| `source` | path | no | — | Path to a local scion source checkout, cross-compiled into the jail at provision time (for local Scion development). Relative paths resolve against the config file's directory. |

### `manager`

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `image` | string | **in practice** | — | Container image for the manager agent. Also the default image for groves that don't set their own (see `groves[].image`). `lever apply` loads this image into the jail's container runtime. Validated for safe ref characters. Without it, agents can't start. |
| `prompt_file` | path | no | — | A file whose contents become the manager's boot task. Resolved at the instance **root** (host-only, **outside** the mount, so an agent can't rewrite its own next boot prompt). Must be a confined relative path (no `..`, not absolute). Omit to start with scion's default task. |
| `credential_file` | path | no | — | A file whose contents are set as the `CLAUDE_CODE_OAUTH_TOKEN` Hub secret and **projected into agent containers**. Relative paths resolve against the config file's directory; `~/` is expanded. Read at apply time with a **permission check (rejected if world-readable) and size cap**. **Its contents reach every agent — point it only at a real, least-privilege, `0600` credential.** See [security-model.md](/security-model/). |
| `allow_ports` | list of int | no | `[]` | Host tool ports the jail may reach over the host alias (`host.orb.internal`). Everything else outbound to the host and the LAN is dropped by the egress allowlist. Used for host-side MCP servers, etc. |
| `llm_auth` | enum | no | inherits `broker.llm_auth` (or `subscription`) | `subscription` (the OAuth token is projected to this agent) or `api-key` (this agent holds only a `capability(llm)` token; the broker injects the real key). **The whole instance must be uniform** — mixing `api-key` and `subscription` across manager/groves is rejected at config load (see [security-model.md](/security-model/) §6.1). |
| `obtain` | list of `{tool, op}` | no | `[]` | Capabilities this agent may self-obtain from the broker. `api-key` agents are auto-granted `obtain: [{tool: llm, op: generate}]`. |
| `delegate` | list of `{tool, op, to: [...]}` | no | `[]` | Capabilities this agent may mint *bound to another agent* (`to`) to hand off. A delegated token is strictly narrower than what the delegator holds. |

### `groves[]`

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `name` | string | **yes** | — | Grove identity (its agent slug / hub project name). |
| `dir` | path | **yes** | — | Grove directory, **relative to `tree`** and inside it. Mounted in place, so files the grove writes appear on the host. Must not be absolute or escape the tree (`..`). |
| `image` | string | no | `manager.image` | Container image for this grove. Set it to give a grove a different toolchain; omit to inherit the manager image (the common single-image case). `lever apply` loads **each distinct** image into the jail. |
| `llm_auth` | enum | no | inherits `broker.llm_auth` | Same as `manager.llm_auth`, per grove — but the instance-uniform rule still applies. |
| `obtain` / `delegate` | list | no | `[]` | Same shape and meaning as `manager.obtain` / `manager.delegate`. |

### `security`

Opt-in image policy applied to `manager.image` and every grove image. Both default off, so existing
configs are unaffected until you turn them on.

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `allowed_image_registries` | list of string | no | `[]` (off) | An image is allowed only if it equals, or is prefixed by `<entry>/`, one of these entries — a registry host and/or namespace prefix (e.g. `scionlocal`, `ghcr.io/myorg`). Matched on whole path components (`scionlocal` allows `scionlocal/x` but not `scionlocalevil/x`). Empty ⇒ any registry. Stops a config from running an image from an untrusted source (the image is the code that runs as the manager/grove, with the projected credential). |
| `require_image_digest` | bool | no | `false` | When `true`, every image must be pinned by **content digest** (`…@sha256:<hex>`) rather than a mutable tag like `:latest`. Guarantees you run exactly the bytes you vetted (a tag can be re-pointed to different content later). |

> **Note:** these are enforced at config-load (host side), so they bound what `lever apply` loads and
> what the manager/groves declare. An explicit `--image` passed to `lever-manager agent start` is a
> runtime override and isn't policy-checked — but it can only run an image already loaded into the
> jail (which came from the validated config).

### `broker`

The host-side capability broker (outside the jail) that holds the real credential and mints
CN-bound, short-lived capability tokens. See [security-model.md](/security-model/) §6.

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `llm_auth` | enum | no | `subscription` | Instance-wide default LLM-auth mode (`subscription` \| `api-key`), inherited by manager/groves that don't set their own. **The effective set must be uniform** — a mixed instance is a hard config error. `api-key` also flips the jail to **closed-internet egress** (§2.2). |
| `api_key_file` | path | **yes for `api-key`** | — | A file holding the real Anthropic Console key. Read host-side by the broker `/llm` proxy and injected into the upstream request; **never enters a container.** Must be **`0600`** (rejected otherwise), mirroring `credential_file`. |
| `jail_port` | int | no | (assigned) | mTLS port the in-jail agents reach the broker on (allowlisted in the egress rules). |
| `admin_port` | int | no | (assigned) | **Loopback-only** unauthenticated admin port (`/register`, `/revoke`, `/bump-epoch`, `/bootstrap`, `/epoch`). Bind is rejected if non-loopback. |
| `grant_ttl` | duration | no | `24h` | Capability token lifetime. A backstop only — the per-call epoch/revocation check is the real cut, so a session-scale TTL is safe (and must outlive the 12h renew cycle). |
| `tools` | list of `{name, backend, operations, allowed_values}` | no | `[]` | First-party / brokered tools registered for capability minting. `allowed_values` restricts a constraint key to a permitted set (e.g. `table: [A, B]`), enforced at mint. |

## Conventions & derived values

- **Canonical filename:** `lever.yaml` at the instance root. Resolved from the current directory
  only — **no walk-up** (run `lever` from the root, or pass an explicit path).
- **Root is not mounted:** the instance root holds the config + boot prompt and stays host-only;
  only the `tree:` subdir is bind-mounted.
- **Machine name:** `lever-<name>`. `up`/`apply`/`down`/`doctor` all agree on this, derived from the
  config (override on `down`/`doctor` with `--machine`).
- **Grove image inheritance:** a grove with no `image:` runs on `manager.image`. The bring-up plan
  loads every distinct image once (deduped). At apply time the host writes a **sanitized runtime
  manifest** (`.lever-manifest.yaml`, grove→image only) into the mount; the in-jail manager resolves
  each grove's image from it — no `--image` needed (an explicit `--image` still overrides). The
  manifest carries no host paths or credentials, so tampering it only re-selects an already-loaded
  image.
- **In-place mounts:** the `tree` subdir (and each `groves[].dir` within it) is bind-mounted into the
  jail so agents edit the real host files. There is no copy/sync step.
- **Path resolution base:** relative `tree`/`prompt_file`/`scion.source`/`credential_file` resolve
  against the **config file's directory**, never the shell's current directory.

## See also

- [getting-started.md](/getting-started/) — build a working instance from scratch.
- [security-model.md](/security-model/) — trust boundaries, threat model, and the credential
  flow these keys drive.
