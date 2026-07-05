# assistant-demo — a tiny AI assistant on lever

A minimal but complete lever instance: a personal assistant that runs a morning
**standup** (today's weather + your pending todos). It's built to show, in one
runnable example, the two ways an agent gets a tool and how lever gates them:

| tool | kind | who runs it | how it's gated |
|---|---|---|---|
| `todo` | **first-party** | the **broker** spawns it (`lever-tool-todo`) | capability-aware: the tool itself verifies the `todo/list` token (via captool) |
| `weather` | **external** | **you** run it (`weather-stub`) | the broker fronts + proxies it, strips the token before forwarding; coarse-gated |

…plus **grove dispatch** (the manager delegates the todo lookup to a `todo`
grove) and **per-agent grants** (the manager may only obtain `weather`, the grove
may only obtain `todo` — neither can reach the other's tool).

Everything is deterministic and offline: `weather-stub` returns canned data (no
API key), and the todos are a CSV. Swap either for the real thing later.

## Layout

```
assistant-demo/
├── lever.yaml                     # manager + todo grove + the two brokered tools
├── manager.md                     # the standup boot prompt (host-side, not mounted)
├── tools/
│   ├── lever-tool-todo/           # FIRST-PARTY capability tool (reads the CSV)
│   └── weather-stub/              # EXTERNAL MCP stand-in (canned weather)
└── workspace/                     # the bind-mounted tree
    ├── CLAUDE.md
    ├── data/todos.csv             # your todos
    └── groves/todo/               # the todo grove's workspace
```

## Run it

From a checkout of lever (`lever` on your PATH — `make all` — and the agent image
built — `make lever-image`; see the [getting-started guide](../../docs-site/_guides/getting-started.md)):

1. **Build the two demo tools onto your PATH** (the broker spawns `lever-tool-todo`;
   you run `weather-stub` yourself):

   ```sh
   cd examples/assistant-demo
   go build -o ~/.local/bin/lever-tool-todo ./tools/lever-tool-todo
   go build -o ~/.local/bin/weather-stub    ./tools/weather-stub
   ```

2. **Provide a Claude OAuth token** at `~/.scion/oauth-token` (0600), as in the
   getting-started guide (this demo uses `subscription` mode).

3. **Scaffold the operator skills** so the agents know the capability flow:

   ```sh
   lever init
   ```

4. **Start the external weather server** (leave it running in its own terminal):

   ```sh
   weather-stub          # serves MCP on 127.0.0.1:3211/mcp
   ```

5. **Bring the instance up and greet it:**

   ```sh
   lever up              # first boot pulls runtimes + loads the image (~10-15 min)
   ```

   In the manager session, type **`morning`**. The manager will mint a `weather`
   capability and fetch the forecast, dispatch the `todo` grove to read your
   pending todos, and give you a short standup. You can also send it from the
   host without attaching:

   ```sh
   lever msg send "morning" --to assistant-demo
   ```

## What to look at

- **`lever doctor`** — the `weather` external-backend check confirms `weather-stub`
  is listening; the operator-skills check confirms `lever init` ran.
- **`.lever-state/broker.log`** — every capability decision. You'll see the
  manager's `weather` mint + call, and the todo grove's `todo/list` mint + call,
  each `allow`ed against its grant, and a `deny` if you ask an agent for the tool
  it wasn't granted.
- **Swap in real tools** — replace `weather-stub` with a real weather MCP (point
  `broker.tools[weather].backend` at it, keep `external: true`), and grow
  `todos.csv` or replace `lever-tool-todo` with your own first-party tool.
