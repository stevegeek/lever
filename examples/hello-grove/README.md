# hello-grove

The minimal lever example: a manager agent plus one worker grove.

## What it demonstrates

- Loading a `lever.yaml` config
- Dispatching a simple task from the manager to a single grove
- Relaying progress and surfacing a completion event back to the manager

## Structure

```
hello-grove/
├── lever.yaml          # Application config
├── manager.md          # Manager system prompt
└── groves/
    └── worker/         # The single grove (agent workspace)
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
report the result. See [../../docs/getting-started.md](../../docs/getting-started.md)
for the full worked example.
