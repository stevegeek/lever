---
title: "The fizzy tool"
nav_order: 5.86
permalink: /fizzy-tool/
---
The `fizzy` broker tool (`lever-tool-fizzy`) lets an agent work on one Fizzy board without holding
a Fizzy token. Part of [giving agents MCP tools](/getting-started/mcp-tools/).

## Why a first-party tool

No official Fizzy MCP server exists. A community MCP server would put third-party npm code on the
host with an account-wide Fizzy token. A generic MCP also cannot pin a board, because card
operations take a card number and not a board. This tool wraps the official fizzy CLI
(`github.com/basecamp/fizzy-cli`, version 4.x). The token stays on the host and the tool enforces
the board.

**The guarantee:** the tool touches only the board given by `-board`. Every card operation first
checks that the card is on that board. The tool passes the token to the CLI only as `FIZZY_TOKEN`.
It runs the CLI with an empty working directory and an empty HOME.

## Operations

| Operation | Parameters | Effect |
|---|---|---|
| `list_cards` | `column`, `search`, `page` | List cards on the board. `column` is a column id or `not-now`, `maybe`, `done`. `page` is digits. |
| `show_card` | `number` | Show one card on the board. |
| `list_comments` | `number` | List the comments of a card. |
| `comment` | `number`, `body` | Add a markdown comment. |
| `move_card` | `number`, `column` | Move a card to a column of the board. |
| `create_card` | `title`, `description` | Create a card on the board. |

A refusal comes back as `{"ok": false, "error": "..."}`, for example when a card is not on the
board. The tool never closes a card.

## Configure the tool

```yaml
broker:
  tools:
    - name: fizzy
      command: [/home/you/.local/bin/lever-tool-fizzy,
                -fizzy, /usr/local/bin/fizzy,
                -token-file, /home/you/lever-dev/secrets/fizzy-token,
                -account, "<account id>",
                -board, <board id>,
                -state, /home/you/.local/state/lever-dev-fizzy]
      backend: 127.0.0.1:3211
      operations:
        - {name: list_cards, params: [column, search, page]}
        - {name: show_card, params: [number]}
        - {name: list_comments, params: [number]}
        - {name: comment, params: [number, body]}
        - {name: move_card, params: [number, column]}
        - {name: create_card, params: [title, description]}
manager:
  obtain:
    - {tool: fizzy, op: list_cards}
    - {tool: fizzy, op: show_card}
    - {tool: fizzy, op: list_comments}
    - {tool: fizzy, op: comment}
    - {tool: fizzy, op: move_card}
    - {tool: fizzy, op: create_card}
```

The broker adds `-backend` and `-admin` itself. Every other setting is a flag in `command`:

| Flag | Required | Meaning |
|---|---|---|
| `-fizzy` | yes | Absolute path of the fizzy CLI. The tool refuses a CLI that is not major version 4. |
| `-token-file` | yes | Absolute path of the Fizzy personal access token. Regular file, mode `0600`, owned by the broker user. |
| `-account` | yes | The Fizzy account id (digits). |
| `-board` | yes | The only board id the tool may touch. |
| `-state` | yes | Absolute path of a private directory. It holds empty `home/` and `work/` directories and temporary body files. |
| `-prefix` | no | Text put before every comment and description. Default `[lever-dev agent] `. It must not be empty. |
| `-name` | no | Tool name. Default `fizzy`. It must equal the `name` in the config. |

Create a Fizzy personal access token only for this tool. Do not keep a `~/.fizzy.yaml` on the
host: the tool refuses to start when one exists above its state directory.

## Residual risk

Fizzy has no per-agent identity. Comments and cards appear as the owner of the token. The prefix
marks them as written by the agent, but a reader must trust the prefix and not the author name.
The token can reach every board of the account, so only the tool enforces the board limit.
