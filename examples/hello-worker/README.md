# hello-worker

The minimal lever example: a manager agent plus one worker.

## What it demonstrates

- Loading a `lever.yaml` config
- Dispatching a simple task from the manager to a single worker
- Relaying progress and surfacing a completion event back to the manager

## Structure

```
hello-worker/
├── lever.yaml          # Application config
├── manager.md          # Manager system prompt
└── workers/
    └── worker/         # The single worker (agent workspace)
```

## How to run

From inside this directory (the `lever.yaml` is discovered automatically):

```sh
lever up                # bring up the jail + attach the manager
# or, headless:
lever apply
lever apply --dry-run   # preview the bring-up plan only
```

The manager will dispatch a task to `worker`, wait for the completion event, and
report the result. See [../../docs-site/_guides/getting-started.md](../../docs-site/_guides/getting-started.md)
for the full worked example.
