---
title: "The tts tool"
nav_order: 5.854
parent: Tools
permalink: /tts-tool/
---
The `tts` broker tool (`lever-tool-tts`) reads agent messages aloud on the chat page with a natural
neural voice that runs on your own host. It serves the chat page's
[read-aloud](/remote-access/#server-voices) when `remote.voice.read_aloud` is `server`. Agent text
goes only to your host, never to a cloud speech service. Part of [tools](/tools/); see also the
[whisper tool](/whisper-tool/), its speech-to-text counterpart.

## What it does

For each part of a message the page reads aloud, the remote proxy sends the text to the tool's
Unix socket (`-speak-socket`, the same path as `remote.voice.speak_socket`). The tool runs
[sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx)'s `sherpa-onnx-offline-tts` once, as its
child, with a speech model from lever's pinned table, and answers the audio as WAV. The default
model is Kokoro (`kokoro-en-v0_19`: 11 English voices, American and British, such as `bf_emma` and
`bm_george`).

The socket is not reached through the broker. It is `0600` in a private `0700` directory, so only
processes of your user can connect. There is no long-running engine and no TCP port: nothing
listens but the socket and the tool's MCP backend.

**The guarantees:** text and audio are never stored or logged. The jail cannot reach the socket,
and the engine has no port. The tool runs the engine only with model files whose sizes and sha256
match lever's table, and only from a program file that no one but you or root can change. The
engine gets a reduced environment and no stdin, and its output is discarded (it prints the text it
speaks).

**For agents** the tool offers one read-only MCP operation, `voices` (the model, its voices and the
default). The broker needs a tool to register at least one operation; `voices` gives an agent
nothing to speak with, and no agent can call it without an `obtain` grant.

## Requirements

- sherpa-onnx's `sherpa-onnx-offline-tts`, which you install yourself (below). Install it outside
  the tree, owned by you or root and writable by no one else. Lever never builds or installs it.
- Disk space for the model: about 350 MB for `kokoro-en-v0_19` and 80 MB for
  `piper-en_GB-alba-medium`, espeak-ng's data included. The engine runs on the CPU.
- Install the tool with `make install`. Releases ship it for darwin and linux.
- [Remote access](/remote-access/) with the chat page and `remote.voice` on (read-aloud is part of
  it).

### Install sherpa-onnx

Build it from source (CMake and a C++17 compiler), or unpack a prebuilt release archive for your
platform from the project's GitHub releases. From source:

```sh
git clone https://github.com/k2-fsa/sherpa-onnx
cd sherpa-onnx && git checkout <a release tag>
mkdir build && cd build
cmake -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF ..
make -j4 sherpa-onnx-offline-tts
sudo install -m 0755 bin/sherpa-onnx-offline-tts /usr/local/bin/
```

A static build (`BUILD_SHARED_LIBS=OFF`) is one file. With a shared build or a prebuilt archive,
keep the program with its `lib/` directory, for example under `/opt/sherpa-onnx/`, owned by root,
and point `-server` at `/opt/sherpa-onnx/bin/sherpa-onnx-offline-tts`. Check it with
`sherpa-onnx-offline-tts --help`. The tool checks the program file and its directory, not the
libraries next to it: keep `lib/` as safe from other users as the program. The tool resolves the program's real path once at start and
refuses it if it later resolves to another file; restart the tool (`lever reload`) after an
upgrade.

## 1. Configure the tool

```yaml
broker:
  tools:
    - name: tts
      command: [/home/you/.local/bin/lever-tool-tts,
                -tree, /home/you/my-instance/workspace,
                -models, /home/you/my-instance/.lever-state/voice-models,
                -server, /usr/local/bin/sherpa-onnx-offline-tts,
                -model, kokoro-en-v0_19,
                -voice, bf_emma,
                -speak-socket, /home/you/my-instance/.lever-state/tts/speak.sock]
      backend: 127.0.0.1:3213
      operations:
        - {name: voices}
remote:
  voice:
    enabled: true                                                       # with the rest of remote.voice
    socket: /home/you/my-instance/.lever-state/whisper/dictate.sock     # dictation (whisper tool)
    read_aloud: server
    speak_socket: /home/you/my-instance/.lever-state/tts/speak.sock     # the tool's -speak-socket
```

The broker adds `-backend` and `-admin` itself. Every other setting is a flag in `command`. Lever
recognises the tool by its program's base name, `lever-tool-tts` (also behind an `env` prefix), or a
command that takes `-speak-socket`, and reads these flags only in its command:

| Flag | Required | Meaning |
|---|---|---|
| `-tree` | yes | Absolute path of the instance tree. Config load requires it and refuses a value that is not the instance's `tree`. |
| `-models` | yes | Absolute path of the directory with the models. `lever voice fetch` downloads into it, each speech model in its own directory. Outside the tree. Each run's output is written to a private directory in it and removed at once. It may be the whisper tool's `-models`. |
| `-server` | yes | Absolute path of `sherpa-onnx-offline-tts`. Outside the tree; a `manager.read_only` entry does not excuse it. Its real file must be an executable owned by you or root that no one else can write to, in a directory no one else can write to. |
| `-model` | no | A name from lever's pinned speech table: `kokoro-en-v0_19` (default) or `piper-en_GB-alba-medium` (lighter, one voice, less natural). |
| `-voice` | no | The default voice, one of the model's. Default: `bf_emma` for Kokoro, `alba` for the Piper model. The page's voice picker can choose another per device. |
| `-speed` | no | Speaking rate, `0.5` to `2.0`. Default `1`, the model's own pace. |
| `-threads` | no | CPU threads of one run, `1` to `16`. Default `2`. |
| `-max-chars` | no | The longest text of one request, in characters, `100` to `2000`. Default `600`. The page splits a message into parts of at most this many. |
| `-speak-socket` | yes | Absolute path of the speech socket, outside the tree, at most 103 bytes. Its directory must be yours and private (`0700`); the tool creates it when it is missing. `remote.voice.speak_socket` must be the same path, and it must not be a dictation socket. |
| `-name` | no | Tool name. Default `tts`. It must equal the `name` in the config. |

Config load checks each of these (the model and voice against lever's table, the numbers against
their ranges) and refuses the program, the models and the socket inside the tree. The Kokoro voices
of `kokoro-en-v0_19` are `af`, `af_bella`, `af_nicole`, `af_sarah`, `af_sky`, `am_adam`,
`am_michael` (American) and `bf_emma`, `bf_isabella`, `bm_george`, `bm_lewis` (British).

## 2. Fetch the model

```sh
lever voice fetch kokoro-en-v0_19
lever voice fetch                  # the whisper tool's model, and the tts tool's
```

The files come from Hugging Face at the commit lever pins: the ONNX model, its tokens, Kokoro's
voice file and espeak-ng's data. Lever keeps each file only if its size and sha256 match its
table. Neither the tool nor the proxy ever downloads a model. Before every run the tool checks the
program and the model: it hashes every file again whenever a file's metadata (inode, size, times,
owner, mode) changed since its last full check, so a model fetched later, or replaced, is checked
at the next request. With no tts tool configured, `lever voice fetch` downloads into
`.lever-state/voice-models/`; point `-models` at that absolute path.

Then run `lever reload` and check `lever doctor`: the tool's own row, and the `tts tool` and
`tts model` rows. `tts tool` asks the speech socket for its health (not running, not ready, or
ready with the model, voice and limits); `tts model` checks every file of the `-model` in
`-models` against lever's pinned sizes and sha256. With `read_aloud: server` they check the tool
that serves `remote.voice.speak_socket`; otherwise the first tts tool in `broker.tools`. The `voice`
row says which read-aloud the chat page uses.

## How a request runs

1. The page cleans the message for speech (markdown removed, a code block read as "code block
   skipped", a link as its text or host name), splits it into parts of whole sentences, the first
   one short so the voice starts soon, and asks for one part at a time, the next while one plays.
2. The proxy checks the login, the request and the limits ([below](#limits)), then posts the text to
   the socket.
3. The tool cleans the text again: control and format characters and angle brackets become spaces,
   and no part starts with `-` (sherpa-onnx would read it as an option). It refuses an empty text,
   one over `-max-chars` and a voice the model lacks.
4. It runs `sherpa-onnx-offline-tts` once, with a private output directory under `-models`, reads
   the WAV it wrote (16-bit PCM, mono, at the model's rate), deletes the directory, and answers a
   WAV it writes itself from those samples. Nothing else of the engine's file goes on.

One run at a time. A run gets 2 minutes; a request whose page went away kills its run. On Linux
the run also dies with the tool when the tool is killed; on macOS a run left behind by a tool killed
hard ends by itself when its one text is spoken, and a later start removes its output directory
once it is ten minutes old.

The tool writes one line per request to `tool-logs/tts.log`: the length, the voice, the outcome and
the latency. Never the text.

## Limits

The proxy's limits, per login, kept in memory (they start again when the proxy restarts):
40,000 characters an hour, 200,000 a day, and 1,200 requests an hour. Three requests at a time in
all, of which a contact never takes the last, so an operator can always listen; one at a time per
login; one with the engine at a time. A text the engine never got (the tool was down or refused
it, or the page gave up while it waited) does not count. A login with `voice: false` gets no
read-aloud at all.

## Trust boundaries

| Who | Reaches | Why |
|---|---|---|
| The remote proxy | the speech socket | It runs as your user. It checks, before each request, that the socket's directory is private and yours. |
| Agents | the tool's MCP backend, through the broker only | The broker forwards `/mcp/tts/` with the tool secret. The tool offers only `voices`, read-only, and only with a capability. |
| Agents | not the socket, not the engine | The jail reaches no host Unix socket, config load refuses the socket path inside the tree, and the engine has no port. |
| Agent text | the engine, as plain text | Bounded, cleaned, never markup, never an option. The engine gets no path in the tree (its files are outside it) and is not expected to use the network, but it is not sandboxed: it runs as your user. Install a sherpa-onnx you trust. |
| Other local users | the engine's command line | The text of a part is the engine's argument, so a local user who can list processes with their arguments can read it while it runs (on Linux every user can, unless `/proc` is mounted with `hidepid`). Run the tool only on a host where you trust every local user, as for the whisper tool. |
