# multi-project

A lever example with three independent workers running in parallel under one manager.

## What it demonstrates

- Dispatching work to multiple isolated workers simultaneously
- Worker isolation: each worker has its own workspace and cannot see the others
- Parallel orchestration: manager collects completion events from all three before reporting

## Structure

```
multi-project/
├── lever.yaml          # Application config (host-side)
├── manager.md          # Manager system prompt (host-side)
└── workspace/          # the bind-mounted tree
    └── workers/
        ├── svc-a/      # Independent worker
        ├── svc-b/      # Independent worker
        └── svc-c/      # Independent worker
```

## How to run

From inside this directory (`lever` reads `./lever.yaml`; there is no walk-up):

```sh
lever up                # bring up the instance + attach the manager
# or, headless:
lever apply
lever apply --dry-run   # preview the bring-up plan only
```

This `lever.yaml` uses `broker.llm_auth: subscription` but sets no
`manager.credential_file`. Add `credential_file: ~/.scion/oauth-token` (0600, mint
with `claude setup-token`) as in `hello-worker`, or see
[getting-started-first-run.md](../../docs-site/_guides/getting-started-first-run.md).

The manager dispatches independent tasks to `svc-a`, `svc-b`, and `svc-c` in
parallel, waits for all three completion events, and summarises the results.
