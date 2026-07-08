# two-agents-comms

A lever example with two workers that exchange a message, coordinated by the manager.

## What it demonstrates

- Agent-to-agent messaging routed through the manager
- The manager acting as a message broker between isolated workers
- The notification loop: manager waits for producer output, then feeds it to consumer

## Structure

```
two-agents-comms/
├── lever.yaml          # Application config
├── manager.md          # Manager system prompt
└── workers/
    ├── producer/       # Produces a value
    └── consumer/       # Consumes the value
```

## How to run

Start the application from this directory with:

```sh
lever apply
```

The manager instructs `producer` to emit a value, relays it to `consumer`, and
reports when the consumer acknowledges receipt.
