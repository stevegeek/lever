---
title: Tools
nav_order: 5.85
permalink: /tools/
---
# Tools

lever ships first-party broker tools: small MCP servers that run on the host, outside the jail,
and give agents one narrow piece of outside reach each. An agent calls the tool; any credential
and the program itself stay on the host.

## How a first-party tool works

- **The broker supervises it.** You register the tool in `broker.tools` with a `command`. The
  broker starts it as a subprocess on host loopback and fronts it over mTLS at `/mcp/<name>/`.
  The jail cannot reach the tool's port directly.
- **The tool's settings are flags.** Credential files, repositories, the board, models and state
  directories go in the tool's `command`, not in `lever.yaml` keys. Config load refuses the
  program, credential and state paths of these tools when they resolve inside the tree, which the
  agent can read. See the [config reference](/reference/config/).
- **Every call needs a capability.** A registered tool is inert until you grant it. With an
  `obtain` grant an agent mints a token for one `{tool, op}`; with a `delegate` grant the manager
  mints one for another agent, such as a worker, and hands it over. The token is bound to the agent's identity. The broker checks
  it and forwards it, and the tool, written with the captool SDK, verifies it again before it acts.
  No grant means no access. The full flow is in [capabilities](/capabilities/).
- **Refusals are plain results.** A tool that refuses a call returns `{"ok": false, "error": "..."}`
  with the reason, so the agent can fix the cause and retry.

`lever-agent boot` registers every brokered tool with the agent's harness, so agents need no
hand-written `.mcp.json`. After you change `broker.tools` or grants on a running instance, run
`lever reload`.

## The tools

| Tool | Program | What it gives an agent |
|---|---|---|
| [github](/github-tool/) | `lever-tool-github` | Push a branch from a git bundle, with no GitHub credential in the jail. |
| [fizzy](/fizzy-tool/) | `lever-tool-fizzy` | Work on one Fizzy board, with no Fizzy token in the jail. |
| [whisper](/whisper-tool/) | `lever-tool-whisper` | Speech to text on the host, for the chat page's dictation and, if granted, an agent's audio file. |

`make install` builds all three into `~/.local/bin`. lever also ships `lever-tool-db`, a reference
tool for writing your own.

To register any MCP server as a broker tool, including third-party servers that are already
running, and to choose between a brokered and an ambient setup, see
[give agents MCP tools](/getting-started/mcp-tools/).
