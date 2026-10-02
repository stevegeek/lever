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
5. Verify: `lever doctor` (skills check covers the new dir), then dispatch it from the manager
   (`lever-manager agent start newworker --task "…"`) or message it from the host
   (`lever msg send "…" --to newworker`).

scion caps how many agents run at once on one runtime broker (a lever instance has one): **12**,
the manager included, on scion `cf16e4b0` (2026-09) and later. A create or start beyond the cap is refused.
Stop idle workers (`lever-manager agent stop <name>`) rather than keeping many running; the cap
itself is a hub limit only a hub admin can raise.

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
| `sessions.jsonl` | each agent's last fresh session start | lever — past 1 MiB it moves to `.1` |

The files lever bounds keep **one** previous generation: the current file plus its `.1`, so at most
about 2 MiB each (8 MiB for `chat-verified.jsonl`), and older lines are gone. That is enough for
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

## Troubleshooting quick table

| Symptom | Likely cause | Do |
|---|---|---|
| Tool call denied `missing capability` | agent didn't mint/attach | the agent should follow its skill (`lever-operator` for the manager, `lever-agent` for a worker): mint via `lever-capability`, pass `_capability`; if the skill is missing, run `lever init` |
| Denied *with* a token attached | not granted, expired, or revoked | `tail .lever-state/broker.log` — the deny line names the reason; fix grants in `lever.yaml`, then `lever reload` |
| "unknown recipient" / new worker invisible | broker still running on the old config | `lever reload` |
| 502 on an external tool call | the host-side server isn't listening | `lever doctor` (external-backends check), start your server |
| `lever up` fails: "resolve go toolchain … exit status 126" | version-manager shim, no real Go on PATH | `export PATH="$HOME/.asdf/installs/golang/<ver>/go/bin:$PATH"` (doctor prints the exact line) |
| Changed `manager.image` (or rebuilt under a new tag) but the manager still runs the old image; doctor's `manager image` row fails | a manager record keeps the image it was created with — `lever up` resumes it unchanged, and a `stop && up` is a resume too | `lever up --fresh` — the bring-up deletes the record once the hub is up and creates the manager on the configured image (the conversation is discarded; before 0.21 the flag was silently dropped after `stop`) |
| On `lima` with `egress: open`, every agent turn ends in `Request timed out` (about 4.5 min) while `lever up` looks fine; doctor's `guest DNS` row fails | the guest resolver is a nat DNAT to the host alias on a per-boot port, which the `LEVER_EGRESS` alias DROP swallowed (lever < 0.22.2, lever#34); in the guest, `sudo iptables -L LEVER_EGRESS -v -n` shows the `-d <alias> DROP` counter climbing with every lookup | upgrade lever and re-run `lever apply` — the open posture now ACCEPTs the live `LIMADNS` DNAT targets ahead of the alias DROP; `lever doctor`'s `guest DNS` row confirms a lookup completes |
| Doctor's `manager agent` row says the harness is `stalled` (or `crashed`/`offline`) over an `Up` container | the harness stopped completing turns: it cannot reach the model API (guest DNS, an outage), or its credential expired; the hub marks `stalled` after its `stalled_threshold` (default 5 min without an activity event) | `lever attach` shows the harness (a stuck call ends in `Request timed out`); check the `guest DNS` and credential rows; `lever stop` then `lever up` restarts the harness. If scion's `auto_suspend_stalled` is on, the hub suspends the manager itself and `lever up` resumes it into the same state until the cause is fixed |
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

1. Pull and rebuild: `cd lever_to && make all` (host binary), and if the agent-side binaries
   changed, `make lever-image-bins` + rebuild your agent image, then `lever apply` to load it
   (or re-save it over the `image_tar` archive on a Docker-less host, then `lever apply`).
2. `lever init` — refreshes the scaffolded skills (your edited and adopted copies are left
   alone; `--check` to preview). If you've customized scaffolds and doctor nags about them, accept them once with
   `lever init --adopt`.
3. `lever reload` (or `lever stop && lever up` to also power-cycle the VM) — restart onto the new
   binaries/config.
4. `lever doctor` — every check green means the upgrade landed.
