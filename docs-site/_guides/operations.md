---
title: Operations & recipes
nav_order: 9
---
# Operations & recipes

Recipes for running an instance day to day.

## Changing config on a running instance

The broker reads `lever.yaml` **once, at its own startup** — and a re-run of `lever up`/`apply`
deliberately keeps a healthy broker process running. So editing the config (new worker, new tool,
changed grants) and re-applying **silently changes nothing at the broker layer**: the symptom is a
correct-looking config plus unexplained denials ("unknown recipient", 403s for a grant you can see
in the file).

Use `lever reload`:

```sh
lever reload   # stops the broker, re-runs apply on the current config, spawns a fresh broker
```

It restarts the broker onto the edited config, re-reads the worker declarations, and re-applies
egress — but leaves the running manager container alone (apply's start-manager sees it already
running), so the **manager's conversation is preserved** and your TTY is not taken. It's the broker
half of a `lever stop` + `lever up` without the VM power cycle or the re-attach. CA, keys, and
enrolments all persist. (A full `lever stop && lever up` still works and additionally power-cycles
the VM if you want that.)

reload validates the edited config *before* it stops the broker, so a config typo fails with the
old broker still serving. If a later step fails (backend, image load), the broker can be briefly
down until you re-run `lever up` — the same recoverable window as a `stop`+`up` that fails midway.
The instance's single scion project registration (`register-project`) is idempotent, so reload's
re-run of apply is a no-op there — it's the broker restart, not a re-registration, that picks up the
edited worker list.

> **reload adds and changes; it does not remove.** reload re-reads the config so the fresh broker
> knows the current worker list, but it does not tear down a worker you *deleted* from the config.
> That worker's container (if one was dispatched) keeps running with its already-minted capability
> tokens (valid until `broker.grant_ttl`, default 24h) even though the new broker no longer knows
> it — new mints and messaging to it are denied, but held tokens still work. To actually cut off a
> removed worker, stop it and revoke it before or after the reload: `lever-manager agent stop
> <worker>` (from the manager) and `lever revoke <worker>`, or `lever broker bump-epoch` to kill
> every outstanding token at once.

## Adding a worker to a running instance

1. Create the directory under your tree: `mkdir -p workspace/workers/newworker` (the dir must
   exist; lever won't create it for you — the broker `Stat`s it before dispatch).
2. Declare it in `lever.yaml`:
   ```yaml
   workers:
     - name: newworker
       dir: workers/newworker
   ```
   Add `obtain:` grants if it needs brokered tools (see [capabilities](/capabilities/)).
3. `lever init` — scaffolds the worker's `lever-agent` skill into the new dir.
4. `lever reload` — the broker must restart to learn the new worker (see above).
5. Verify: `lever doctor` (the `operator skills` row covers the new dir), then dispatch it from the manager
   (`lever-manager agent start newworker --task "…"`) or message it from the host
   (`lever msg send "…" --to newworker`).

scion caps how many agents run at once on one runtime broker (a lever instance has one): **12**,
the manager included, on scion `cf16e4b0` (2026-09) and later. A create or start beyond the cap is refused.
Stop idle workers (`lever-manager agent stop <name>`) rather than keeping many running; the cap
itself is a hub limit only a hub admin can raise.

## Worker slots the manager recycles

A worker's task is fixed when its record is created. For fixed worker slots that take one piece of
work after another (`deal-1` … `deal-5`, each reused when its deal ends), the manager needs a fresh
context per piece of work. Mark such a slot recyclable:

```yaml
workers:
  - name: deal-1
    dir: workers/deal-1
    recyclable: true
```

then `lever init` (the manager's skill gains the recycle directions) and `lever reload`. The manager can now run `lever-manager agent stop deal-1` and
`lever-manager agent recycle deal-1 --task "…"`: the broker deletes the worker's scion record
and staged ticket (what `lever worker purge` deletes) and starts it fresh with the new task.

**Plan the change with the manager's next fresh start.** Two parts of it reach the manager only
when it is created again:

- **The verb is in the manager image.** `lever-manager agent recycle` exists only in a
  `lever-manager` binary of this release or later. Rebuild the image (`make lever-image`, and
  rebuild an instance image built from it), then give the manager the new image with
  `lever up --fresh --keep-conversation`, which keeps the manager's conversation (plain `--fresh`
  starts a new one).
  Until then the manager's old binary has no `recycle` command.
- **The skill changes on an instance with contact logins.** Setting `recyclable: true` on the
  first worker and running `lever init` changes the manager's `lever-operator` skill. While the
  manager's session was started on the old skill, contacts cannot post to the manager; that ends
  only when the manager starts fresh (plain `lever up --fresh`, which backs the old conversation
  up first; `--keep-conversation` would keep the old skill too). Make the config change and the
  image rebuild together, then run one `lever up --fresh`.

- **What is lost:** the worker's conversation. Nothing can resume it afterwards.
- **What is kept:** the worker's `dir` (its work product). The next piece of work sees the files
  the last one left there; clear them in the task or from the host if it must not.
- **What the broker refuses:** a worker without `recyclable: true` (403), the manager, a worker
  that is running or changing phase, or whose container is still running whatever phase the hub
  shows (409, nothing deleted), a revoked manager, a worker that
  another start, resume, stop, wake or heal holds (503), and a second recycle of the same worker
  within a minute (429).
- **Audit:** each recycle is a `broker.decision` line with `op=worker`, `decision=allow` and a
  `recycle <name>` detail; a refusal is a `deny` (or `error`) line with its reason. The task is
  logged by size and its first 60 bytes only.

Leave `recyclable` off for a worker whose conversation is worth keeping: without it, only you can
discard a record (`lever worker purge NAME --force`). `lever doctor`'s *worker recycle* row lists
the recyclable workers.

## Where the logs are

All host-side state lives in `.lever-state/` at the instance root:

| File | What's in it | When to read it |
|---|---|---|
| `broker.log` | every capability decision — `allow`/`deny` with caller, tool, op, and the deny reason. Mint allows are a ledger line: the token `id`, the matched policy `rule` (`obtain:…`/`delegate:…`), `exp`, `epoch`, and any baked `constraints`. Gateway and LLM lines carry the same `id`, so a mint correlates with every later use — and denied use — of that token: `grep id=<id> .lever-state/broker.log`. (On deny lines the id is the token's *claimed* id — the signature was not necessarily valid.) | **first stop for any 403**: it names the difference between "no token attached", "not granted", and "revoked" |
| `broker.out.log` | the broker process's own stdout and stderr (startup, proxy errors) | broker won't start, or brokered tool calls 502 (backend refused) |
| `tool-logs/<tool>.log` | one file per supervised tool (its own stdout/stderr, not shared with the others, and the broker's lines about that tool: held back, or failed to start) | a specific tool misbehaves (never registered, crashed, or returned bad output) and you need forensics without other tools' output in the way |
| `broker.pid` | the daemonized broker's pid | `lever doctor` reads it for the alive check |
| `logs/bringup-<command>-<time>.jsonl` | one record per `lever apply`, `lever up` and `lever acceptance`: each step with its time and outcome, and for a failed step the log lines shown with it; the last 10 runs are kept | a bring-up was slow or failed and the terminal output is gone; `lever doctor`'s *last bring-up* row summarises the newest |

scion prints `WARN settings file contains unrecognized keys (these will be ignored)
keys=[hub.project_id runtimes[*].type ...]` on every CLI call in the guest. It is harmless: scion's
legacy-format probe reads the v1 settings file before the v1 loader, which does read those keys
(scion writes `hub.project_id` itself). lever hides this one line from what it prints and from
the scion errors it shows, and records each distinct copy once per lever process (the broker: once per run) in `broker.log` (a
`msg="scion output hidden from the console"` line). Any other scion warning is still shown. You
still see the line when you run `scion` by hand in the guest, and `lever attach` may show it
briefly before the agent's terminal takes over the screen.

### The shape of a log line

Every lever process that runs unattended writes one line shape: slog's text form, with the time
in UTC to the millisecond, a level, the message, the process named in `component`, then
`key=value` fields. A value with a space or a quote is quoted.

```text
time=2026-10-12T13:00:00.000Z level=INFO msg=broker.decision component=broker op=request caller=hello decision=allow detail="" id=…
time=2026-10-12T13:00:01.250Z level=WARN msg="not starting the tool: …" component=broker tool=notes
time=2026-10-12T13:00:02.400Z level=ERROR msg="gateway: resolve broker URL: …" component=lever-agent cmd=gateway
```

The level is `INFO` for what happened (an audit decision is `INFO` whatever it decided), `WARN`
for a problem the process carries on through, and `ERROR` for a failure: an operation that did
not happen, or a process that exits. Lines Go itself writes (an HTTP server's TLS handshake
error, a proxy error) are `INFO` whatever they say.

| Component | Where its lines go |
|---|---|
| `broker` | `broker.log` (its decisions), `broker.out.log` (start-up, warnings, Go's own lines such as an HTTP server's TLS handshake error), and `tool-logs/<tool>.log` for a line about one tool (held back, failed to start), with `tool=<name>` |
| `lever` | the few lines `lever` adds to `broker.log` (a scion line hidden from your terminal) |
| `lever-tool-<name>` (the tool's program, e.g. `lever-tool-db`) | `tool-logs/<tool>.log`, named after the tool in `lever.yaml`: the broker writes each supervised tool's output there |
| `remote-proxy` | `remote.log` |
| `lever-agent` | in the agent's container, with `cmd=<verb>`: `boot` (scion's pre-start hook) in the container's log; the `gateway` and `renew` sidecars in `~/.scion/services/logs/lever-gateway.stderr.log` and `lever-renew.stderr.log` in the agent's home; `serve-capability` in Claude Code's MCP server log |

A daemon that fails to start ends its file with the plain `Error: …` line `lever` prints. Files
written before 0.36.0 keep their old lines; `lever doctor` reads both.

Every line passes one redaction rule before it is written, in its message and in each field: an
env value in a `podman run` argv, Anthropic, GitHub and scion hub tokens, a JWT, an
Authorization or `*-Token` header, a bearer token and a `--…secret`/`--…token`/`--…password`
flag value are masked (`***`). That is a backstop; lever does not log a credential on purpose. A
tool written with the captool SDK gets the same shape and masking from `captool.NewLogger`, and
captool masks its decision lines' `detail` whatever logger the tool passes.

**Audit lines** are records that you and your scripts parse, so their form does not change: the
`broker.decision` lines in `broker.log` (fields `op`, `caller`, `decision` and `detail`, in that
order, then the decision's own fields such as `id`, `rule` and `reason`), a tool's decision
lines (`captool.decision`, `github.push`, `github.<op>`, `fizzy.<op>`) in its tool log, the
JSON-lines audits (`remote-audit.jsonl`, `directives.log`) and the ledgers.

What a person reads is not a log line but a message. `lever` prints on your terminal in one
form, `<who>: <text>`, and for a warning `<who>: warning: <text>`, where *who* is `lever`, `jail`
(the VM) or the bring-up step (`start-manager`, `load-image`). In an agent's container,
`lever-manager` prints its results and `Error: <text>` for the manager to read, and
`lever-agent`'s one-shot verbs (`request`, `delegate`, `call` and the Claude Code hooks) print
their result and, on a failure, `lever-agent: <text>`.

### Watching a bring-up

`lever apply`, `lever up` and `lever acceptance` list their steps as they run: the jail's
(start VM, guest hardening, guest state, packages and runtimes, scion, egress firewall, nested
virt), then apply's numbered steps, then the manager's. *Guest state* reads, in two guest calls,
what the jail steps after it check; on a jail that is already provisioned those steps then say
*already provisioned*, *verified, unchanged* or *unchanged* instead of running (the firewall is
always compared rule for rule, never assumed). The first bring-up after a VM restart provisions in
full once. On a terminal one live line shows the running step, what it waits for
(Lima's boot stage, "hub not answering yet", the manager start's stage) and its time; finished
steps scroll above it with their time and a note (*reused*, *already loaded*, *already running*).
The run ends with its total and its slowest steps.

Some steps run alongside others. The host's scion build (*build scion on the host*) starts before
the VM and runs while it boots; for the scion pin of a release it downloads that release's prebuilt
scion instead. `broker-up` and every `load-image` (the load and the keep-id
warm-up, one image at a time) start with apply and run while `init-machine`, `scion-server` and the
registration steps run; `mint-manager-bootstrap` and the remote proxy wait for the broker, and
`start-manager` waits for the broker and every image. The live line shows the step in front and
names the others in brackets (`[6/11] scion-server (+ load-image …): hub not answering yet`);
`plain` marks such a step's start with *alongside the next steps*. When a step running alongside
fails, its FAIL line and log tail print at once and the run stops after the step in front ends;
when the step in front fails, the steps alongside are stopped and listed as *skipped (stopped: an
earlier step failed)*; after Ctrl-C they read *stopped: the run was interrupted*. The error names every step that failed. `--progress` (or `LEVER_PROGRESS`) chooses the
output: `auto` (the default: the live line on a terminal, else `plain`), `plain` (a timestamped
line per step, and a *still running* line every 15 s for a long one), `json` (one JSON object per
event), `quiet` (the header, warnings and failures only).

When a step fails, the list shows the step, its time, and the last lines of the log that explains
it: `broker.out.log` for broker-up, `remote.log` for the remote proxy, the hub's log in the jail
(`~/.scion/server.log`) for the steps that talk to the hub, Lima's `ha.stderr.log` for the VM start; credentials in those
lines are masked. An interrupted run (Ctrl-C) shows no tail.
The manager container's own log is never shown: it is the agent's word.

`LEVER_TRACE=1` adds a line for every external command lever runs (`limactl`, `scion`, `podman`,
`go`) with its time and exit code. Only the command's leading words are shown, up to the first flag,
assignment or anything that is not a plain word, so a flag value, an env value or a script never
appears.

Agent-side, `lever attach [name]` shows the full scrollback including every incoming message and
tool call. Run `lever doctor` first whenever anything looks wrong; every check prints a fix hint.

### Log rotation

The plain logs do not rotate on their own — on a long-running instance they grow without bound.
Rotate them with a `logrotate` drop-in (adjust the path to your instance):

```
# The audit trail: keep it longer than the rest.
/path/to/instance/.lever-state/broker.log
/path/to/instance/.lever-state/remote-audit.jsonl {
    monthly
    rotate 24
    copytruncate
    compress
    missingok
    notifempty
}

/path/to/instance/.lever-state/broker.out.log
/path/to/instance/.lever-state/remote.log
/path/to/instance/.lever-state/tool-logs/*.log {
    weekly
    rotate 8
    copytruncate
    compress
    missingok
    notifempty
}
```

`copytruncate` avoids restarting the broker, the remote proxy or the tools on rotation: each holds
its file open in append mode, so it keeps writing to the same (now-truncated) file. Name the files
rather than globbing `.lever-state/*.log`: the glob also matches `directives.log`, which lever
rotates itself (below).

### What lever writes, and what to retain

Everything is in `.lever-state/`, mode 0600. Two kinds of file differ in who bounds them:

| File | What it is | Bounded by |
|---|---|---|
| `broker.log` | the capability and messaging audit trail | **you** — unbounded |
| `instance-id.json` | this instance's id and the state directory it was made for (see [two instances on one host](#two-instances-on-one-host)); delete it only with the broker stopped | lever — written once |
| `remote-audit.jsonl` | one line per request the remote-access proxy allowed or denied (only with `remote:` configured) | **you** — unbounded |
| `broker.out.log`, `remote.log`, `tool-logs/<tool>.log` | process output of the broker, the remote proxy and each supervised tool | **you** — unbounded |
| `directives.log` | the operator-directive audit log (issued, delivered, consumed, revoked, and each denial) | lever — past 1 MiB it is renamed to `directives.log.1`, replacing the previous `.1` |
| `sent-ledger/` | the text of every message lever sent to an agent, one file per recipient and kind; `message_verify` reads it | lever — each file past 1 MiB moves to `<file>.1` |
| `chat-ledger/` | verified web-chat posts, one file per login (only with `remote:`) | lever — each file past 1 MiB moves to `<file>.1` |
| `chat-verified.jsonl` | which messages an agent has already verified (one use each) | lever — past 4 MiB it moves to `.1`, and only once the old `.1` is 48 h stale |
| `agent-ledger/` | the agent messages to contacts the broker authorized, one file per contact (only with `remote.agent_messages`) | lever — each file past 4 MiB moves to `<file>.1` |
| `files-ledger/` | every chat upload and share with its sha256, one file per agent (only with `remote.files`) | lever — each file past 8 MiB moves to `<file>.1`; a file shared in a dropped generation can no longer be downloaded |
| `sessions.jsonl` | each agent's last fresh session start | lever — past 1 MiB it moves to `.1` |
| `voice-models/` | the Whisper and speech models `lever voice fetch` downloaded when no whisper or tts tool names its own `-models` (point the tool's `-models` here); never audio, a transcript or read-aloud text | **you** — lever never removes a model; delete one you no longer use |

The files lever bounds keep **one** previous generation: the current file plus its `.1`, so at most
about 2 MiB each (8 MiB for `chat-verified.jsonl` and an `agent-ledger/` file, 16 MiB for a
`files-ledger/` file), and older lines are gone. That is enough for
what lever reads them for (a message verifies for 24 hours), but it is not retention. Leave these
files to lever: do not point `logrotate` at them, since the broker reads both generations by name.

If you need the audit trail kept (who was allowed what, which directives were sent), the two things
to arrange yourself are the retention of `broker.log` and `remote-audit.jsonl` (the first stanza
above) and a copy of `directives.log` taken more often than it turns over — a nightly `cp` into
dated files is enough on any instance that does not send directives by the thousand. The ledgers
hold message **text**; ship them off the host only if you want that text kept.

### The hub's log, inside the jail

The scion hub runs in the guest as a daemon and writes `~/.scion/server.log` in the run user's home
(the guest's default user). scion opens it in append mode and never rotates it; it is chatty at the
default level. Lever does not install a rotation for it, and the guest is not provisioned with one,
so add a drop-in in the guest. Get a root shell there with `orb -u root -m lever-<name>` (OrbStack)
or `limactl shell lever-<name>` then `sudo -i` (Lima), install `logrotate` if the guest lacks it
(`apt-get install -y logrotate`), and write `/etc/logrotate.d/scion-hub`:

```
/home/<run-user>/.scion/server.log {
    su <run-user> <run-user>
    weekly
    rotate 4
    copytruncate
    compress
    missingok
    notifempty
}
```

Use the path `echo ~/.scion/server.log` prints for the run user (a Lima guest's home is not always
`/home/<user>`). `copytruncate` matters here too: lever starts the hub and expects to stop it, so
the rotation must not restart it. `su` is needed because the file is in a user-owned directory. The drop-in lives on
the guest's disk: it survives `lever stop`/`up`, and `lever destroy` removes it with the machine.

### Running under systemd or launchd

Lever ships **no unit file**, and it is not a service in the supervisor's sense.
`lever up --no-attach` (or `lever apply`) is a one-shot bring-up: it powers the jail on, starts the
hub, starts the broker (and the remote proxy) as **detached** host processes with their own pid
files, and returns. `lever stop` stops those host processes by pid, suspends the manager and the
running workers, and powers the jail off. Nothing restarts a broker that dies; the next `lever up`,
`apply` or `reload` starts one.

So do not wrap `lever broker serve` in a unit with a restart policy: `lever stop` and
`lever reload` stop the broker by its pid file and start their own, and a supervisor that brings it
straight back fights them. Log to the files above and rotate them; there is no journald output.

What a supervisor can usefully do is run the bring-up at boot and the stop at shutdown. The unit
below only wraps the two documented commands. It is an **untested example**, not a shipped or
supported file — check it on your host before relying on it:

```ini
# ~/.config/systemd/user/lever-myinstance.service   (example, untested)
[Unit]
Description=lever instance myinstance

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=/path/to/instance
ExecStart=/usr/local/bin/lever up --no-attach
ExecStop=/usr/local/bin/lever stop
TimeoutStartSec=15min

[Install]
WantedBy=default.target
```

Run it as the user that owns the instance (a user unit, with `loginctl enable-linger <user>` so it
starts without a login), with the same `PATH` your shell gives `lever` — the backend CLI
(`limactl`), and the Go toolchain the bring-up resolves, must be on it. On macOS the same two
commands go in a LaunchAgent (`RunAtLoad`), in the user's session; OrbStack itself must be running
first.

## Agent session heals

Two faults leave an agent's container up and most doctor rows green. lever heals both on that
agent's own record, with no restart, and the conversation is kept.

**An expired agent hub token.** scion gives each agent a 10 h hub token, and sciontool refreshes it
2 h before expiry with a timer that stands still while the host sleeps. An expired token cannot
refresh itself: every reply, status update and heartbeat of that agent then fails with
`401 … token is expired`. The heal is `scion reset-auth` under the controller PAT; the hub mints the
new token from the agent's stored role, so nothing widens.

- `lever apply` (and `lever up` on a running manager) heals the manager and every configured
  worker whose container is live and whose phase is `running` or `stopped`.
- `lever stop` heals a manager in phase `stopped` before it suspends it.
- The broker reads the tokens once at its start and then every 5 minutes, and resets an expired
  token of a `running` agent itself, at most once per agent in 15 minutes. `broker.auto_reenrol`
  sets who it covers (`all`, the default; `manager`; `off`). Each outcome is a `broker.log` line
  with `op=hub-token`.
- A revoked agent (`lever revoke`) is never healed, and a record from before scion's stored roles
  is refused (the pre-role record guard).

Doctor's `agent hub tokens` row reads the token of the manager and of every configured worker
whose container is live. It fails on an expired token of a `running` or `stopped` agent. It warns
when a refresh is overdue, or when a token expired under another phase (the resume that phase
needs issues a new token). A running manager's expired token fails the `manager agent` row, and
this row warns and points there. An agent whose token lever cannot read is listed as
"not checked (token unreadable)" with the reason: `no container`, `timed out`,
`output over its limit`, `unexpected output` or `exit status <n>`. Doctor never prints what the
container wrote.

**A stopped phase over a running claude.** sciontool reports phase `stopped` on any SessionEnd
hook in the container, and every claude process there shares the agent's hooks. So a `claude`
command run in the container (for example `podman exec … claude mcp list`) marks the agent stopped
when it exits. The hub then refuses `lever attach` and resumes the record in a new claude session
(only a suspended record gets `--continue`). `lever apply` (manager and workers) and `lever stop`
(manager) have the agent report its session running again, and the conversation continues.
Doctor's `manager agent` row reports this state.

> **Never run `claude` in an agent container.** Use `lever attach` to see the session and
> `lever doctor` to check it. The operator skill gives the manager the same rule for its own
> container.

**Lima: the run user's uid.** The jail's rootless podman finds the agent containers only under
the run user's `XDG_RUNTIME_DIR`, which comes from that user's uid (1000 in a Lima guest).
`lever doctor` and `lever worker purge` read the jail run user's uid before they use the jail, as
`lever apply` and the broker do. Before 0.32.0 they used the default uid (501), so on Lima the
`agent hub tokens` row read "not checked (token unreadable): … (no container)" for every agent,
and `worker purge` looked for the worker ticket in the wrong runtime directory.

## Create-time manager settings

scion keeps a record's mounts and devices for life. Two manager settings therefore reach only a
freshly created manager: change them, then run `lever up --fresh --keep-conversation`, which
recreates the manager and carries its conversation over
([below](#the-managers-conversation-across---fresh)). `lever stop` + `lever up` and `lever reload`
keep the old mounts; `lever apply` warns.

- **`manager.read_only`** — the listed tree directories are read-only in the manager, and while
  the list is set every worker dir and the parents of each entry are pinned (bind-mounted over
  themselves). Keep host-run code (an operator CLI, scripts) under an entry. Edit a protected
  directory in place: replacing it on the host (`rm -rf` and recreate, a rename deploy, a
  `git checkout` that removes it) leaves the read-only mount on the old directory until the next
  fresh create. Removing an entry, or a worker while the list is set, also needs `--fresh`, and the
  directory must stay on the host until then (see the troubleshooting row for `cannot be
  resumed`). Doctor's `manager read-only paths` row checks every entry and pin of the running
  manager from the guest side (the kernel's mount table and the directory behind each mount),
  without running anything inside it. Details and limits:
  [`read_only`](/reference/config/#manager).
- **`nested_virt`** (Lima on an x86_64 Linux host) — gives the **manager** container `/dev/kvm`;
  workers and the hub do not get it. `lever apply` checks the host's KVM `nested` module and
  installs the guest udev rule (`/dev/kvm` mode 0666); a jail VM created without `/dev/kvm` needs a
  recreate (`lever conversation backup`, `lever destroy`, `lever up`, `lever conversation restore`). Doctor's `nested virt` row
  checks the guest device, the manager's device, and that the old podman drop-in that gave every
  container the device is gone (`lever apply` removes it; workers created while it was there keep
  the device until they are recreated). Turned off, the row fails while the manager still holds
  the device. See [`nested_virt`](/reference/config/#top-level).

## The manager's conversation across `--fresh`

A manager keeps the image, model, mounts and devices it was created with, so changing one of them
needs a fresh create: `lever up --fresh`. That deletes the manager's scion record and, with it, the
record's home in the jail, where Claude Code keeps the conversation (`~/.claude/projects`).

- **`lever up --fresh`** backs the conversation up before it deletes anything: it suspends a
  running manager, copies its `~/.claude/projects` to `.lever-state/conversations/<id>/` and prints
  the path. If the backup fails, or lever cannot find the manager's home in the jail, nothing is
  deleted, the manager is resumed and `up` stops: fix the cause, or run
  `lever up --fresh --no-backup` to discard the conversation without a copy. The new manager
  starts a new conversation.
- **`lever up --fresh --keep-conversation`** does the same, then, once the new manager is live,
  suspends it, puts the backed-up transcripts in its home and resumes it, so `claude --continue`
  picks up the old conversation. Use it for an image, model, mount or device change. The new
  manager's boot turn is cut short, and its transcript is moved aside in the manager's home
  (`~/.claude/projects.lever-replaced-<stamp>`). If the restore fails, the backup stays:
  `lever conversation restore <id>` tries again.
- **`lever conversation list`** shows the backups, newest first. **`lever conversation backup`**
  takes one by hand (the jail must be up; a running manager is copied as it runs).
  **`lever conversation restore [<id>]`** puts one back, the newest by default, into a running or
  suspended manager: it backs up the conversation it replaces (`--no-backup` skips that), suspends
  the manager, swaps the transcripts and resumes it. lever keeps the last 10 backups.
- **A manager with no conversation to continue.** A manager that was started and never sent a
  message has no transcript, and scion resumes a suspended or crashed manager with
  `claude --continue`, which then stops with "No conversation found to continue". When that
  resume runs in `lever up`'s bring-up (a crashed manager, and every `up` after `lever stop`), `up`
  then looks at the manager's home from the guest. Only when `~/.claude/projects` is missing or
  holds no transcript with content, holds no link or special file, and has no other `projects*`
  directory beside it, does `up` start the manager fresh, as `lever up --fresh` would (backup
  first, saved only if there is something in it), and say so. Otherwise it keeps the record,
  prints why, and names the cause with `lever up --fresh` as the way out. This works the same on
  a terminal: `up` attaches your terminal to a resumed manager only once claude has reported its
  session start, which a `--continue` with no conversation never does. When `up` resumes a
  suspended manager itself (the hub was still running, or the guest restarted under it), it names
  the cause and keeps the record; `lever stop && lever up` then makes the fresh start (the resume
  after `lever stop` runs in the bring-up), and so does `lever up --fresh`. `lever apply` names
  the cause and never starts a manager fresh.

  The fresh start `up` makes on its own does not move the contact baseline (see
  [remote access](/remote-access/)): it records the new session only when the manager's lever
  skill on disk, read before the delete and again right before the create (when the old container
  is gone), is the one its last recorded session started with. The agent can bring this start
  about (an empty home, a claude that exits) and can write the skill in the tree, so when the
  skill has changed, `up` records a session that matches no skill and says that contacts cannot
  post to the manager until it next starts fresh other than on its own (`lever up --fresh`).
  Putting the old skill text back does not lift that. A manager lever has no session record of
  gets none from this start either, so contacts stay refused. A conversation from such a session
  keeps the refusal through `--fresh --keep-conversation` and `lever conversation restore`.

  On a terminal, a resumed manager whose claude waits at a dialog before its session starts (a
  login, trust or update prompt) is attached only after the 2 min wait for its session start;
  `lever attach` from another terminal reaches it sooner.
- **Recreating the VM** (`lever destroy`, `lever up`) loses the agent homes too: run
  `lever conversation backup` first and `lever conversation restore` after. `lever destroy` does
  not touch the state directory.

Only the transcripts are carried over. `~/.claude.json` (the account identity, onboarding state,
the MCP configuration lever writes when the agent boots, and in api-key mode the key) stays the
new record's own.

**Security.** A backup holds the whole conversation, with whatever secrets the agent saw. It lives
in the state directory, never in the tree: lever keeps no backup when the state directory is inside
the tree. Directories are 0700 and files 0600, and lever never prints a backup's content. Every byte
of it was written by the agent, so lever never follows a link the agent made in its home, keeps only
directories and regular files with clean relative names, refuses an archive past its limits (1 GiB
a file, 4 GiB in all), and restores only that checked copy into a directory it has just created.
A restored conversation keeps the lever skill it was started with: the restore records that skill
(from the backup) as the manager's session, so contacts can post to the manager only while that
skill is the one on disk; a backup that does not know it refuses contacts until the next plain
fresh start.

**When not to keep it.** To get rid of a session you no longer trust (a compromise, a prompt
injection), use plain `lever up --fresh`: `--keep-conversation` brings the session back.

## Shared folders

`shared_folders` gives a set of agents one common directory at `/shared/<name>`: writers mount it
read-write, readers read-only, and nobody else mounts it ([reference](/reference/config/#shared_folders)).
Use it when some agents produce files that others consume: reference material, a hand-off folder
between workers, build outputs, or reviewed tools that one agent publishes for the rest.

**Adding a folder:**

1. Create the folder in the tree as a real directory (not a symbolic link), outside every worker
   `dir`.
2. Add the entry to `lever.yaml` and run `lever apply` (or `lever reload`): the broker reads the
   config only when it starts, and a change to `shared_folders` restarts it.
3. Recreate the agents that should mount it: mounts are create-time only. The manager comes
   first, because it mounts every folder (read-only unless it writes it) and holds the pins that
   protect the folder: run `lever up --fresh --keep-conversation`. Until it has them, the
   broker refuses to start or resume any worker. Then, for each worker,
   `lever worker purge <worker>` (or `lever-manager agent recycle` from the manager for a
   recyclable worker), and dispatch it again.
4. Run `lever doctor`: the *shared folders* row lists the agents whose mounts match and names any
   that still need recreating; the *manager read-only paths* row checks the manager's pins.
5. Tell the agents about it: lever's skills do not mention shared folders, so say in each agent's
   `instructions_file` (or task) what `/shared/<name>` holds and whether that agent may write it.

**Changing or removing a folder** works the same way: edit `lever.yaml`, run `lever apply` (it restarts the broker), then
recreate the agents whose access changed. Until then the broker refuses to resume a worker whose
record holds access the config withdrew, and doctor fails for any agent that still holds it. Keep a
removed folder on the host until every agent that mounted it is recreated: a resume needs the mount
source. Adding a **worker** while any folder is configured also needs a fresh manager, as with
`manager.read_only`: the manager pins every worker dir, and the broker holds back every worker until
the running manager has the new plan. (Removing one leaves an unused pin, which `lever apply` only
warns about.)

**Updating a folder's contents.** Bind mounts show new files at once, so readers see a change at
their next read without a restart. Change the folder's contents, never the folder itself: a mount
stays on the directory that existed when the container started, so replacing the folder on the host
leaves agents on the old one (doctor's *shared folders* row names the readers affected; recreate
them). Write a file under a temporary name and rename it into place, so a reader never sees half of
it. For versioned content, add a new subdirectory per version and point at it with a relative
symbolic link or a small file naming the version; switching back is moving the pointer back.

**Before you add a writer**, read [security model §4.5](/security-model/worker-isolation/#45-opt-in-shared-folders):
whatever a writer publishes, the readers and the manager may run.

## Claude Code settings and the after-compaction note

`claude_settings` and `after_compact_note` (under `manager:` or a worker) configure the agent's
Claude Code. They are **start-time** settings, not create-time ones: once the agent runs an image
that can deliver them, a change needs no `--fresh`.

```yaml
manager:
  claude_settings:
    auto_compact_window: 400000
  after_compact_note: Re-read NOTES.md and the open cards before you go on.
```

- **How they arrive.** The host puts them in the agent's enrolment envelope (`bootstrap.json`): for
  the manager when apply mints its ticket, for a worker before every dispatch and resume. At every
  container start, `lever-agent boot` (in scion's pre-start hook) writes them into Claude
  Code's managed-settings file, `/etc/claude-code/managed-settings.json` (lever's image creates
  that directory for the `scion` user, because under rootless podman the hook runs as that user):
  `auto_compact_window` as `env.CLAUDE_CODE_AUTO_COMPACT_WINDOW`, the note as a `SessionStart` hook
  with matcher `compact` (the session start that follows a compaction), which runs
  `lever-agent after-compact` and gives the agent the note, marked
  `[after compaction: note from this instance's lever.yaml]`. scion's own hooks stay in `~/.claude/settings.json`;
  Claude Code runs both sets. With nothing configured, boot writes only the switch that turns
  background sessions off (see [background sessions and /login](#background-sessions-and-login)).
- **Changing them.** Edit the config and run `lever reload` (or `lever apply`): the broker
  restarts and the manager's envelope is staged again. A running agent keeps the values of its
  last start; the new ones apply at its next start (`lever stop && lever up` for the manager, a
  stop and a resume for a worker). That holds for a removed setting too: a running agent keeps
  what lever wrote until its next start. `lever apply` warns while the running manager differs,
  and doctor's `claude settings` row compares each running agent with the config.
- **What the agent can change.** The settings are a configuration aid, not a boundary. The
  managed-settings file is in the agent's own container and belongs to the `scion` user that runs
  Claude Code, so the agent can change it during a session; boot writes it again from your
  lever.yaml at the next start, unless it cannot (the agent can also make the file or its
  directory unwritable; boot then logs `claude settings not delivered`, or, with no claude config,
  `agent-view switch not written`). The env value ranks above
  the agent's own settings files and `/autocompact`. The manager's envelope is in the tree it can
  write, so the manager can rewrite the copy staged for its own next start
  (`.lever/bootstrap.json`) and give itself another window or its own after-compaction note,
  within the same validated bounds; a broker restart (`lever reload`) stages a fresh one. This is
  no new power: the manager can already write a `CLAUDE.md` in its tree, which Claude Code
  reloads. Nothing in this path goes from the agent to the host: the host only reads back an
  integer, a checksum of the note and whether the agent-view switch is on, for doctor and apply.
- **A failed delivery never stops the agent** (lever-agent 0.34.1 or later, in the image). If
  boot cannot write the file or the envelope's block is invalid, it logs
  `claude settings not delivered` and the agent starts without them; doctor's `claude settings`
  row shows what the file holds (the agent's own file, so read it as the agent's word),
  including `agent view: off` or `agent view: not set`. An image
  with the 0.34.0 lever-agent and no `/etc/claude-code` still fails to start: rebuild it, or
  remove the two settings and run `lever apply` to unblock the agent at once.
- **Images.** Delivery needs the new `lever-agent` and the new pre-start hook in the agent image.
  Run `make lever-image` (and rebuild any instance image built `FROM` it), then put each agent on
  a new container: the manager with `lever up --fresh --keep-conversation`, a worker with `lever worker purge <name>` or a recycle. `lever stop && lever up`
  is not enough: it resumes the record on its old image. An older image ignores the block, and
  doctor's row shows the difference.

## A frozen attach view

The view `lever attach` (or `lever up`) gives you can freeze, or keep an old terminal's size,
while the agent and Claude Code run normally. The usual cause is a stale tmux client: an earlier
attach whose terminal went away (a closed window, a dropped ssh session, a host that slept) is
still attached to the agent's tmux session.

By default `lever attach` and `lever up` leave the other terminals attached to the agent alone,
so a second terminal (or a terminal in scion's web UI) can watch alongside the first.
`--detach-others` detaches them before the attach, as `tmux attach -d` does, and prints how many
it detached. It never detaches the container's own tmux client: scion starts the agent's tmux
session with a client attached in the container's main process, and detaching it would stop the
agent. Only clients a `podman exec` started (an attach) are detached, and a terminal in scion's web UI
is one of them, so `--detach-others` drops it too; a client lever cannot place is left alone.

When a view freezes:

1. Press `Ctrl-L`: Claude Code clears and redraws its screen and keeps the conversation.
2. If it stays frozen, open another terminal and run `lever attach --detach-others` there: it
   detaches the frozen terminal's stale client and attaches in its place. Or detach the frozen
   one with `Ctrl-b d` and run `lever attach --detach-others` in it: a plain `lever attach`
   leaves the stale client attached.
3. If a stray key opened Claude Code's background-agent view, `Esc` leaves it. Do not move the
   session into the background (see [background sessions and /login](#background-sessions-and-login)).

To check whether the agent itself is working without attaching: `lever doctor` (the *manager
agent* row), or `lever msg send` and watch for the reply.

## tmux server crashes

scion runs each agent's harness (claude) in a tmux session inside the agent's container. The tmux
server holds claude's pane, so when the server crashes the harness and the container go with it.
The agent image installs a pinned tmux from trixie-backports, replacing the base image's 3.5a (see
[the tmux version](/getting-started/install/#1a-build-the-agent-image)).

**What lever reports.** sciontool logs the reaped server in the container log (`Reaped zombie
process … (tmux: server) (reason: killed by signal segmentation fault …)`). When the manager died,
`lever up` (after your attach ends, or at the start gate) reads the last such line and names the cause: a
crash (a fault signal: segmentation fault, abort, bus error) or a kill (SIGKILL, usually the guest's
out-of-memory killer; the guest's `sudo dmesg` names the victim). lever matches the fixed line only
and repeats nothing of the log. `lever up` starts the manager again; the conversation is kept when
it had one.

**The core.** In a Lima jail, lever sets the guest's `kernel.core_pattern` to a handler,
`/usr/local/libexec/lever-core-handler` (with `/etc/sysctl.d/99-lever-core.conf`, converged on every
apply and up). It keeps the core dump of a process named `tmux: server` (or `tmux`) and drops every
other dump. The cores stay in the guest, in `/var/lib/lever/cores` (root only, mode 0700, files 0600,
named `core.tmux.<time>.<pid>`); lever never copies one to the host or into a container. A tmux
server core holds the terminal contents of the agent's panes, and the server's memory besides,
which can include its environment (the container's environment, and with it any token or key
passed in it), so treat it as you would the agent's conversation and its secrets. The agent can
crash its own processes, so the handler bounds what it keeps: one dump at a time, each cut at
256 MiB, the newest two, and none while the newest kept core is less than a minute old (a crash
loop keeps one core a minute). The kernel runs the handler as root, outside the container's
limits, so lever also sets `kernel.core_pipe_limit` to 1: one handler at a time, and the kernel
skips a dump that would start a second. The same freedom lets the agent drop
a real core or push it out with fakes, and print a fake reaper line: the hint and a kept core are
leads, not proof. A piped dump does not depend on the
container's core size limit, so nothing in the agent image or its container changes. The handler
replaces the guest's own core handler: Ubuntu's apport is stopped and masked, and other processes' dumps are
dropped. An OrbStack machine shares one kernel with every other machine on the host and lever
does not change its `core_pattern`, so there no core is kept.

To read a core, copy it out of the guest by hand (`limactl shell <vm> sudo cat
/var/lib/lever/cores/<file> > core`) and open it with gdb against the same tmux build: the image's
`tmux_version` label names the Debian package version; its debug symbols are that version's
`tmux-dbgsym` package from Debian's debug archive.

**Several attached terminals.** Each attach is a tmux client of its own size, and scion's sessions
use `window-size latest`, so a client that gets input resizes the window, and claude redraws its
whole screen. The one 3.5a crash seen in a manager was a read past the end of a pane's grid in
code that copy mode uses (the mouse wheel enters copy mode when claude does not take the mouse),
and an upstream report ties that code to a size change while in copy mode. Several terminals
attached at once (other `lever attach` terminals, or scion's web terminal) make size changes more frequent, so they
plausibly make it more likely; it has not been reproduced. The container's own tmux client is always attached too.

## Background sessions and /login

**Do not use `/login` or background mode in an agent tmux.** lever supplies the agent's login
(subscription mode) and restarts the harness with `claude --continue`; both features fight that.

- **Every agent reports its session start to lever.** At every start, `lever-agent boot` also
  adds a `SessionStart` hook with no matcher, `lever-agent claude-ready`, to the managed-settings
  file. When claude has started a session in scion's tmux pane, the hook writes
  `/tmp/lever-claude-ready` in the container: the guest's boot id, the start time of the
  container's first process and the pane's id. After a start or resume of the manager, lever
  waits for that file to match the container and pane it sees, not for a fixed time. When the
  file does not come in 2 min while claude's pane stays alive, lever falls back to a 3 s settle
  window and, if the pane is still alive after it, reports the manager up with a warning that the
  readiness signal is missing. The file claims only that claude started, which a live pane claims
  already: an agent that writes it itself gains nothing, and lever only compares it with what it
  reads itself.
- **Background sessions are off in every agent, at start.** At every start, `lever-agent boot`
  writes `env.CLAUDE_CODE_DISABLE_AGENT_VIEW=1` and `"disableAgentView": true` into the
  managed-settings file (above), whatever the agent's `claude_settings`; it is not an option.
  The switch guards against accidents, not against the agent: the agent owns `/etc/claude-code`
  in its container and can change the switch during a session. lever rewrites it at every start.
  Doctor's `claude settings` row shows `agent view: off`, or warns with `agent view: not set`
  (background sessions are then possible, for example on an image without a writable
  `/etc/claude-code`: check that directory's permissions or rebuild the image). Claude Code documents
  them as equivalent: they turn off background agents and agent view (`claude agents`, `--bg`,
  `/background`, and the on-demand supervisor). In the Claude Code build lever bakes (2.1.296) the
  left-arrow shortcut that backgrounds the session and opens agent view is gated on the same
  switch. This is read from that build, **not yet tested in a running container**; doctor's
  `claude background sessions` row is the check that matters.
- **Why.** A session moved into the background (`/background`, `/bg`, or the left-arrow key)
  leaves session records in the agent home (`~/.claude/sessions/<pid>.json` with `"kind":"bg"`,
  the interactive record with a `"parkedJobId"`, plus `~/.claude/jobs/` and `~/.claude/daemon*`)
  that name the container it ran in. After the next container restart, scion starts Claude with
  `--continue`, and Claude refuses: "Your most recent conversation moved to a background session
  … registered from another machine or container, so this one can't tell whether it is still
  running", and exits. `lever apply` or `lever up` then reports the manager `came up, then died`,
  and adds the cause and the recovery to the error when it finds one. The container usually
  outlives the harness (tmux's shell window keeps it running), so lever first counts the stale
  background records in it, as doctor's row does. When the container is gone or holds none, it
  falls back to the container log, matching the refusal with terminal control codes and line
  wrapping removed. Whether the refusal reaches the container log at all is not yet tested;
  without the hint, doctor's row and the recovery below still apply.
- **Recovery.** In the agent home (`/home/scion` in the agent's container), move aside the
  `~/.claude/sessions/*.json` records that hold `"kind":"bg"` or a `"parkedJobId"`, and
  `~/.claude/jobs/` and `~/.claude/daemon*`. The conversation transcript is not touched and
  stays. Then `lever stop && lever up`. While the container runs, doctor's
  `claude background sessions` row counts such records left by another container: they will block
  the next `--continue`.
- **The left-arrow key.** Claude Code also has a per-user switch for the left-arrow shortcut
  (`leftArrowOpensAgents: false` in `~/.claude.json`). lever does not write it: that file is
  Claude Code's own state, which Claude rewrites while it runs, and lever does not manage it.
  The managed setting above covers the same shortcut.
- **`/login` does nothing useful, and leaks.** In subscription mode lever gives Claude its login
  as `CLAUDE_CODE_OAUTH_TOKEN`, which takes precedence over a stored login. `/login` in an agent
  writes `~/.claude/.credentials.json` into the jail, where agents can read it, and changes
  nothing. If someone ran it, delete that file.

## Stopping without powering off

`lever stop --keep-vm` is `lever stop` without the power-off. It stops the broker and the remote
proxy, suspends the manager and every running worker (the conversation is kept, as with a plain
stop), stops the jail's hub, and leaves the VM running. The next `lever up` finds the VM already
up (the *start VM* step reads *already running*), so it skips the VM boot, then starts the hub
and the broker and resumes the manager. Use it for a short break, or overnight when a fast
morning `lever up` matters more than the memory.

The trade-off is memory. A running VM keeps its memory on the host while nothing runs in it:
with Lima, the memory the guest has used stays allocated to the VM process, up to the
instance's configured `memory`, and lever does nothing to hand it back (with the `vz` driver
there is no balloon to return it). With OrbStack the machine stays running in OrbStack's shared
VM, which manages its own memory. On a laptop, prefer a plain `lever stop` when you need the RAM
or before a long time away; `lever stop` also powers off a VM that `--keep-vm` left running.

What stays the same: containment does not change (the VM keeps its egress firewall, and with the
broker stopped the jail reaches nothing of the host's), and a guest the host kills or restarts
meanwhile is recovered by the next `lever up` like any other
([recovery after a hard VM kill](#recovery-after-a-hard-vm-kill)).

`--keep-vm` fails (non-zero exit) when it cannot vouch that the agents are suspended: when the
jail does not answer (nothing was suspended; if the VM is up after all, `lever stop` powers it
off), or when a suspend did not take (the hub is still stopped and the VM left up, but that
agent's container goes on running in it until `lever stop`). Only the manager and the workers in
`lever.yaml` are suspended, as with a plain stop; any other agent container keeps running in the
VM. A plain `lever stop` after a `--keep-vm` finds the hub down: its suspend warning says the hub
did not answer and that the earlier `--keep-vm` already suspended the agents, and the VM powers
off as usual.

## Tree backups

Off by default. With `tree_backup: {enabled: true}` in `lever.yaml`, lever keeps snapshots of the
instance tree on the host, in `.lever-state/tree-backups/` (0700), out of the agents' reach. They
answer three questions: what did the tree look like before an agent damaged it, what changed
between two points in time, and how to copy that history off the host. Every byte, name, link and
timestamp in the tree was written by agents, so lever treats all of it as hostile: it never
follows a link in the tree, never opens anything but a regular file, never runs anything from it,
and records what it could not capture instead of failing.

### When snapshots are taken

| When | Trigger | Label |
|---|---|---|
| `lever stop`, after the VM is off | `stop` | `vm-off` |
| `lever destroy`, after the machine is deleted | `destroy` | `vm-off` |
| `lever stop --keep-vm`, after the agents are suspended, the hub stopped and the guest synced | `keep-vm` | `suspended` when every suspend took and the sync worked, else `none` |
| `lever tree snapshot` | `manual` | `vm-off` when the machine is stopped before and after the walk, else `none` |

lever asks the backend again after the walk: a `lever up` that started meanwhile turns `vm-off`
into `none`. A failed or incomplete snapshot at stop or destroy is a warning; it never changes
their exit status. `--no-tree-backup` skips it, `tree_backup.at_stop: false` turns it off, and
`--machine` never takes one. There is no resident timer: run `lever tree snapshot` from cron or
launchd for snapshots while the jail runs (exit `0` complete, `3` incomplete, `1` failed, `75`
another snapshot running, with `--no-wait`).

**The first snapshot** reads the whole tree, up to the `read` budget (8 GiB by default) within
the `time` budget (5 min at stop). Take it with `lever tree snapshot` while the jail is up, so the
first `lever stop` is fast. Later snapshots read only what changed: a file whose device, inode,
size, modification and change times all match the previous snapshot is not read again (the stat
cache). Each snapshot also re-reads the files verified longest ago, from the read budget left
over, so every file is re-read at least every `reverify` (7 days); `lever tree snapshot --full`
re-reads all of them.

### What is captured, and what is not

Captured: directories, regular files (content, all 12 mode bits, size, times, device, inode and
link count), symbolic links (their target, never followed), and the type and mode of FIFOs,
sockets and devices. Names are kept as raw bytes, so names that are not UTF-8 survive.
`.lever/` directories, `.lever-files/` and workers' `bootstrap.json` are captured like any file.

Not captured: the content of FIFOs, sockets and devices; anything inside a mount point in the
tree (recorded as `mount`, never entered); extended attributes, ACLs, resource forks, file flags
and birth times. Hard links are recorded through each name's device, inode and link count; their
content is read and stored once. One file is hashed but never stored: the manager's broker
ticket, `<tree>/.lever/bootstrap.json`, so a change to it shows as `M secret` without its content
in the store. That is by name only: a copy or a hard link of the ticket elsewhere in the tree is
stored like any other file.

Everything else in the tree is stored, **secrets included**: a leaked secret lives on in every
snapshot that holds it, and in every off-host copy. Rotating the secret is the fix.
`tree_backup.exclude` leaves paths out (a pattern with no `/` matches a name at any depth; one with
a `/` is anchored at the tree root, each component matched like a shell glob). An excluded path is
recorded with its metadata and never read. Every exclude is a place where agents can hide changes
from the record. In-tree `.gitignore` files have no effect.

### Incomplete snapshots, and what `?` means

Every limit in `tree_backup.limits` is a budget. Running out of one makes the snapshot
**incomplete**, never absent: lever commits what it has, marks what it did not capture, and says
so (`tree snapshot INCOMPLETE: truncated(entries) at docs (100000)` after `lever stop`, exit 3 from
`lever tree snapshot`, and `lever doctor`'s *tree backup* row). The paths the newest complete
snapshot held are looked up by name first, and their changed content is read before any new
file, so flooding the tree with new names cannot keep damage to existing files out of the record.
Each worker's directory and the rest of the tree get their own share of the budgets, so one worker
cannot starve the others. What the manager can still do is flood the tree root so that its own
*new* files are not captured in that snapshot; that is loud.

A snapshot never shows something it did not see as deleted. Below a directory that was truncated,
unreadable, could not be listed, changed while it was listed, or is a mount, `lever tree diff`
shows each name it no longer sees as `?` with the reason, not as `D`. Making a file or directory
unreadable (mode 000, a 0300 directory) shows as `?` "became unreadable"; the earlier content stays
in history. A snapshot is complete when the root was readable and nothing was truncated or
skipped; unreadable files, racy reads (`changed`, `unstable`) and excluded paths are counted and
shown but do not make it incomplete.

### Looking at history

- `lever tree list`: the snapshots in order, with trigger, label, complete or not, and pins.
- `lever tree log [-n N] [--all]`: one line per snapshot with what changed since the previous one.
- `lever tree log -- <path>`: the snapshots in which that path changed, with size, short hash and
  state: "which snapshot has the good version of X".
- `lever tree ls <id> [<path>] [-r]`: what a snapshot holds.
- `lever tree diff <a> [<b>] [-- <path>...]`: what changed between two snapshots (`<b>` defaults
  to the newest), summary first, then line diffs of text files. `--stat`, `--meta` (changes of
  modification time alone, as `~`), `--no-limit` (lift the 8 MiB output limit), `--full-diff`
  (line diffs even over 500 changed paths).
- `lever tree cat <id> <path>`: one file's content, verified against its hash, to stdout. It
  refuses a terminal: redirect it to a file or a pager.

A snapshot is named by its id, `latest`, or `latest~N` (N snapshots before the newest). Paths
print in a quoted form when they hold anything but plain printable ASCII: `"odd\xffname"`, with
every other byte as `\xNN`, so two different names never print the same and nothing an agent
wrote can drive your terminal. To pass such a path back, prefix the printed form with `q:`:
`lever tree cat latest 'q:"odd\xffname"'`. Text from files is shown with a two-character prefix;
a line that holds control, format or invisible characters is shown escaped, marked by `\` in the
second column. `lever tree diff` prints no row wider than 80 columns: a longer line goes on in rows
that start with `| `, so text an agent padded past the edge of the terminal cannot start a row and
pass for one of lever's own lines (`== M <path>`, `@@`). On a terminal narrower than 80 columns the
terminal wraps rows itself and that no longer holds.

### Restoring

```
lever tree restore <id> --to <dir> [--path <p>]... [--links=inside|all|none] [--keep-modes] [--keep-vcs]
```

lever never restores over the live tree. It creates `<dir>.lever-partial`, writes into it through
directories it has just created, and renames it to `<dir>` at the end; a restore that stops leaves
the `.lever-partial` directory. `<dir>` must not exist, and must not be inside the tree, the state
directory or the store. `--path` restores those subtrees (and their parent directories).

- Files are 0600 and directories 0700 unless `--keep-modes`, which applies the recorded permission
  bits but never setuid, setgid, sticky, group write or other write: on a shared host, a writable
  directory in the restore would let another local user plant files in it. `<dir>` itself stays
  0700. The report lists every mode bit it did not apply.
- Names that were hard links of one file in the snapshot are restored as hard links of one file,
  written once, so a file with a thousand names takes its size once on your disk, not a thousand
  times. Where the target refuses a hard link, the name gets its own copy. Runs of zeros are
  written as holes, as the store keeps them.
- **Links** (`--links`, default `inside`): only links whose target cannot leave the restored
  tree are created: a relative target of some leading `..` (no more than the link's depth below
  `<dir>`) followed by plain names. `all` creates every link as recorded (lever never follows
  them); `none` creates none.
- **Version control and code that runs on use:** restored files belong to you, and git, Mercurial
  and Jujutsu trust a repository's own settings because you own it; shell prompts and editors run
  them in any directory you enter. So lever renames, to `<name>.lever-restored`, every file through
  which one of them would run a command an agent chose:
  - git: `config`, `config.worktree`, `commondir` and `hooks` in any directory with an entry named
    `HEAD`, and every `.git` that is not a directory (a gitlink file, or a link);
  - Mercurial: `hgrc`, `hgrc-not-shared` and `sharedpath` in any `.hg` directory, and every `.hg`
    that is not a directory;
  - Jujutsu: `repo/config.toml` in any `.jj` directory, and every `.jj` or `.jj/repo` that is not a
    directory.

  Names are compared as a case-insensitive filesystem folds them. `--keep-vcs` turns this off. The
  rule covers tools that run unasked and trust by ownership. Not covered, and restored as they are:
  files a tool runs only when you ask it to (Makefiles, `package.json` scripts), files whose tool
  asks for your approval first (direnv's `.envrc`, mise), editor project settings (`.vscode/`,
  `.idea/`), which the editor's workspace trust gates (open the restored tree in restricted mode;
  a folder under one you already trust inherits that trust), and other version control systems
  (Subversion, Bazaar, Fossil, darcs).
- FIFOs, sockets, devices, the broker ticket and entries without content are not created.
- Two names that the target filesystem folds into one (by case on APFS) are a collision: lever
  skips the later one and its subtree and reports both. An entry that already exists and that no
  restored name folds to is reported as a foreign writer (Finder's `.DS_Store`).
- The report lists every collision, link and special file not created, entry without content,
  file that changed while it was read (`unstable`), name the target could not hold, hash failure,
  renamed git file and stripped mode bit. The exit status is 0 only when everything selected was
  restored.

**Copying back into the tree**, from a restore of snapshot `<id>` into `<dir>`:

1. `lever stop`, so that no agent writes while you copy.
2. `lever tree snapshot`, then `lever tree diff <id> latest -- <paths>`: look for `T` and `L`
   lines, on what you copy back and on every directory above it. Each is a file or directory an
   agent turned into a link, possibly to one of your own files outside the tree (`~/.zshrc`). Delete
   each such link from the tree (`rm`, which removes the link, not its target) before you copy.
3. Copy with `rsync -r --no-links --checksum <dir>/ <tree>/` (or only the paths you need). rsync
   replaces a link in the destination instead of writing through it. Never use `cp`, with or
   without `-P`: `-P` concerns links in the source, and `cp` writes through a link it finds in the
   destination, out of the tree. `--checksum` matters: the restore gives every file its recorded
   modification time, and rsync's default check skips a file whose size and time match, so a file
   an agent damaged but kept at the same size and time would stay damaged. If your rsync refuses
   either option (macOS 15 ships openrsync), install rsync proper (Homebrew's `rsync`) rather than
   leave one out.
4. `lever tree snapshot` again, and `lever tree diff <id> latest -- <paths>` must show no `M`, `T`,
   `L` or `D` line for what you copied back. A `P` line is a mode the restore did not keep (files
   are 0600 unless `--keep-modes`).

### Retention and garbage collection

lever keeps the `keep.last` (20) newest snapshots, the newest of each day for the last
`keep.daily` (7) days with snapshots, and the newest of each ISO week for the last `keep.weekly`
(8) weeks with snapshots, plus always the newest complete snapshot, the newest `vm-off` one, and
every snapshot taken with `lever tree snapshot --pin`.

**Store size.** The store holds every retained snapshot's content, and agents choose that content.
Three limits keep it from taking over the host's disk:

- Runs of zeros are stored as holes, so a sparse file costs the store what it costs the tree.
- `limits.new` (2 GiB) bounds the disk new content may take per snapshot, once any snapshot of
  the tree exists (each file is charged at least one 4 KiB block for its content and one for the
  file itself). Past it, new files are `skipped(new)` and the snapshot is incomplete; changed
  files the last complete snapshot held are stored first.
- `max_store` (32 GiB) caps the disk the retained snapshots take. Past it, GC deletes the oldest
  snapshots until the rest fit, except the newest, the newest complete, the newest `vm-off` and
  pinned ones, which stay even when they alone take more. This deletes history to make room: an
  agent that writes new content before every snapshot can push the oldest unpinned snapshots out
  (per snapshot, at most `limits.new` of content plus its manifest, up to `limits.manifest`), and
  doctor warns for a week after each such deletion. Pin
  the snapshots you rely on. Without the cap (`max_store: 0`) nothing is deleted for space, and
  the same agent can instead hold your filesystem at `min_free` for weeks, with every snapshot in
  that time `incomplete(disk)`.

When free space on the store's filesystem falls below `min_free` (2 GiB, plus 1% of the inodes),
new content is skipped (the snapshot is incomplete with reason `disk`) and the manifest still
commits.

**A full disk.** The tree and the store usually share a filesystem, so agents can fill it. The
manifest then goes into space the store holds back for it (`reserve`: 64 MiB, or twice the last
manifest, up to `limits.manifest`). Before the walk, lever caps the manifest at the space it is
sure to have when it commits (the reserve, plus the part of `min_free` that new content may not
take), so on a full disk the snapshot commits incomplete with `truncated(disk)` instead of failing.
A commit that fails anyway gives the reserve back. A commit that used the reserve makes a new one
once there is room; until then doctor says so, and another snapshot on the still-full disk may
fail. With `min_free: 0` every manifest is held to the reserve alone, even on an empty disk: the
first snapshot then stops at about 150 000 entries, and each later one at about twice the last. The status doctor reads is
written even on a full disk.

After each snapshot lever starts `lever tree gc` in the background (log in
`.lever-state/logs/tree-gc.log`). GC deletes expired snapshots and content no snapshot references.
It reads every snapshot while other snapshots may still run, and holds the store to itself only
for the deletions, so a `lever stop` during a long GC still takes its snapshot.
It validates every snapshot first, and when one is invalid it deletes nothing (doctor shows *GC
blocked*); `lever tree verify` names the problem and `lever tree delete <id>` removes that
snapshot, pinned or invalid. `lever tree verify` re-hashes every stored file; `--fix` moves damaged
ones to `blobs/corrupt/`, and the next snapshot of the same content stores it again.

### Off-host copies

lever does not talk to any cloud. `lever tree lock -- <cmd>` runs your copy tool while garbage
collection cannot change the store (snapshots still run). The command gets
`LEVER_TREE_BACKUP_STORE` and `LEVER_TREE_BACKUP_LATEST` in its environment, and a `PATH` without
empty, relative or in-tree entries; a command that resolves inside the tree is refused. It exits
with the command's status, or 75 when the lock is not free within `--wait` (10 min).

```
lever tree snapshot -q; lever tree lock -- sh -c 'rsync -a "$LEVER_TREE_BACKUP_STORE/snapshots" "$LEVER_TREE_BACKUP_STORE/blobs" backup:lever/'
lever tree snapshot -q; lever tree lock -- sh -c 'restic backup "$LEVER_TREE_BACKUP_STORE"'
```

- The store holds every retained snapshot and the secrets in them: use a tool that encrypts
  off-host (restic, borg, rclone crypt).
- Never pass paths from a manifest to a shell: they are agent-chosen.
- A snapshot committed during a copy can reach that copy without all its content if the tool
  copies `blobs/` before `snapshots/`; the next copy heals it, and the rsync recipe copies
  `snapshots/` first. `lever tree verify` on a recovered copy names any gap.
- Stop-time snapshots reach the copy at its next run.
- `lever tree path` prints the store's path.

### Limits of the host

- The store must be on a local POSIX filesystem: NFS, SMB and exFAT are not supported (locks and
  the 0700 owner check need it).
- On Linux, tree backups need kernel 5.8 or later with `openat2` allowed; doctor says so.
- On a network or FUSE tree, one blocked read can run past the time budget.
- A snapshot holds about twice `limits.manifest` in memory, plus 64 MiB: about 576 MiB at the
  default 256 MiB, whatever the agents put in the tree. Lower `limits.manifest` on a small host.
- **macOS privacy (TCC):** a tree under `~/Documents`, `~/Desktop` or iCloud Drive may be closed to
  the program that runs lever (a launchd job, for example). The snapshot then commits with the
  tree root unreadable and doctor's *tree backup* row fails. Move the instance out of those
  folders. Granting Full Disk Access to the program that runs lever also works, but gives
  everything run from that terminal or job access to all your files. Files that iCloud has evicted (`dataless`) are recorded without
  content rather than downloaded.
- The stat cache trusts the change time the host kernel sets when the share applies a write. If an
  agent could forge it, a changed file would be missed until it is re-verified, within `reverify`.

## Troubleshooting quick table

| Symptom | Likely cause | Do |
|---|---|---|
| Tool call denied `missing capability` | agent didn't mint/attach | the agent should follow its skill (`lever-operator` for the manager, `lever-agent` for a worker): mint via `lever-capability`, pass `_capability`; if the skill is missing, run `lever init` |
| Denied *with* a token attached | not granted, expired, or revoked | `tail .lever-state/broker.log` — the deny line names the reason; fix grants in `lever.yaml`, then `lever reload` |
| "unknown recipient" / new worker invisible | broker still running on the old config | `lever reload` |
| 502 on an external tool call | the host-side server isn't listening | `lever doctor` (`tool backends` row), start your server |
| `lever apply` fails: "the broker on 127.0.0.1:8444 belongs to another lever instance" | another instance on this host uses the same broker ports | give this instance its own `broker.jail_port` and `broker.admin_port`; see [two instances on one host](#two-instances-on-one-host) |
| `lever up` fails: "resolve go toolchain … exit status 126" | version-manager shim, no real Go on PATH | `export PATH="$HOME/.asdf/installs/golang/<ver>/go/bin:$PATH"` (doctor prints the exact line) |
| `lever apply`/`up` fails at `scion-server`: "hub not ready after …" | a hub start spends about 15 s before it serves (most of it a GCP metadata lookup that times out off GCP), and a cold start after a reboot or a scion pin change takes longer. lever waits up to 2 minutes and prints a line after 15 s; before 0.30.0 it gave up after 30 probes (about 30 s), and a retry seconds later found the hub up | run the command again; if it fails again, read `~/.scion/server.log` in the guest (`orb -m lever-<name>` or `limactl shell lever-<name>`). "no scion server is running in the jail any more" means the server exited during start-up: its log says why |
| `lever apply`/`up` fails at `start-manager`: "manager … stayed in phase \"created\" … and did not settle"; the guest's `~/.scion/server.log` shows `context canceled` on a start about every 30 s | the first rootless start of a large image (several GB) builds podman's user-namespace layer copy, which can take longer than scion's ~30 s start timeout; scion then deletes the partial work and starts again from zero, every time. lever 0.34.2 and later warms the copy of every image in the `load-image` step on every apply (fast once warm; a warning there means the warm-up failed) | in the guest, as the jail user: `podman run --rm --userns=keep-id:uid=1000,gid=1000 --entrypoint true <image>` (it can take a few minutes), then `lever up --fresh` — a manager that never left phase created has no conversation to lose |
| `lever up` fails: "manager … cannot be resumed: <dir> mounted by the manager record but no longer on the host" (before 0.30.0: podman `statfs /lever/<dir>: no such file or directory`) | the manager was created with `manager.read_only` naming `<dir>` (or a worker dir while the list was set); the record keeps that mount for life, and the directory was deleted after the entry was removed from `lever.yaml`. lever reads the mounts off the stopped manager container, or, with no container, off the hub record, and refuses every resume (the forced resume of an `error` phase too); it only warns when it keeps a running manager as it is | recreate the directory (`mkdir`, empty is enough) and run `lever up` again; to drop the mount, run `lever up --fresh --keep-conversation`. Keep a removed entry's directory until that fresh create |
| `lever up` exits **3**: "resume (or resume --force) of the manager failed. lever did NOT delete the manager …" | scion could not resume the manager record (a late runtime broker, a container whose state the VM lost, a wedged harness). lever keeps the record and the conversation; it never starts a fresh manager on its own after a failed resume | run `lever up` again — a transient failure clears; `lever doctor` (manager row) for the cause. To give the session up, run `lever up --fresh` (it backs the conversation up first; `--keep-conversation` resumes the new manager on it). In a script, test the exit code (3), not the text: the message ends with the hub's own words after "Cause:" |
| `lever up`/`apply` fails at `start-manager`: "did not come up … the harness is not live — cause: no conversation to continue" | the manager was never sent a message, so its home holds no transcript, and the resume's `claude --continue` found nothing to continue. `lever up` starts such a manager fresh on its own once it has checked the home from the guest, when the resume ran in its bring-up (on a terminal too: a resumed manager is attached only after claude reports its session start); this message means it could not prove the home empty (the line before it says why), the resume was `up`'s own (a suspended manager under a running hub, or after a guest restart), or the command was `lever apply` | `lever stop && lever up` (the resume then runs in the bring-up, which makes the fresh start when the line before did not give a reason), or `lever up --fresh`: it backs up any transcript first, so nothing is lost ([the manager's conversation across `--fresh`](#the-managers-conversation-across---fresh)) |
| `lever up` exits **4**: "the hub refused resume of the manager. lever kept the manager …" | the hub refused the call: a controller token without `agent:lifecycle`, or a phase that cannot be resumed now | `lever stop`, then `lever up` (re-mints the token), or wait and retry. Do **not** answer exit 4 with `--fresh` |
| Changed `manager.image` (or rebuilt under a new tag) but the manager still runs the old image; doctor's `manager image` row fails | a manager record keeps the image it was created with — `lever up` resumes it unchanged, and a `stop && up` is a resume too | `lever up --fresh` — the bring-up deletes the record once the hub is up and creates the manager on the configured image (add `--keep-conversation` to keep the conversation; plain `--fresh` starts a new one after backing the old one up; before 0.21 the flag was silently dropped after `stop`) |
| Doctor's `agent lever version` row fails | the agent image's in-jail code does not match the host `lever`: its `lever_jail_abi` label differs from the host's jail ABI (or, on an image without that label, its `lever_version` release differs). The tag names only the arch, so any `make lever-image` on the host (a throwaway instance, another checkout) replaces it. A release that changed no in-jail code keeps the jail ABI, so the row passes and shows the release difference as information | rebuild from the host lever's source: `make lever-image LEVER_IMAGE_FORCE=1`, then any instance image built `FROM` it; `lever apply`; `lever up --fresh` to recreate the manager on it (the conversation is discarded) |
| On `lima` with `egress: open`, every agent turn ends in `Request timed out` (about 4.5 min) while `lever up` looks fine; doctor's `guest DNS` row fails | the guest resolver is a nat DNAT to the host alias on a per-boot port, which the `LEVER_EGRESS` alias DROP swallowed (lever < 0.22.2, lever#34); in the guest, `sudo iptables -L LEVER_EGRESS -v -n` shows the `-d <alias> DROP` counter climbing with every lookup | upgrade lever and re-run `lever apply` — the open posture now ACCEPTs the live `LIMADNS` DNAT targets ahead of the alias DROP; `lever doctor`'s `guest DNS` row confirms a lookup completes |
| Doctor's `manager agent` row says the harness is `stalled` (or `crashed`/`offline`) over an `Up` container | the harness stopped completing turns: it cannot reach the model API (guest DNS, an outage), or its credential expired; the hub marks `stalled` after its `stalled_threshold` (default 5 min without an activity event) | `lever attach` shows the harness (a stuck call ends in `Request timed out`); check the `guest DNS` and credential rows; `lever stop` then `lever up` restarts the harness. If scion's `auto_suspend_stalled` is on, the hub suspends the manager itself and `lever up` resumes it into the same state until the cause is fixed |
| After the host slept, the manager's replies, status updates and heartbeats fail with `401 … token is expired` (agent.log: `AUTH_LOST`); doctor's `manager agent` or `agent hub tokens` row fails with "hub token expired" | scion's agent hub token lives 10 h and sciontool refreshes it 2 h before expiry with a timer that stands still while the host sleeps; an expired token cannot refresh itself. Doctor warns earlier, when a refresh is overdue | `lever apply` (or `lever up` on a running manager) runs `scion reset-auth` for every running or stopped agent whose token expired — no restart, the conversation is kept. The broker also checks every 5 min and resets a running agent's lapsed token itself (governed by `broker.auto_reenrol`; audit `op=hub-token`); see [agent session heals](#agent-session-heals). By hand, in the guest: `scion reset-auth <agent> -g <mount>` with the controller PAT |
| Doctor's `manager agent` row: "session ended in the hub (phase stopped), but claude still runs in its container"; `lever attach` refuses | another claude process in the manager's container — e.g. `podman exec … claude mcp list` — exited and fired the SessionEnd hook every claude there shares; sciontool reports any SessionEnd as phase stopped. The hub resumes a stopped record in a NEW claude session (only a suspended one gets `--continue`) | `lever apply`: it has the agent report its session running again, and the conversation continues; `lever stop` does the same before it suspends. Do not run `claude` in an agent container through `podman exec` |
| Doctor's `manager agent` row: "claude has exited (phase stopped)", or `lever up` logs that a stopped record resumes in a new claude session | the session really ended (or the record was stopped while the machine was down); scion's hub resumes a stopped record without `--continue` | `lever up`, then `lever attach` and `/resume` to pick the old conversation (it stays in the agent home) |
| `lever-manager agent resume <w>` returns 502 after `lever stop && lever up`; the audit detail ends in `statfs …/lever/tickets/<w>: no such file or directory` | the worker's ticket directory is a tmpfs under the run user's `XDG_RUNTIME_DIR` and the jail restart emptied it; before 0.22.4 the resume verb never re-staged a ticket (lever#36) | upgrade lever; on an older version, stage one through the admin route (`curl -X POST 127.0.0.1:8444/worker-ticket -d '{"worker":"<w>"}'`) and resume again |
| `lever apply`/`up` fails at `load-image`: "payload does not match any of the supported image formats … trying to reuse blob … readlink …/overlay/l/: no such file or directory", maybe after podman's "storage corruption might have occurred" | the jail's podman store holds a damaged layer left by an earlier failed or interrupted load, and podman picked it to reuse; the image archive is fine. Doctor's *podman storage* row counts the layers no image uses | back up the guest's `~/.scion`, then as the jail user `podman system reset --force`, then `lever up` without `--fresh`: see [recovering the jail's podman store](#recovering-the-jails-podman-store) |
| `lever up` reports the manager died and names a tmux server crash | the tmux server in the manager's container crashed (sciontool reaped it after a fault signal), taking claude's pane with it | `lever up` starts it again (the conversation is kept); in a Lima jail the guest keeps the core in `/var/lib/lever/cores` (see [tmux server crashes](#tmux-server-crashes)) |
| The attach view is frozen (or keeps an old terminal's size) while the agent works | a stale tmux client: an earlier attach whose terminal went away is still attached | `Ctrl-L`; else `Ctrl-b d` and `lever attach --detach-others`, which detaches the stale client (a plain `lever attach` leaves it attached; see [a frozen attach view](#a-frozen-attach-view)); `Esc` leaves Claude Code's background-agent view if a stray key opened it |
| Manager boots into a stale/odd state | suspect the tree, not the thread | see [security-model §5.1](/security-model/config-trust/) — `--fresh` resets the conversation, not the tree |
| Doctor nags `skipped-modified` about a SKILL.md / CLAUDE.md you customized on purpose | your edits aren't recorded as accepted | `lever init --adopt` — records them as your baseline (host-side); doctor then passes, and any change *past* that baseline still fails as "modified since adoption" (tamper signal preserved) |
| `lever up` printed `is up.` but the manager is dead moments later, or doctor's `manager agent` row fails | the harness died after scion reported the record running — a failed pre-start hook (`lever-agent boot`), an oversized boot task (see `prompt_file`), or `claude --continue` with no conversation to continue | after a start or resume, `lever up` waits until claude's tmux pane in the manager container is alive (up to 2 min: the pre-start hook runs first), then until claude reports that its session started (lever's `claude-ready` hook, up to 2 min more), and reports the manager up as soon as it has: a failed pre-start hook, an oversized task and a `--continue` with no conversation all end before claude starts a session, so before that report. A manager whose image has no `claude-ready` hook (built by an older lever) keeps the older check, with a note: a 3 s settle window counted from the pane, then a second look at the pane (10 s counted from the hub record when the pane cannot be probed). A claude whose pane stays alive but which never reports in the 2 min (a dialog that waits for a key before its session starts, a `/tmp` claude cannot write, hooks disabled in its settings) is not failed: lever falls back to the 3 s settle window and its second look at the pane and container, and if they pass reports the manager up with a warning that claude never reported its session start, so the readiness signal is missing and the settle window decided; `lever attach` shows what claude waits at. A death during that window still fails. `lever up` on a terminal does not wait for the report from a manager it has just created: it attaches as soon as the pane is alive. A resumed manager is attached only after the same wait as without a terminal, so a `claude --continue` that exits for want of a conversation is caught first. Either way `up` looks at the manager again after you detach, reporting `is not running after the attach ended` with what it saw (the bring-up record, and doctor's *last bring-up* row, still say the bring-up itself succeeded); `--no-attach` and `lever apply` always wait. It reports `came up, then died` when the container stops, the hub record leaves running, or the pane dies before the report; a claude that exits before it comes up (tmux answers in three probes in a row but claude's pane is not alive: scion's shell window keeps tmux and the container up) is `came up, then died: claude exited before it came up`, within a few seconds. It reports `did not come up` when the container runs but no tmux server answers in it after the wait (a hung pre-start hook). When the probe itself cannot run (it times out or fails a few times in a row), the container still running (podman's own state, not the hub's) after the window counts, with a warning. The container log in the guest holds the harness's last output. Fix the cause, then `lever up` again |
| `lever up` fails: "record says running, but its container is \"created\" — the harness died" after the jail VM was killed or crashed | nothing suspended the manager before the VM went down. lever now recognises the new guest boot (or, with no boot recorded, a container podman last started before the current boot) and suspends and resumes the record itself; this still shows when podman in the guest cannot say whether the container runs or when it started, or when the suspend fails | `lever stop`, then `lever up` (the conversation is kept). See [nested virtualization](#nested-virtualization) |
| `lever up` (or `apply`) warns `the manager's claude never reported its session start to lever` | claude's tmux pane stayed alive for 2 min, but lever's `claude-ready` hook never wrote its file: claude most likely waits at a dialog before its session starts (a trust, login or update prompt), cannot write `/tmp` in the container, or runs with hooks disabled in its settings. With no readiness signal lever used the 3 s settle window and a second look at the pane (or, when the pane cannot be read, the container) instead, and counted the manager up | `lever attach` shows what claude waits at; answer or fix it. Check that `/tmp` in the manager image is writable and that the agent's `claude_settings` do not disable hooks. A death during the window still fails the start |
| `lever up` (or `apply`) prints `start-manager: manager "<name>" still starting — …; 45s elapsed` | a manager start takes a while: the container start, the pre-start hook and claude's boot up to its session start can run over a minute | nothing: it prints a line about every 15 s with what it waits for, the hub's phase and container, and the time since the start. A line that repeats the same stage for minutes names where it is stuck |
| The manager start fails (`claude exited before it came up`, or `came up, then died`) and the error names a stale background session; or doctor's `claude background sessions` row warns | the conversation was moved into a Claude Code background session (`/background`, `/bg`, the left-arrow key) before the last restart; its session records name the old container, and `claude --continue` refuses | move the stale records aside in the agent home, then `lever stop && lever up`; see [background sessions and /login](#background-sessions-and-login) |
| Doctor fails "modified since adoption" | something changed a scaffold after you adopted it — possibly an agent (the tree is agent-writable) | review the diff first; if the change is yours, re-run `lever init --adopt`; if not, `lever init --force` restores framework content |

## Two instances on one host

Each instance is its own directory: its `lever.yaml`, its tree and its `.lever-state/`. Two
instances can run on one host at once, but they share the host's loopback ports, and `lever.yaml`
validation sees only its own file, so it cannot catch a clash between them. Give the second
instance:

- **its own `name`.** The jail VM is `lever-<name>`; two instances with one name share a VM.
- **its own broker ports**, `broker.jail_port` and `broker.admin_port` (defaults 8443 and 8444).
- **its own remote ports** when remote access is on, `remote.port` and `remote.login_port`
  (defaults 8445 and 8447); see [several remote-enabled instances](/remote-access/).
- **its own tool ports**: each supervised tool's `backend` port, and a whisper tool's
  `-whisper-port`.

```yaml
# instance B's lever.yaml
name: assistant-b
broker:
  jail_port: 8453
  admin_port: 8454
  # and each supervised tool's backend on a port instance A does not use
```

**Keep each instance's `manager.allow_ports` off the other's ports.** Validation refuses an
instance's own admin port and tool ports in its `allow_ports`, but not another instance's: listing
instance A's admin port in instance B's `allow_ports` hands B's agents A's unauthenticated admin
API.

**A broker is never shared.** Each instance has a random instance id, made on first use in
`.lever-state/instance-id.json`, and its broker reports it on `/epoch`. `lever apply`, `up` and
`reload` reuse or restart a running broker only when it reports this instance's id (or, for a broker
from an older lever, as below). When another instance's broker answers on the configured admin port,
they stop with an error that names the ports to change, and leave that broker running. `lever
revoke` and `lever broker bump-epoch` refuse it the same way, so a revoke never reaches the other
instance's agents. A broker started by a lever that predates the id reports none: apply restarts it
when this instance's `broker.pid` names a live process, then checks the port fell silent, and
otherwise refuses. Once it has started its broker, apply waits only for a broker that reports this
instance's id, this binary's version and this config's hash: any other broker that answers makes
apply fail at once, and it is never used. A copied instance directory makes its own id on its first
apply (the file records the directory it belongs to), so a copy and its original never pass for one
another; the copy's `broker.pid` and `remote.pid` name the original's processes, so its `lever
stop`, `reload` and `apply` remove them and never signal them. Moving an instance looks the same to
lever (a new id, the pid files dropped): stop it (`lever stop`) before you move it.

`lever doctor`'s `instance ports` row warns when a configured port is held by a process that is not
this instance's: another instance's broker on the broker ports, or any listener on a port whose
process of this instance (the broker, its tools, the remote proxy) is not running. It names the
holder when `lsof` can see it.

## Deploying an image as an archive

A host that only runs an instance does not need Docker. Build the agent image where Docker is
(`make lever-image`), save it, and ship the archive with the instance:

```sh
docker save -o images/lever-claude.tar scionlocal/lever-claude:arm64   # on the build machine
```

```yaml
manager:
  image: scionlocal/lever-claude          # the tag inside the archive (arch-resolved, as always)
  image_tar: images/lever-claude.tar      # instance root, outside the mounted tree
```

`lever apply` streams the archive straight into the jail's container runtime and never consults
the host Docker store; the "already loaded" skip compares the archive's config digest with the
image ID in the jail, so a re-apply with an unchanged archive streams nothing. `lever doctor`
reads the baked Claude Code version from the archive. A worker with no `image:` runs the manager
image and ships in the same archive; a worker with its own `image:` names its own `image_tar`.
One `docker save` of several tags writes shared layers once and loads every tag. Add the archive
to `.gitignore` when the instance root is a repository. Details:
[`image_tar`](/reference/config/#manager).

To upgrade the image, replace the archive and run `lever apply` (the digest changes, so it
streams), then `lever up --fresh` to recreate the manager onto it — a plain `up` (or `stop && up`)
resumes the record on the image it was created with.

## Recovering the jail's podman store

The agent images and containers live in the jail's rootless podman store. That store can be
damaged in a way that the image archive is not, and then every image load fails.

**Symptoms.** `lever apply` or `lever up` fails at `load-image`, and podman's error (lever prints
its last lines) says `payload does not match any of the supported image formats`, with a cause like
`trying to reuse blob … reading layer … creating file-getter: readlink …/overlay/l/: no such file or
directory`, often after podman's warning that storage corruption might have occurred. Doctor's
*podman storage* row fails on that warning, or warns about layers no image or container uses. A
re-run of the same command fails the same way: the archive is fine, the store is not.

**Why it happens.** podman writes an image's layers into the store one by one while it loads it,
and records the image only at the end. A load that fails or is interrupted (a broken stream, a
full disk, Ctrl-C, a timeout) leaves the layers it already wrote with no image: orphan layers.
`lever apply` prunes dangling *images* after each load, but neither that prune nor any other podman
command before 5.x removes a *layer* that no image owns, so the orphans pile up. When a later load
looks for an existing layer to reuse, podman searches all layers, orphans included, and if it picks
one whose write never finished (its overlay link is missing), the load fails, every time.

**Check.** `lever doctor`, row *podman storage*: the podman version, the counts of images,
containers and layers, how many layers nothing uses and their size, layers flagged incomplete, and
podman's own corruption warnings. lever changes nothing in the store (podman's own startup cleanup
may run while the row reads it), and lever never deletes a layer. podman's "storage corruption"
warning fails the row; hints such as "layer not known", which also appear in working stores, warn. A count
read while a load is running includes the layers that load has written so far.

**On podman 5.x** there is a narrower repair: in the guest as the jail user, `podman system check`
lists the damage, and `podman system check --repair` removes damaged images and layers that nothing
has used for more than a day (`--max` changes the age). Take the backup in step 1 first. On podman
4.x this command does not exist, and the reset below is the only safe repair: deleting files from
the store by hand corrupts it further.

### What a reset loses and what it keeps

`podman system reset --force` deletes the whole podman store of the user who runs it.

- **Lost:** every image in the jail (`lever up` loads the configured images again, from `image_tar`
  or the host Docker store, and warms them, which takes a few minutes for a large image); every
  container, the manager's and the workers', with anything written inside a container outside
  its mounts (packages an agent installed at run time, for example); podman volumes and networks
  in the jail.
- **Kept:** the hub database and the agent records (`~/.scion` in the guest), the agent homes,
  where each conversation lives (bind mounts, outside the podman store), the tree, and lever's
  state on the host. `lever up` re-applies lever's guest configuration either way.

### The runbook

1. **Back up first.** Open a shell in the guest as the jail user (`orb -m lever-<name>` or
   `limactl shell lever-<name>`). Do not use `lever stop` here: it powers the guest off, and the
   steps below need it running. Stop the hub so its database is not written during the copy
   (`scion server stop`), then the agent containers so no agent writes its home during the copy
   or is killed mid-write by the reset (`podman stop --all`; on a damaged store it may fail for
   some containers, which the reset then removes). Then copy the scion directory, which holds the hub database (`hub.db`) and
   the agent records, to a place outside the podman store:

   ```sh
   (umask 077; tar -C "$HOME" -czf "$HOME/scion-backup-$(date +%Y%m%d-%H%M%S).tar.gz" .scion)
   ```

   Make sure the agent homes are in a backup too. If a container still exists, this lists its
   bind mounts (and nothing else, so no environment value is printed):

   {% raw %}
   ```sh
   podman inspect --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{println}}{{end}}' <container>
   ```
   {% endraw %}

   Copy any home directory outside `~/.scion` the same way. Copy the archives off the guest if
   the guest itself is in doubt.
2. **Reset the store, as the jail user** (the same shell; not as root, whose store is a different
   one): `podman system reset --force`. If podman complains about `XDG_RUNTIME_DIR`, run
   `export XDG_RUNTIME_DIR=/run/user/$(id -u)` first.
3. **Bring the instance up again on the same records:** `lever up` on the host, **without
   `--fresh`**. The `load-image` step loads and warms the images again, and the manager record
   resumes with its conversation. `--fresh` would discard the conversation and is not needed.
4. `lever doctor`: the *podman storage* row should read `no orphan layers`.

If `lever up` then exits 3 (the resume failed), see the troubleshooting table; the record and the
conversation are still kept. To restore the backup, stop the hub, move the damaged `~/.scion`
aside and unpack the archive in `$HOME`.

## Nested virtualization

With [`nested_virt`](/reference/config/#top-level) the manager runs KVM guests inside the Lima
jail VM (to test lever itself, for example). That load runs the guest kernel's KVM code and the
host's nested SVM/VMX emulation, both less exercised than plain KVM. In one incident a Lima guest
(Ubuntu 24.04, kernel 6.8.0-146 GA) running a nested VM on an AMD host took a kernel page fault
on an idle CPU about 45 minutes after the nested VM started; the oops path faulted again (a
double fault on the stack guard page), then ran `kmsg_dump`, whose EFI pstore backend made an EFI
runtime call that hung in a spinlock. CPU after CPU soft-locked for about a day, until Lima
killed QEMU. lever answers that with three things: the guest recovers instead of hanging, a newer
guest kernel, and a `lever up` that resumes the manager after such a kill.

### Crash recovery in every Lima guest

Every `lever apply` and `lever up` converges these in every Lima guest, `nested_virt` or not
(an existing VM gets them on its next apply or up). `lever up` does it before it looks at the
manager, so it runs whether up then applies, resumes a suspended manager or only attaches to a
running one. They are on by default and have no switch:

- **Sysctls**, in `/etc/sysctl.d/99-lever-crash-recovery.conf` and applied to the running kernel:
  `kernel.panic_on_oops=1` (an oops panics instead of leaving a half-dead kernel),
  `kernel.softlockup_panic=1` (a CPU stuck in the kernel panics), `kernel.panic=10` (reboot
  10 s after a panic; the kernel's default waits forever). `kernel.hung_task_panic` stays `0` and
  is written so: a task blocked in D state for two minutes is what slow guest disk I/O looks like
  (the first podman layer copy of a large image, a stalled host mount), and panicking on it would
  reboot a healthy guest. A hung task does not wedge the kernel; the oops and soft-lockup paths
  cover the failure that does. A side effect: a soft lockup the host causes now reboots the
  guest instead of passing (see [soft lockups the host causes](#soft-lockups-the-host-causes)).
- **No EFI pstore.** The panic path runs `kmsg_dump` too, so with the EFI backend registered a
  panic could hang in the same EFI call. On Ubuntu's 6.8 (24.04) and 7.0 (26.04) kernels
  `efi_pstore` is a loadable module, loaded at boot and registered as pstore's backend. lever
  unloads it now when it can, writes
  `/etc/modprobe.d/lever-no-efi-pstore.conf` (`blacklist` plus `install efi_pstore /bin/true`) so
  nothing loads it again, and adds `efi_pstore.pstore_disable=1` to the kernel command line
  through `/etc/default/grub.d/99-lever-crash-recovery.cfg` and `update-grub`. The parameter is
  honoured by the module and by a built-in backend, also when the initramfs loads it. The cost:
  an oops is no longer saved in EFI variables for the next boot; read it on the serial console
  log instead (below).

The same step sets up the guest's capture of tmux server cores (see [tmux server
crashes](#tmux-server-crashes)); a failure there is a warning and does not stop the bring-up.

Each file is written only when its content differs, and `update-grub` runs only when the drop-in
changed or `grub.cfg` lacks the parameter. lever never reboots the guest.

**What a panic does to the VM.** The guest kernel asks for a reboot 10 s after a panic. What the
VM then does depends on Lima's driver: under **qemu** the guest reboots, unless Lima starts QEMU
with `-no-reboot` (then QEMU exits and the VM stops); under **vz** (Apple Virtualization.framework,
lever's default on macOS) a guest reboot usually stops the VM. Either way, `lever up` brings it
back (it starts a stopped VM) and resumes the manager (see
[recovery after a hard VM kill](#recovery-after-a-hard-vm-kill)). **Unverified as of this
release:** no panic has been forced on a lever-managed Lima VM under either driver, and lever does
not depend on Lima's source, so whether Lima passes `-no-reboot` to QEMU was not checked. Step 5 of
the [live test plan](https://github.com/stevegeek/lever/blob/main/CONTRIBUTING.md#live-test-plan-for-guest-hardening-and-nested-virt)
checks it.

**A guest booted without grub.** The command-line parameter reaches the kernel through grub, and
the pending-reboot check reads `/boot/vmlinuz`. A guest that boots a kernel directly (a Lima
template whose `images` entry sets `kernel:`, rather than the cloud image's grub) never gets the
parameter, so apply and doctor report "reboot pending" for it forever. lever's own template boots
the cloud image through grub; with a custom one, add `efi_pstore.pstore_disable=1` to that
kernel's command line yourself, or ignore the line.

#### Soft lockups the host causes

`kernel.softlockup_panic=1` panics on any soft lockup, including one the guest did not cause: a
vCPU that the host did not run for 20 s or more looks the same from inside. That happens under
heavy CPU steal on an overcommitted host, and can happen after the host sleeps or is suspended
with the VM running. The guest then panics and reboots (or stops), and every agent in it goes
down with it; `lever up` brings the manager back, workers are restarted as usual.

lever has no setting to turn it off, and an override file does not hold: every `lever apply` and
`lever up` rewrites `/etc/sysctl.d/99-lever-crash-recovery.conf` and sets the value in the running
kernel again. `sudo sysctl -w kernel.softlockup_panic=0` in the guest turns it off until the next
apply or up. If it bites repeatedly, give the VM fewer vCPUs than the host has free, keep the host
awake while agents run, and open an issue: a switch would be the fix.

**What needs a reboot:** the kernel command line (always, the first time), and the EFI pstore
backend when it could not be unloaded (built in, or in use). The sysctls and the unload take
effect at once. Apply prints `jail: reboot pending: …` for each; `lever stop`, then `lever up`,
reboots the VM and keeps the manager conversation. Doctor's *guest crash recovery* row reads
`/proc/sys/kernel/*`, `/proc/cmdline` and `/sys/module` and warns until all of it is in place.

On **OrbStack** none of this applies: an OrbStack machine shares OrbStack's one kernel with every
other machine and container on the Mac, `kernel.*` sysctls are not namespaced, and that kernel is
OrbStack's, not lever's. lever changes nothing there, and the doctor rows say not applicable.
`nested_virt` is Lima only anyway.

### The guest kernel

[`guest_kernel`](/reference/config/#top-level) picks the Lima guest's kernel: `hwe` installs the
guest release's hardware-enablement kernel, `ga` keeps the release kernel the cloud image ships.
On a 26.04 guest (every VM lever creates now) `hwe` means `linux-generic-hwe-26.04`, and `ga` the
7.0 release kernel; until Ubuntu moves 26.04's HWE series to a newer kernel the two are the same
7.0 series, and `hwe` only installs the newest 7.0 build. On a VM created on 24.04 `hwe` means
`linux-generic-hwe-24.04` (well past 6.8) and `ga` the 6.8 release kernel. On any other release
`hwe` installs nothing and warns. Unset, it is `hwe` with `nested_virt` and `ga` without: the 6.8
GA kernel is the one that crashed under nested load, and an instance that runs no nested VM has
no reason to change kernels under it. lever installs the package on the next apply or up and then
warns that a reboot is pending until the guest runs the new kernel (`lever stop`, then `lever up`).
The GA kernel stays installed; grub's *Advanced options* menu still offers it. lever counts the
package installed only when dpkg says `install ok installed`.

The install normally happens on the VM's first apply, before lever's egress chain exists, so it
reaches the Ubuntu mirrors under either posture. `apt-get update` and the install each wait up to
2 minutes for the apt and dpkg locks, which cloud-init or `apt-daily` may hold on a fresh VM.
Later, when the package is still missing:

- **Egress closed in the guest.** When lever's egress chain ends in the closed posture's
  catch-all `DROP`, lever does not run apt at all and says so in a warning.
- **A failed install is stamped.** When the install fails, lever writes
  `.lever-state/hwe-install-failed.json` (mode 0600) with the VM's `/etc/machine-id` and the
  time. While the stamp names the VM and the instance's posture is `egress: closed`, apply and up
  warn and do not run apt, so they do not wait out apt's timeouts each time. An apply under the
  open posture tries again and removes the stamp when the package is installed; deleting the
  stamp makes the next apply or up try again. A recreated VM has a new machine id, so an old
  stamp does not apply to it.

Doctor's *guest kernel* row shows the guest's Ubuntu release and `uname -r`, and warns when
`/boot/vmlinuz` names another kernel (a reboot is pending), when `guest_kernel` is `hwe` and the
release's package is missing or the release has none lever knows, and when `nested_virt` runs on a
6.8 kernel.

**Known host/guest kernel pairs under nested load.** Fill this in as pairs are tested (no host
names). *Guest kernel* is the Lima guest's, the kernel that runs the nested VMs; *host kernel* is
the kernel under the Lima guest's QEMU.

| Host kernel | Host CPU | Guest kernel | Load | Result |
|---|---|---|---|---|
| 7.2.x | AMD (`kvm_amd nested=1`) | 6.8.0-146 (24.04 GA) | a nested VM run by the manager, about 45 min | crashed: oops, double fault, EFI pstore hang, soft lockups until QEMU was killed |
| 7.0.0-38 (24.04 HWE), itself a KVM guest of an AMD host | AMD Zen 5 (`kvm_amd nested=1`) | 7.0.0-34 (26.04 GA, cloud image) | two 2-vCPU nested KVM guests busy on CPU and memory, the Lima guest busy on all 4 vCPUs and on disk with fsync; 5 min | clean: no oops, lockup, RCU stall or KVM error in the guest's kernel log or serial log |
| same | same | 7.0.0-38 (`linux-generic-hwe-26.04`) | same; 5 min | clean, as above |

The two 26.04 rows are short runs on a host that was itself a VM (three levels of KVM, one more
than a Lima jail on a physical host has): evidence that the 26.04 kernel runs nested guests, not
the 24 h soak that step 9 of the live test plan asks for.

### Recovery after a hard VM kill

A guest killed hard (a wedged kernel, QEMU killed, a host crash) and started again leaves the hub
record saying "running" over a container that never started in the new boot: nothing suspended
the manager. Earlier releases' `lever up` refused ("record says running, but its container is
\"created\" — the harness died; `lever stop` then `lever up` resumes it").

lever now records the guest's boot id (`/proc/sys/kernel/random/boot_id`, new on every boot) in
`.lever-state/guest-boot.json` each time it sees the manager live. When `lever apply` or
`lever up` finds the record "running", the boot id changed since, and the manager's container is
not running (podman's own answer in the guest; when podman cannot be asked, lever does nothing,
because the hub briefly reports `created` for a container that is starting), it logs
that the VM restarted, suspends the record and resumes it: what `lever stop` + `lever up` did by
hand. The conversation is kept; lever never uses `--fresh` for this.

When no boot id was recorded yet (the first bring-up after upgrading from a release before
0.35.0, or a lost `.lever-state/guest-boot.json`), podman's own start time stands in for it: lever
recovers the same way when podman says the manager's container is not running **and** that it last
started before the guest's current boot (`State.StartedAt` against `btime` in `/proc/stat`). A
container podman never started is not evidence. When the boot id or the start time cannot be read,
or the suspend does not take, `lever up` behaves as before and names `lever stop`, then
`lever up`.

### If a nested VM wedges the guest

1. With the crash recovery in place the guest should panic and reboot by itself within seconds of
   the oops or soft lockup. The hub and the manager do not come back by themselves: run
   `lever up`, which resumes the manager (above).
2. If the guest stays wedged (no answer from `limactl shell <vm> true`, QEMU at full CPU), stop
   it on the host: `limactl stop --force <vm>`, or kill its QEMU process. Then `lever up`.
3. Keep the evidence before it rotates. The guest console goes to the Lima instance directory on
   the host (`~/.lima/<vm>/serial*.log`), which holds the oops even when the guest could not
   write its disk. After the reboot, `journalctl -k -b -1` in the guest may have the previous
   boot's kernel log, but journald rarely flushes the oops itself before a panic: trust the
   serial log.
4. Check `lever doctor`: the *guest crash recovery* and *guest kernel* rows should be green. Add
   the host and guest kernels and the outcome to the table above.

### Breaking a reboot loop

If the guest oopses on every boot after `systemd-sysctl` has applied lever's file (a broken
kernel or module that faults early), `panic_on_oops` turns that into a reboot about every 10 s.
To stop it:

1. Stop the VM: `limactl stop --force <vm>`.
2. Start it with its console attached and interrupt grub (hold `Esc` or `Shift` as it boots; the
   cloud image hides the menu). Under qemu, Lima's instance directory has the serial console
   socket (`~/.lima/<vm>/serial*.sock`; the name depends on the Lima version), for example
   `socat -,raw,echo=0 unix-connect:<socket>`. Under vz, use the VM's console as your Lima version
   offers it.
3. Either pick the previous kernel under *Advanced options* (the GA kernel stays installed), or
   edit the `linux` line and add `sysctl.kernel.panic_on_oops=0`. `systemd-sysctl` applies
   `/etc/sysctl.d` after the kernel reads that parameter, so lever's file sets it back to 1; add
   `systemd.mask=systemd-sysctl.service` on the same line so the boot keeps 0.
4. Once the guest is up, fix the cause (remove the faulting module, pin the working kernel as
   grub's default) before the next `lever apply` or `lever up`: both set `panic_on_oops=1` again.

This path is unverified on Lima as of this release (the console steps vary by Lima version and
driver).

## Upgrading lever

1. Pull and rebuild: `cd lever_to && make all` (the host `lever`, `lever-tool-github`,
   `lever-tool-fizzy`, `lever-tool-whisper` and `lever-tool-tts`), and if the agent-side binaries changed, rebuild the agent image
   (`make lever-image`, or `make lever-image-bins` + your instance's own image build), then
   `lever apply` to load it (or re-save it over the `image_tar` archive on a Docker-less host,
   then `lever apply`). The manager runs a new image only after
   `lever up --fresh --keep-conversation`, which keeps its conversation. The agent-side binaries changed when the release's jail ABI did:
   `lever doctor`'s `agent lever version` row fails after step 3 if they did.
2. `lever init` — refreshes the scaffolded skills (your edited and adopted copies are left
   alone; `--check` to preview). If you've customized scaffolds and doctor nags about them, accept them once with
   `lever init --adopt`.
3. `lever reload` (or `lever stop && lever up` to also power-cycle the VM) — restart onto the new
   binaries/config.
4. `lever doctor` — every check green means the upgrade landed. The `agent lever version` row
   fails while the agent image's in-jail code is older than the host's (its `lever_jail_abi` label
   differs). When the new release changed no in-jail code, the row passes and names both
   releases: no image rebuild and no `lever up --fresh` is needed.

### Moving a Lima VM to Ubuntu 26.04

lever creates every new Lima VM from a pinned Ubuntu 26.04 LTS cloud image (one URL and sha256 per
architecture; Lima refuses an image whose digest does not match). Lima reads the image only when
it creates a VM, so upgrading lever does not change the release of an existing VM: a VM created
on Ubuntu 24.04 keeps running 24.04, and lever keeps provisioning it as before (the crash
recovery, `guest_kernel: hwe` with `linux-generic-hwe-24.04`, the doctor rows). Doctor's *guest
kernel* row names the guest's release and says when it is not the one new VMs get.

To move a VM to 26.04, recreate it:

1. `lever conversation backup` (the jail must be up).
2. `lever destroy`. It deletes the VM and everything in it: the hub's database, the agent homes,
   the podman store, and anything an agent left in the guest outside the tree. The tree and the
   instance's `.lever-state` are on the host and are not touched.
3. `lever up`. This is a first boot: it downloads the 26.04 image (about 0.9 GB, once per host),
   installs the guest packages, copies the agent image in again and starts the manager fresh,
   10-15 minutes in all.
4. `lever conversation restore`, which puts the backed-up conversation into the new manager.
   Workers start in the new VM with new conversations when the manager next starts them.
5. `lever doctor`.

Settings that apply at create time only (`disk`, `nested_virt`'s `/dev/kvm`) come from the
current config, so this is also the moment to change them.

**Never run `do-release-upgrade` in the guest.** lever does not support an in-place release
upgrade and never runs one. It swaps podman (4.9 to 5.7) under an existing rootless store, the
kernel, iptables, passt and the cloud image's services in one step, none of which lever's
provisioning, warm path or crash recovery has been checked against as an upgrade; under
`egress: closed` the guest cannot reach the mirrors and the upgrade stops halfway. The
recreate above is the tested path.

### Upgrading past 0.30.0: contacts, agent messages, files, push

- **The operator view is on with no new key.** With `remote.landing: chat` and at least one
  `tier: contact` login, an operator login's agent list gains a
  [Contacts section](/remote-access/#the-operators-view-of-contact-conversations) after the
  upgrade. The proxy reads a contact's conversation with that contact's own hub session, and only
  for a contact `lever apply` has bound to a hub user.
- **Agent messages and files need the new agent image.** `remote.agent_messages`,
  `remote.files` and `remote.push` are off by default. To turn on agent messages or files:
  `make lever-image` (and rebuild an instance image built from it), `lever init`, `lever apply`,
  then start the agents fresh: `lever up --fresh` for the manager (it backs the conversation up
  first; keeping it with `--keep-conversation` also keeps the skill it started with), purge and
  start for a worker. Push touches no agent: `lever apply` restarts the
  proxy with it.
- **Four more tools in every instance.** After an image rebuild, every instance's
  `lever-capability` server lists `contacts`, `contact_message`, `contact_files` and
  `share_file`. While their feature is off, `contact_message` and `share_file` refuse with `off`,
  and `contacts` and `contact_files` return an empty answer with a note.
- **Three new doctor rows:** `agent messages`, `files` and `push`. Each says `off` until you turn
  its feature on.
