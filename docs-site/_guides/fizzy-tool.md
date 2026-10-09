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
checks that the card is on that board. The tool passes the token to the CLI only as `FIZZY_TOKEN`
and the account as `FIZZY_PROFILE`, and pins the API URL (`FIZZY_API_URL`) to
`https://app.fizzy.do`. It runs the CLI with an empty working directory and an empty
HOME, with the OS keyring (`FIZZY_NO_KEYRING`) and the update check (`FIZZY_NO_UPDATE_NOTIFIER`)
off.

## Operations

| Operation | Parameters | Effect |
|---|---|---|
| `list_cards` | `column`, `search`, `page` | List cards on the board. `column` is a column id or `not-now`, `maybe`, `done`. `page` is digits. The tool drops any card of another board from the result. |
| `show_card` | `number` | Show one card on the board. |
| `list_comments` | `number` | List the comments of a card. |
| `comment` | `number`, `body` | Add a markdown comment. |
| `move_card` | `number`, `column` | Move a card to a column of the board. `column` is a column id; a column of another board is refused with the list of valid columns. |
| `create_card` | `title`, `description` | Create a card on the board. An empty description is written as `(no description)` after the prefix. |

A refusal comes back as `{"ok": false, "error": "..."}`, for example when a card is not on the
board. The tool never closes a card.

The CLI turns a body or description from markdown into HTML and passes raw HTML through. The tool
refuses a body or description that contains `action-text-attachment` or `trix-attachment` (any
case): with that markup an agent could @mention an account user, which notifies them as the token
owner, or re-embed an attachment it saw. `comment` and `create_card` together are limited to 10
calls per minute per tool process; a call over the limit gets an error that starts with `rate`.

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
| `-token-file` | yes | Absolute path of the Fizzy personal access token. Regular file, mode `0600`, owned by the broker user. Config load refuses it inside the instance `tree` (the tool takes no `-tree` of its own), and refuses `-state` and `-fizzy` there too. |
| `-account` | yes | The Fizzy account id (digits). |
| `-board` | yes | The only board id the tool may touch (10 to 40 lowercase letters and digits). |
| `-state` | yes | Absolute path of a private directory. It holds empty `home/` and `work/` directories and temporary body files. |
| `-prefix` | no | Text put before every comment and description. Default `[lever-dev agent] `. It must not be empty. |
| `-name` | no | Tool name. Default `fizzy`. It must equal the `name` in the config. |

Create a Fizzy personal access token only for this tool. Do not keep a `~/.fizzy.yaml` on the
host: the tool refuses to start when a `.fizzy.yaml` or `.fizzy.yml` exists in its state directory
or in any directory above it, because the CLI would read it.

## Residual risk

Fizzy has no per-agent identity. Comments and cards appear as the owner of the token. The prefix
marks them as written by the agent, but a reader must trust the prefix and not the author name.
The token can reach every board of the account, so only the tool enforces the board limit.
