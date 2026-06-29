# multi-project

A lever example with three independent groves running in parallel under one manager.

## What it demonstrates

- Dispatching work to multiple isolated groves simultaneously
- Grove isolation: each grove has its own workspace and cannot see the others
- Parallel orchestration: manager collects completion events from all three before reporting

## Structure

```
multi-project/
├── lever.yaml          # Application config
├── manager.md          # Manager system prompt
└── groves/
    ├── svc-a/          # Independent grove
    ├── svc-b/          # Independent grove
    └── svc-c/          # Independent grove
```

## How to run

Start the application from this directory with:

```sh
lever apply
```

The manager dispatches independent tasks to `svc-a`, `svc-b`, and `svc-c` in
parallel, waits for all three completion events, and summarises the results.
