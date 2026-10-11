---
title: "The whisper tool"
nav_order: 5.87
permalink: /whisper-tool/
---
The `whisper` broker tool (`lever-tool-whisper`) runs speech to text on your host with
whisper.cpp. It serves the chat page's [dictation](/remote-access/#dictation-and-read-aloud), and,
only if you grant it, an MCP operation that lets an agent transcribe an audio file. Part of
[giving agents MCP tools](/getting-started/mcp-tools/).

## What it does

The tool runs whisper.cpp's `whisper-server` as its child, on host loopback, with a Whisper model
from lever's pinned table. It offers it two ways:

- **Dictation.** The remote proxy sends each clip that a login records on the chat page to the
  tool's Unix socket (`-dictate-socket`, the same path as `remote.voice.socket`). The socket is
  not reached through the broker. It is `0600` in a private `0700` directory, so only processes
  of your user can connect.
- **Agents.** The operation `transcribe` reads a WAV file from the tree root's
  `.lever-files/whisper/` (`/workspace/.lever-files/whisper/` in the manager's container). An agent
  can call it only with a capability from an `obtain` grant (or one delegated to it). Without a
  capability, no agent can use the tool.

**The guarantees:** audio and transcripts are never stored or logged. The jail cannot reach
whisper-server or the dictation socket. The tool starts whisper-server only with a model whose
size and sha256 match lever's table, and only from a program file that no one but you or root can
change.

## Requirements

- whisper.cpp's `whisper-server`, which you build or install yourself: with CUDA on a Linux host
  with an NVIDIA GPU, with Metal on Apple Silicon. Install it outside the tree, owned by you or
  root and writable by no one else. Lever never builds or installs it.
- Disk space and GPU memory for the model: about 1.6 GB for `large-v3-turbo`, 0.55 GB for
  `large-v3-turbo-q5_0`. With `-gpu=false` it runs on the CPU, much more slowly.
- Install the binary with `make install`. Releases ship it for darwin and linux.

## 1. Configure the tool

```yaml
broker:
  tools:
    - name: whisper
      command: [/home/you/.local/bin/lever-tool-whisper,
                -tree, /home/you/my-instance/workspace,
                -models, /home/you/my-instance/.lever-state/voice-models,
                -server, /usr/local/bin/whisper-server,
                -model, large-v3-turbo,
                -language, en,
                -vocabulary, "Lever, Scion, podman, mTLS",
                -whisper-port, "8448",
                -dictate-socket, /home/you/my-instance/.lever-state/whisper/dictate.sock]
      backend: 127.0.0.1:3212
      operations:
        - {name: transcribe, params: [file]}
remote:
  enabled: true                           # dictation is on the chat page
  base_url: https://lever.example.ts.net  # with the rest of your remote access setup
  landing: chat
  allowed_users:
    - operator@example.com                # landing: chat needs an operator login
  voice:
    enabled: true
    socket: /home/you/my-instance/.lever-state/whisper/dictate.sock
```

Leave out `remote:` to let only agents use the tool. Dictation needs [remote
access](/remote-access/) with the chat page: `remote.enabled`, `base_url` and `landing: chat`.

The broker adds `-backend` and `-admin` itself. Every other setting is a flag in `command`. Lever
recognises the tool by its program's base name, `lever-tool-whisper` (also behind an `env` prefix),
and reads these flags only in its command:

| Flag | Required | Meaning |
|---|---|---|
| `-tree` | yes | Absolute path of the instance tree. Config load requires it and refuses a value that is not the instance's `tree`. |
| `-models` | yes | Absolute path of the directory with the models. `lever voice fetch` downloads into it. Outside the tree. whisper-server also runs with this directory as its working directory. |
| `-server` | yes | Absolute path of `whisper-server`. Outside the tree; a `manager.read_only` entry does not excuse it. Its real file must be an executable owned by you or root that no one else can write to, in a directory no one else can write to. |
| `-model` | no | A name from lever's pinned table: `large-v3-turbo` (default) or `large-v3-turbo-q5_0`. |
| `-language` | no | A Whisper language code such as `en` or `de`. Unset: Whisper detects the language of each clip. |
| `-vocabulary` | no | A comma list of words Whisper should expect (names, jargon), passed to it as a prompt. Each 1 to 64 bytes, no control characters; at most 800 bytes in all. |
| `-gpu` | no | Default `true`. Write `-gpu=false` (with `=`) to pass `--no-gpu`. |
| `-whisper-port` | yes | The host loopback port of the whisper-server child. Config load refuses a port in `manager.allow_ports`, the broker's jail and admin ports, the remote proxy and login ports, port 8446, and any tool's `backend` port. Two instances on one host need distinct ports. |
| `-dictate-socket` | yes | Absolute path of the dictation socket, outside the tree, at most 103 bytes. Its directory must be yours and private (`0700`); the tool creates it when it is missing. `remote.voice.socket` must be the same path. |
| `-max-seconds` | no | The longest clip. Default 300, 1 to 600 (config load checks it). `remote.voice.max_seconds` must not be larger. |
| `-agent-max-seconds` | no | The longest clip of the agent operation `transcribe`. Default 120 (or `-max-seconds` if that is smaller), 1 to `-max-seconds` (config load checks it). It bounds how long one agent clip can hold the GPU ahead of dictation. |
| `-name` | no | Tool name. Default `whisper`. It must equal the `name` in the config. |

The broker starts tools with only `PATH` in their environment. If your GPU build needs more, such
as `LD_LIBRARY_PATH` or `CUDA_VISIBLE_DEVICES`, put an `env` prefix in front of the command:
`command: [env, LD_LIBRARY_PATH=/opt/cuda/lib64, /home/you/.local/bin/lever-tool-whisper, ...]`.
The child keeps only such GPU and locale variables, never the broker's tool secret.

## 2. Fetch the model

```sh
lever voice fetch                      # the tool's -model, into its -models directory
lever voice fetch large-v3-turbo-q5_0  # another model from the table
```

The download comes from Hugging Face at the commit lever pins. Lever keeps the file only if its size
and sha256 match its table. Neither the tool nor the proxy ever downloads a model. The tool checks
the model before every start of its child, so a running tool picks up a model fetched later within
a minute. With no whisper tool configured, `lever voice fetch` downloads into
`.lever-state/voice-models/`; point `-models` at that absolute path.

Then run `lever reload` and check `lever doctor`: the tool's own row, and the `voice tool` and
`voice model` rows. Those two run for any configured whisper tool, also with `remote.voice` off:
`voice tool` asks the tool's dictation socket for its health (not running, whisper-server not
ready, or ready with the model), and `voice model` checks the `-model` file in `-models` against
lever's pinned size and sha256. With `remote.voice` on they check the tool that serves
`remote.voice.socket`; with it off, the first whisper tool in `broker.tools`. The `voice` row is
about dictation only: off, or on with the socket, the limits and the tool's port.

## 3. Agents: the transcribe operation

To let the manager transcribe audio files, add the grant:

```yaml
manager:
  obtain:
    - {tool: whisper, op: transcribe}
```

The agent writes a WAV file into `/workspace/.lever-files/whisper/` and calls `transcribe` with
`file`, the file name only (for example `note-1.wav`, not a path). The name must match
`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,92}\.wav$`. The answer is `{"text": "..."}`. Whisper's known
non-speech markers such as `[BLANK_AUDIO]` are removed, and the text is cut to 16000 characters.

The file must be exactly the format the chat page sends: PCM, 16-bit, 16 kHz, mono, with the plain
44-byte WAV header and no other chunks, at least 0.1 s and at most `-agent-max-seconds` long (a
longer file is refused with `file ... is too long`; split it into shorter clips). The tool
never decodes another format, so whisper.cpp only parses a header that lever has checked byte for
byte. Convert other audio first, for example
`ffmpeg -i in.m4a -ar 16000 -ac 1 -c:a pcm_s16le -map_metadata -1 -fflags +bitexact -flags:a +bitexact out.wav`
(the last flags keep ffmpeg from adding a metadata chunk).

**Whose files.** The tool always reads from that one folder, the tree root's
`.lever-files/whisper/`, whoever the caller is. Only the manager can write there (a worker mounts
only its own directory), but any agent with a whisper `transcribe` capability, a worker holding a
delegated or obtained one included, reads the files the manager put there. Grant the capability,
and delegate it, only to agents that may read what the manager puts in that folder. The
[github tool](/github-tool/)'s bundle folder works the same way.

**Limits.** Per caller, 30 clips an hour and 60 minutes of audio a day, kept in memory (they start
again when the tool restarts). A caller over a limit is refused before its file is read. One clip
at a time reaches whisper-server. "Dictation goes first" means queue order: a waiting dictation clip
is taken before any waiting agent clip, but a clip already running is never interrupted, so
dictation can wait for up to one agent clip. An agent clip waits at most 2 minutes for its turn and
then gets as many seconds to finish as `-agent-max-seconds` (at least 30; with `-gpu=false`, four
times that, at least 2 minutes, since a CPU can be slower than real time). A clip that never
reached whisper-server (it was not ready, or the wait ran out) does not count.

**Refusals** come back as `{"ok": false, "error": "..."}` and never name a host path, except a
bad file name, which the tool's backstop refuses as `forbidden` before the call runs:

| Cause | Result |
|---|---|
| Bad file name | `forbidden` (the backstop). Nothing is read. |
| A symbolic link anywhere on the path, or a hard link | Refused. Nothing is read. |
| No such file, or not a regular file | Named refusal. |
| Not the exact WAV format | Named refusal. |
| Over 30 clips this hour, or 60 minutes today | `limit: ...`, try again later. |
| whisper-server not ready, or the GPU busy for 2 minutes | Try again later. Not counted. |
| Longer than `-agent-max-seconds` | `file ... is too long`. Nothing is sent. |
| whisper-server failed, or took longer than its bound | `the transcription failed`. Counted. |

The tool writes one line per call to `tool-logs/whisper.log`: the caller, the file name, the
clip's length, the outcome and the latency. Never the text.

## Trust boundaries

| Who | Reaches | Why |
|---|---|---|
| The remote proxy | the dictation socket | It runs as your user. It checks, before each clip, that the socket's directory is private and yours. |
| Agents | the tool's MCP backend, through the broker only | The broker forwards `/mcp/whisper/` with the tool secret. The tool offers only `transcribe`, and only with a capability, which reads files from the tree root's `.lever-files/whisper/` whoever holds it. Config load refuses the backend port in `manager.allow_ports`. |
| Agents | not the socket, not whisper-server | The jail reaches no host Unix socket, and config load refuses the socket path inside the tree. The egress rules drop every host loopback port that is not allowed, and `-whisper-port` is never allowed. |
| Other local users | whisper-server's port | Any local user can connect to a loopback port and can read the child's secret route prefix in the process list. Run the tool only on a host where you trust every local user. See the security notes under [dictation](/remote-access/#dictation-and-read-aloud). |

## Stopping and restarts

The broker stops the tool with SIGTERM, and the tool stops its child within a second. On Linux the
child also dies with the tool when the tool is killed. On macOS a tool killed hard can leave
whisper-server running and holding `-whisper-port`; the next start then finds the port taken and
starts no child (`lever doctor` shows the tool not ready). Find the process with
`lsof -nP -iTCP:<port> -sTCP:LISTEN`, stop it, and run `lever reload`.
