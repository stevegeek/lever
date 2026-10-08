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
  `lever up --fresh`. A fresh start **discards the manager's conversation**: back it up first.
  Until then the manager's old binary has no `recycle` command.
- **The skill changes on an instance with contact logins.** Setting `recyclable: true` on the
  first worker and running `lever init` changes the manager's `lever-operator` skill. While the
  manager's session was started on the old skill, contacts cannot post to the manager; that ends
  only when the manager starts fresh, which loses its conversation unless you back it up. Make
  the config change, the image rebuild and the backup together, then run one `lever up --fresh`.

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
| `tool-logs/<tool>.log` | one file per supervised tool (its own stdout/stderr, not shared with the others) | a specific tool misbehaves (never registered, crashed, or returned bad output) and you need forensics without other tools' output in the way |
| `broker.pid` | the daemonized broker's pid | `lever doctor` reads it for the alive check |

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
| `remote-audit.jsonl` | one line per request the remote-access proxy allowed or denied (only with `remote:` configured) | **you** — unbounded |
| `broker.out.log`, `remote.log`, `tool-logs/<tool>.log` | process output of the broker, the remote proxy and each supervised tool | **you** — unbounded |
| `directives.log` | the operator-directive audit log (issued, delivered, consumed, revoked, and each denial) | lever — past 1 MiB it is renamed to `directives.log.1`, replacing the previous `.1` |
| `sent-ledger/` | the text of every message lever sent to an agent, one file per recipient and kind; `message_verify` reads it | lever — each file past 1 MiB moves to `<file>.1` |
| `chat-ledger/` | verified web-chat posts, one file per login (only with `remote:`) | lever — each file past 1 MiB moves to `<file>.1` |
| `chat-verified.jsonl` | which messages an agent has already verified (one use each) | lever — past 4 MiB it moves to `.1`, and only once the old `.1` is 48 h stale |
| `agent-ledger/` | the agent messages to contacts the broker authorized, one file per contact (only with `remote.agent_messages`) | lever — each file past 4 MiB moves to `<file>.1` |
| `files-ledger/` | every chat upload and share with its sha256, one file per agent (only with `remote.files`) | lever — each file past 8 MiB moves to `<file>.1`; a file shared in a dropped generation can no longer be downloaded |
| `sessions.jsonl` | each agent's last fresh session start | lever — past 1 MiB it moves to `.1` |

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
freshly created manager: change them, back up the manager's conversation, then run
`lever up --fresh`. `lever stop` + `lever up` and `lever reload` keep the old mounts; `lever apply`
warns.

- **`manager.read_only`** — the listed tree directories are read-only in the manager, and while
  the list is set every worker dir and the parents of each entry are pinned (bind-mounted over
  themselves). Keep host-run code (an operator CLI, scripts) under an entry. Edit a protected
  directory in place: replacing it on the host (`rm -rf` and recreate, a rename deploy, a
  `git checkout` that removes it) leaves the read-only mount on the old directory until the next
  fresh create. Removing an entry, or a worker while the list is set, also needs `--fresh`, and the
  directory must stay on the host until then (see the troubleshooting row for `cannot be
  resumed`). Doctor's `manager read-only paths` row probes each entry with a write test in the
  running manager. Details and limits:
  [`read_only`](/reference/config/#manager).
- **`nested_virt`** (Lima on an x86_64 Linux host) — gives the **manager** container `/dev/kvm`;
  workers and the hub do not get it. `lever apply` checks the host's KVM `nested` module and
  installs the guest udev rule (`/dev/kvm` mode 0666); a jail VM created without `/dev/kvm` needs a
  recreate (back up the conversation, `lever destroy`, `lever up`). Doctor's `nested virt` row
  checks the guest device, the manager's device, and that the old podman drop-in that gave every
  container the device is gone (`lever apply` removes it; workers created while it was there keep
  the device until they are recreated). Turned off, the row fails while the manager still holds
  the device. See [`nested_virt`](/reference/config/#top-level).

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
  Claude Code runs both sets. With nothing configured, boot touches nothing.
- **Changing them.** Edit the config and run `lever reload` (or `lever apply`): the broker
  restarts and the manager's envelope is staged again. A running agent keeps the values of its
  last start; the new ones apply at its next start (`lever stop && lever up` for the manager, a
  stop and a resume for a worker). That holds for a removed setting too: a running agent keeps
  what lever wrote until its next start. `lever apply` warns while the running manager differs,
  and doctor's `claude settings` row compares each running agent with the config.
- **What the agent can change.** The settings are a configuration aid, not a boundary. The
  managed-settings file is in the agent's own container and belongs to the `scion` user that runs
  Claude Code, so the agent can change it during a session; boot writes it again from your
  lever.yaml at the next start. The env value ranks above the agent's own settings files and
  `/autocompact`.
- **A failed delivery never stops the agent.** If boot cannot write the file (an image built
  before 0.34.1 has no `/etc/claude-code`) or the envelope's block is invalid, it logs
  `claude settings not delivered` and the agent starts without them; doctor's `claude settings`
  row shows what the agent got. The manager's
  envelope is in the tree it can write, so the manager can rewrite the copy staged for its own
  next start (`.lever/bootstrap.json`) and give itself another window or its own after-compaction
  note, within the same validated bounds; a broker restart (`lever reload`) stages a fresh one.
  This is no new power: the manager can already write a `CLAUDE.md` in its tree, which Claude
  Code reloads. Nothing in this path goes from the agent to the host: the host only reads back an integer
  and a checksum of the note for doctor and apply.
- **Images.** Delivery needs the new `lever-agent` and the new pre-start hook in the agent image.
  Run `make lever-image` (and rebuild any instance image built `FROM` it), then put each agent on
  a new container: the manager with `lever up --fresh` (back up its conversation first: it is
  discarded), a worker with `lever worker purge <name>` or a recycle. `lever stop && lever up`
  is not enough: it resumes the record on its old image. An older image ignores the block, and
  doctor's row shows the difference.

## Troubleshooting quick table

| Symptom | Likely cause | Do |
|---|---|---|
| Tool call denied `missing capability` | agent didn't mint/attach | the agent should follow its skill (`lever-operator` for the manager, `lever-agent` for a worker): mint via `lever-capability`, pass `_capability`; if the skill is missing, run `lever init` |
| Denied *with* a token attached | not granted, expired, or revoked | `tail .lever-state/broker.log` — the deny line names the reason; fix grants in `lever.yaml`, then `lever reload` |
| "unknown recipient" / new worker invisible | broker still running on the old config | `lever reload` |
| 502 on an external tool call | the host-side server isn't listening | `lever doctor` (`tool backends` row), start your server |
| `lever up` fails: "resolve go toolchain … exit status 126" | version-manager shim, no real Go on PATH | `export PATH="$HOME/.asdf/installs/golang/<ver>/go/bin:$PATH"` (doctor prints the exact line) |
| `lever apply`/`up` fails at `scion-server`: "hub not ready after …" | a hub start spends about 15 s before it serves (most of it a GCP metadata lookup that times out off GCP), and a cold start after a reboot or a scion pin change takes longer. lever waits up to 2 minutes and prints a line after 15 s; before 0.30.0 it gave up after 30 probes (about 30 s), and a retry seconds later found the hub up | run the command again; if it fails again, read `~/.scion/server.log` in the guest (`orb -m lever-<name>` or `limactl shell lever-<name>`). "no scion server is running in the jail any more" means the server exited during start-up: its log says why |
| `lever up` fails: "manager … cannot be resumed: <dir> mounted by the manager record but no longer on the host" (before 0.30.0: podman `statfs /lever/<dir>: no such file or directory`) | the manager was created with `manager.read_only` naming `<dir>` (or a worker dir while the list was set); the record keeps that mount for life, and the directory was deleted after the entry was removed from `lever.yaml`. lever reads the mounts off the stopped manager container, or, with no container, off the hub record, and refuses every resume (the forced resume of an `error` phase too); it only warns when it keeps a running manager as it is | recreate the directory (`mkdir`, empty is enough) and run `lever up` again; to drop the mount, back up the conversation and run `lever up --fresh`. Keep a removed entry's directory until that fresh create |
| `lever up` exits **3**: "resume (or resume --force) of the manager failed. lever did NOT delete the manager …" | scion could not resume the manager record (a late runtime broker, a container whose state the VM lost, a wedged harness). lever keeps the record and the conversation; it never starts a fresh manager on its own | run `lever up` again — a transient failure clears; `lever doctor` (manager row) for the cause. To give the session up, back up the conversation if you want it, then `lever up --fresh`. In a script, test the exit code (3), not the text: the message ends with the hub's own words after "Cause:" |
| `lever up` exits **4**: "the hub refused resume of the manager. lever kept the manager …" | the hub refused the call: a controller token without `agent:lifecycle`, or a phase that cannot be resumed now | `lever stop`, then `lever up` (re-mints the token), or wait and retry. Do **not** answer exit 4 with `--fresh` |
| Changed `manager.image` (or rebuilt under a new tag) but the manager still runs the old image; doctor's `manager image` row fails | a manager record keeps the image it was created with — `lever up` resumes it unchanged, and a `stop && up` is a resume too | `lever up --fresh` — the bring-up deletes the record once the hub is up and creates the manager on the configured image (the conversation is discarded; before 0.21 the flag was silently dropped after `stop`) |
| Doctor's `agent lever version` row fails | the agent image was built from another lever release than the host `lever` — the tag names only the arch, so any `make lever-image` on the host (a throwaway instance, another checkout) replaces it | rebuild from the host lever's source: `make lever-image LEVER_IMAGE_FORCE=1`, then any instance image built `FROM` it; `lever apply`; `lever up --fresh` to recreate the manager on it (the conversation is discarded) |
| On `lima` with `egress: open`, every agent turn ends in `Request timed out` (about 4.5 min) while `lever up` looks fine; doctor's `guest DNS` row fails | the guest resolver is a nat DNAT to the host alias on a per-boot port, which the `LEVER_EGRESS` alias DROP swallowed (lever < 0.22.2, lever#34); in the guest, `sudo iptables -L LEVER_EGRESS -v -n` shows the `-d <alias> DROP` counter climbing with every lookup | upgrade lever and re-run `lever apply` — the open posture now ACCEPTs the live `LIMADNS` DNAT targets ahead of the alias DROP; `lever doctor`'s `guest DNS` row confirms a lookup completes |
| Doctor's `manager agent` row says the harness is `stalled` (or `crashed`/`offline`) over an `Up` container | the harness stopped completing turns: it cannot reach the model API (guest DNS, an outage), or its credential expired; the hub marks `stalled` after its `stalled_threshold` (default 5 min without an activity event) | `lever attach` shows the harness (a stuck call ends in `Request timed out`); check the `guest DNS` and credential rows; `lever stop` then `lever up` restarts the harness. If scion's `auto_suspend_stalled` is on, the hub suspends the manager itself and `lever up` resumes it into the same state until the cause is fixed |
| After the host slept, the manager's replies, status updates and heartbeats fail with `401 … token is expired` (agent.log: `AUTH_LOST`); doctor's `manager agent` or `agent hub tokens` row fails with "hub token expired" | scion's agent hub token lives 10 h and sciontool refreshes it 2 h before expiry with a timer that stands still while the host sleeps; an expired token cannot refresh itself. Doctor warns earlier, when a refresh is overdue | `lever apply` (or `lever up` on a running manager) runs `scion reset-auth` for every running or stopped agent whose token expired — no restart, the conversation is kept. The broker also checks every 5 min and resets a running agent's lapsed token itself (governed by `broker.auto_reenrol`; audit `op=hub-token`); see [agent session heals](#agent-session-heals). By hand, in the guest: `scion reset-auth <agent> -g <mount>` with the controller PAT |
| Doctor's `manager agent` row: "session ended in the hub (phase stopped), but claude still runs in its container"; `lever attach` refuses | another claude process in the manager's container — e.g. `podman exec … claude mcp list` — exited and fired the SessionEnd hook every claude there shares; sciontool reports any SessionEnd as phase stopped. The hub resumes a stopped record in a NEW claude session (only a suspended one gets `--continue`) | `lever apply`: it has the agent report its session running again, and the conversation continues; `lever stop` does the same before it suspends. Do not run `claude` in an agent container through `podman exec` |
| Doctor's `manager agent` row: "claude has exited (phase stopped)", or `lever up` logs that a stopped record resumes in a new claude session | the session really ended (or the record was stopped while the machine was down); scion's hub resumes a stopped record without `--continue` | `lever up`, then `lever attach` and `/resume` to pick the old conversation (it stays in the agent home) |
| `lever-manager agent resume <w>` returns 502 after `lever stop && lever up`; the audit detail ends in `statfs …/lever/tickets/<w>: no such file or directory` | the worker's ticket directory is a tmpfs under the run user's `XDG_RUNTIME_DIR` and the jail restart emptied it; before 0.22.4 the resume verb never re-staged a ticket (lever#36) | upgrade lever; on an older version, stage one through the admin route (`curl -X POST 127.0.0.1:8444/worker-ticket -d '{"worker":"<w>"}'`) and resume again |
| Manager boots into a stale/odd state | suspect the tree, not the thread | see [security-model §5.1](/security-model/config-trust/) — `--fresh` resets the conversation, not the tree |
| Doctor nags `skipped-modified` about a SKILL.md / CLAUDE.md you customized on purpose | your edits aren't recorded as accepted | `lever init --adopt` — records them as your baseline (host-side); doctor then passes, and any change *past* that baseline still fails as "modified since adoption" (tamper signal preserved) |
| `lever up` printed `is up.` but the manager is dead moments later, or doctor's `manager agent` row fails | the harness died after scion reported the record running — an oversized boot task (see `prompt_file`), or `claude --continue` with no conversation to continue | `lever up` holds the record for a 10 s settle window and reports `came up, then died` with the phase and container it saw; the container log in the guest holds the harness's last output. Fix the cause, then `lever up` again |
| Doctor fails "modified since adoption" | something changed a scaffold after you adopted it — possibly an agent (the tree is agent-writable) | review the diff first; if the change is yours, re-run `lever init --adopt`; if not, `lever init --force` restores framework content |

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

## Upgrading lever

1. Pull and rebuild: `cd lever_to && make all` (the host `lever`, `lever-tool-github` and
   `lever-tool-fizzy`), and if the agent-side binaries changed, rebuild the agent image
   (`make lever-image`, or `make lever-image-bins` + your instance's own image build), then
   `lever apply` to load it (or re-save it over the `image_tar` archive on a Docker-less host,
   then `lever apply`). The manager runs a new image only after `lever up --fresh` (back up its
   conversation first).
2. `lever init` — refreshes the scaffolded skills (your edited and adopted copies are left
   alone; `--check` to preview). If you've customized scaffolds and doctor nags about them, accept them once with
   `lever init --adopt`.
3. `lever reload` (or `lever stop && lever up` to also power-cycle the VM) — restart onto the new
   binaries/config.
4. `lever doctor` — every check green means the upgrade landed. The `agent lever version` row
   fails while the agent image still carries the previous release's binaries.

### Upgrading past 0.30.0: contacts, agent messages, files, push

- **The operator view is on with no new key.** With `remote.landing: chat` and at least one
  `tier: contact` login, an operator login's agent list gains a
  [Contacts section](/remote-access/#the-operators-view-of-contact-conversations) after the
  upgrade. The proxy reads a contact's conversation with that contact's own hub session, and only
  for a contact `lever apply` has bound to a hub user.
- **Agent messages and files need the new agent image.** `remote.agent_messages`,
  `remote.files` and `remote.push` are off by default. To turn on agent messages or files:
  `make lever-image` (and rebuild an instance image built from it), `lever init`, `lever apply`,
  then start the agents fresh: `lever up --fresh` for the manager after you back up its
  conversation, purge and start for a worker. Push touches no agent: `lever apply` restarts the
  proxy with it.
- **Four more tools in every instance.** After an image rebuild, every instance's
  `lever-capability` server lists `contacts`, `contact_message`, `contact_files` and
  `share_file`. While their feature is off, `contact_message` and `share_file` refuse with `off`,
  and `contacts` and `contact_files` return an empty answer with a note.
- **Three new doctor rows:** `agent messages`, `files` and `push`. Each says `off` until you turn
  its feature on.
