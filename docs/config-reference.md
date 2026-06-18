# Config reference — `lever.yaml`

A **lever application** is described by a single config file. It declares the manager agent and
the groves (project agents) it orchestrates, plus how the jail is built around them.

The canonical filename is **`lever.yaml`**, placed at the **instance root** — a package.json-style
manifest. Commands that take a config (`lever up`, `lever apply`) discover it by walking up from the
current directory when you don't pass an explicit path, so you can run `lever` from anywhere inside
the instance. `lever down` / `lever doctor` use the same discovery to find the jail.

> **Security note:** discovery walks *up* the directory tree (like `git`/`npm`). Running `lever` from
> inside a directory you don't control means a `lever.yaml` planted in a parent directory could be
> loaded. Only run `lever` with no argument inside trees you trust, or pass an explicit config path.
> See [security-model.md](./security-model.md).

## Minimal config

`tree:` defaults to the config file's directory, so the smallest useful config is:

```yaml
name: myapp
backend: orbstack
manager:
  image: scionlocal/lever-claude:latest
```

## Full example

```yaml
name: assistant                      # instance identity → jail machine "lever-assistant"
backend: orbstack                    # containment backend
tree: .                              # host dir mounted in-place (default: this file's dir)
scion:
  source: vendor/scion-src           # scion source to cross-compile into the jail
manager:
  image: scionlocal/lever-claude:latest
  prompt_file: assistant/prompts/manager-boot.md   # boot task, relative to tree
  credential_file: ~/.scion/oauth-token            # credential projected to agents
  allow_ports: [3101, 3102, 3305]                  # host tool ports the jail may reach
groves:
  - name: scratch
    dir: groves/scratch              # relative to tree, mounted in place
    # image: <ref>                   # optional; defaults to manager.image
```

## Keys

### Top level

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `name` | string | **yes** | — | Instance identity. The jail machine is named `lever-<name>`; the manager agent's slug is `<name>`. Keep it to `[A-Za-z0-9_-]` (it becomes a machine name and a shell token). |
| `backend` | string | **yes** | — | Containment backend. `orbstack` is the validated backend today; `linux-docker` is reserved (not yet implemented). |
| `tree` | path | no | the config file's directory | Host directory mounted **in place** into the jail (the manager edits these real files live). Relative paths resolve against the config file's directory; `~/` is expanded; the result is made absolute. |
| `scion` | object | no | — | Scion engine source (see below). |
| `manager` | object | **yes** | — | The manager agent (see below). |
| `groves` | list | no | `[]` | Project agents the manager orchestrates (see below). |

### `scion`

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `source` | path | no | — | Path to the scion source tree, cross-compiled into the jail at provision time. Relative paths resolve against the config file's directory. Omit to rely on a scion already present in the jail. |

### `manager`

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `image` | string | **in practice** | — | Container image for the manager agent. Also the default image for groves that don't set their own (see `groves[].image`). `lever apply` loads this image into the jail's container runtime. Without it, agents can't start. |
| `prompt_file` | path | no | — | A file (relative to `tree`) whose contents become the manager's boot task. Omit to start with scion's default task. |
| `credential_file` | path | no | — | A file whose contents are set as the `CLAUDE_CODE_OAUTH_TOKEN` Hub secret and **projected into agent containers**. Relative paths resolve against the config file's directory; `~/` is expanded. **This file is read verbatim and its contents reach every agent — point it only at a real, least-privilege credential.** See [security-model.md](./security-model.md). |
| `allow_ports` | list of int | no | `[]` | Host tool ports the jail may reach over the host alias (`host.orb.internal`). Everything else outbound to the host and the LAN is dropped by the egress allowlist. Used for host-side MCP servers, etc. |

### `groves[]`

| Key | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `name` | string | **yes** | — | Grove identity (its agent slug / hub project name). |
| `dir` | path | **yes** | — | Grove directory, **relative to `tree`** and inside it. Mounted in place, so files the grove writes appear on the host. Must not be absolute or escape the tree (`..`). |
| `image` | string | no | `manager.image` | Container image for this grove. Set it to give a grove a different toolchain; omit to inherit the manager image (the common single-image case). `lever apply` loads **each distinct** image into the jail. |

## Conventions & derived values

- **Canonical filename:** `lever.yaml` at the instance root. Discovered by walking up from cwd.
- **Default tree:** an omitted `tree:` is the config file's directory — so the canonical config sits
  at the root of the tree it mounts.
- **Machine name:** `lever-<name>`. `up`/`apply`/`down`/`doctor` all agree on this, derived from the
  config (override on `down`/`doctor` with `--machine`).
- **Grove image inheritance:** a grove with no `image:` runs on `manager.image`. The bring-up plan
  loads every distinct image once (deduped). At dispatch time the manager resolves a grove's image
  from this config — no `--image` needed (an explicit `--image` still overrides).
- **In-place mounts:** `tree` and each `groves[].dir` are bind-mounted into the jail so agents edit
  the real host files. There is no copy/sync step.
- **Path resolution base:** all relative paths in the config resolve against the **config file's
  directory**, never the shell's current directory — so behavior doesn't depend on where you run
  `lever` from.

## See also

- [getting-started.md](./getting-started.md) — build a working instance from scratch.
- [security-model.md](./security-model.md) — trust boundaries, threat model, and the credential
  flow these keys drive.
