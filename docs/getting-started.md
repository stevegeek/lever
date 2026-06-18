# Getting started

This walks you from nothing to a running lever application — a **manager** agent that dispatches
work to a **grove** (project agent), all inside a jail that contains the whole stack. We use the
bundled [`examples/hello-grove`](../examples/hello-grove) as the worked example.

## What you'll end up with

```
your machine
└── OrbStack isolated machine  "lever-hello-grove"   (the jail)
    ├── rootless podman + scion hub (loopback)
    ├── manager container        ← edits your tree in place, dispatches groves
    └── worker container         ← runs the dispatched task
```

The jail is the security boundary. Your project tree is bind-mounted **in place**, so agents edit
the real files — there's no copy or sync. See [security-model.md](./security-model.md) for what
the jail does and doesn't protect.

## Prerequisites

- **macOS on Apple Silicon** with [OrbStack](https://orbstack.dev) running. (OrbStack is the
  validated backend today.)
- **Go 1.26+** to build the binaries.
- **A manager container image** on your host Docker, e.g. `scionlocal/lever-claude:latest`. `lever
  apply` loads this image into the jail; it can't be pulled from inside (egress is locked down).
  Confirm with `docker images | grep scionlocal/lever-claude`.
- **A Claude OAuth token** in a file (mint with `claude setup-token`), if your manager image runs
  Claude Code. Point `manager.credential_file` at it. Use a least-privilege token — it is projected
  into agent containers (see the security doc).

## 1. Install the binaries

```sh
cd /path/to/lever_to
make all
```

This builds two binaries:

- **`lever`** (host control plane) → `~/.local/bin/lever` (make sure that's on your `PATH`).
- **`lever-manager`** (in-jail orchestration) → `$LEVER_INSTANCE/vendor/bin/lever-manager`,
  cross-compiled for the jail's linux/arm64. It's staged into the instance tree so the manager can
  run it inside the container.

By default `LEVER_INSTANCE` is `$LEVER_INSTANCE`. For a different instance:

```sh
make lever-manager-linux LEVER_INSTANCE=/path/to/your/instance
```

Verify: `lever version`.

## 2. Look at the instance

`examples/hello-grove` is a complete, minimal instance:

```
hello-grove/
├── lever.yaml          # the manifest (manager + one grove)
├── manager.md          # the manager's boot prompt
└── groves/
    └── worker/         # the grove's workspace (bind-mounted in place)
```

```yaml
# examples/hello-grove/lever.yaml
name: hello-grove
backend: orbstack
tree: .
manager:
  image: scionlocal/lever-claude:latest
  prompt_file: manager.md
  allow_ports: []
groves:
  - name: worker
    dir: groves/worker
```

`tree: .` mounts the example directory itself; you could omit it and get the same result (the tree
defaults to the config file's directory). Every relative path (`manager.md`, `groves/worker`)
resolves against this directory. See [config-reference.md](./config-reference.md) for every key.

Stage the in-jail binary into this instance before bringing it up:

```sh
make lever-manager-linux LEVER_INSTANCE=/path/to/lever_to/examples/hello-grove
```

## 3. Preview the bring-up plan (no side effects)

From anywhere inside the instance — the config is discovered by walking up from cwd:

```sh
cd /path/to/lever_to/examples/hello-grove
lever apply --dry-run
```

You'll see the ordered plan:

```
  jail-up           /…/hello-grove
  load-image        scionlocal/lever-claude:latest
  init-machine
  config-registry
  scion-server
  register-manager  /…/hello-grove
  register-grove    /…/hello-grove/groves/worker
  start-manager     hello-grove
```

## 4. Bring it up

```sh
lever up
```

`lever up` creates the jail if needed (isolated machine → rootless podman → cross-compiled scion →
egress allowlist), loads the image, registers the manager + groves, starts the manager, and hands
you its terminal. **First boot takes ~10–15 minutes** (runtimes + a multi-GB image load); after that
it's fast.

`up` is idempotent: re-running it resumes a suspended manager and re-attaches. Detach with
`Ctrl-b d` (the manager is left suspended; the next `lever up` resumes the same conversation).
`--fresh` starts a new manager thread; `--no-attach` brings up without taking your terminal.

## 5. Dispatch a grove (inside the manager session)

You're now talking to the manager agent. It drives groves with the in-jail `lever-manager` binary.
A dispatch looks like:

```sh
vendor/bin/lever-manager agent start worker --task "Write a haiku to haiku.md" -g groves/worker
```

Notes:
- **`-g groves/worker` is a path**, resolved relative to the manager's working directory in the
  jail — not a bare slug. (A bare slug silently falls back to the manager's own project.)
- **No `--image` needed** — the grove's image is resolved from the config (it inherits the manager
  image here). An explicit `--image` overrides.

Watch progress and relay events:

```sh
vendor/bin/lever-manager watch &           # streams scion events to a file you can tail
vendor/bin/lever-manager agent list        # phases of running agents
vendor/bin/lever-manager msg list          # typed inbox (input-needed, completion, …)
vendor/bin/lever-manager msg send "answer" --to worker
```

When `worker` finishes, the file it wrote (`groves/worker/haiku.md`) is there on your host — it was
mounted in place.

## 6. Tear down

```sh
lever down
```

`down` removes the jail machine named `lever-<name>` (derived from the config — run it inside the
instance, or pass `--machine`). Your tree on disk is untouched; only the jail goes away. The next
`lever up` re-provisions it.

## Where to go next

- [config-reference.md](./config-reference.md) — every config key, defaults, conventions.
- [security-model.md](./security-model.md) — trust boundaries, the threat model, and the
  credential flow.
- `examples/two-agents-comms` and `examples/multi-project` — richer topologies.
