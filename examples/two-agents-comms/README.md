# two-agents-comms

A lever example with two workers that exchange a message, coordinated by the manager.

## What it demonstrates

- Agent-to-agent messaging routed through the manager
- The manager acting as a message broker between isolated workers
- The notification loop: manager waits for producer output, then feeds it to consumer

## Structure

```
two-agents-comms/
├── lever.yaml          # Application config (host-side)
├── manager.md          # Manager system prompt (host-side)
└── workspace/          # the bind-mounted tree
    └── workers/
        ├── producer/   # Produces a value
        └── consumer/   # Consumes the value
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

The manager instructs `producer` to emit a value, relays it to `consumer`, and
reports when the consumer acknowledges receipt.
