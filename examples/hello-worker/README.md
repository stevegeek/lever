# hello-worker

The minimal lever example: a manager agent plus one worker.

## What it demonstrates

- Loading a `lever.yaml` config
- Dispatching a simple task from the manager to a single worker
- Relaying progress and surfacing a completion event back to the manager

## Structure

```
hello-worker/
├── lever.yaml          # Application config (host-side, not mounted)
├── manager.md          # Manager system prompt (host-side)
└── workspace/          # the bind-mounted tree (`tree: workspace`)
    └── workers/
        └── worker/     # The single worker (agent workspace)
```

## How to run

From inside this directory (`lever` reads `./lever.yaml`; there is no walk-up):

```sh
lever up                # bring up the jail + attach the manager
# or, headless:
lever apply
lever apply --dry-run   # preview the bring-up plan only
```

The manager dispatches a task to `worker`, waits for the completion event, and reports the
result. This example is the worked example in
[getting started](../../docs-site/_guides/getting-started.md).
