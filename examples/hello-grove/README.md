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

Start the application from this directory with:

```sh
lever apply lever.yaml
```

The manager will dispatch a task to `worker`, wait for the completion event, and
report the result.
