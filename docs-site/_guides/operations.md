---
title: Operations & recipes
nav_order: 9
---
# Operations & recipes

Task-shaped guides for running an instance day to day and growing it past the first
`lever up`. Each recipe is self-contained.

## Changing config on a running instance

The broker reads `lever.yaml` **once, at its own startup** — and a re-run of `lever up`/`apply`
deliberately keeps a healthy broker process running. So editing the config (new grove, new tool,
changed grants) and re-applying **silently changes nothing at the broker layer**: the symptom is a
correct-looking config plus unexplained denials ("unknown recipient", 403s for a grant you can see
in the file).

Use `lever reload`:

```sh
lever reload   # stops the broker, re-runs apply on the current config, spawns a fresh broker
```

It restarts the broker onto the edited config, re-registers groves, and re-applies egress — but
leaves the running manager container alone (apply's start-manager sees it already running), so the
**manager's conversation is preserved** and your TTY is not taken. It's the broker half of a
`lever stop` + `lever up` without the VM power cycle or the re-attach. CA, keys, and enrolments all
persist. (A full `lever stop && lever up` still works and additionally power-cycles the VM if you
want that.)

reload validates the edited config *before* it stops the broker, so a config typo fails with the
old broker still serving. If a later step fails (backend, image load), the broker can be briefly
down until you re-run `lever up` — the same recoverable window as a `stop`+`up` that fails midway.

> **reload adds and changes; it does not remove.** reload re-reads the config and re-registers the
> groves in it, but it does not tear down a grove you *deleted* from the config. That grove's
> container keeps running with its already-minted capability tokens (valid until `broker.grant_ttl`,
> default 24h) even though the new broker no longer knows it — new mints and messaging to it are
> denied, but held tokens still work. To actually cut off a removed grove, stop it and revoke it
> before or after the reload: `lever-manager agent stop <grove>` (from the manager) and
> `lever revoke <grove>`, or `lever broker bump-epoch` to kill every outstanding token at once.

## Adding a grove to a running instance

1. Create the directory under your tree: `mkdir -p workspace/groves/newgrove` (the dir must exist;
   lever won't create it for you).
2. Declare it in `lever.yaml`:
   ```yaml
   groves:
     - name: newgrove
       dir: groves/newgrove
   ```
   Add `obtain:` grants if it needs brokered tools (see [capabilities](/capabilities/)).
3. `lever init` — scaffolds the grove's `lever-agent` skill into the new dir.
4. `lever reload` — the broker must restart to learn the new grove (see above).
5. Verify: `lever doctor` (skills check covers the new dir), then dispatch it from the manager
   (`lever-manager agent start newgrove --task "…"`) or message it from the host
   (`lever msg send "…" --to newgrove`).

## Where the logs are

All host-side state lives in `.lever-state/` at the instance root:

| File | What's in it | When to read it |
|---|---|---|
| `broker.log` | every capability decision — `allow`/`deny` with caller, tool, op, and the deny reason. Mint allows are a ledger line: the token `id`, the matched policy `rule` (`obtain:…`/`delegate:…`), `exp`, `epoch`, and any baked `constraints`. Gateway and LLM lines carry the same `id`, so a mint correlates with every later use — and denied use — of that token: `grep id=<id> .lever-state/broker.log`. (On deny lines the id is the token's *claimed* id — the signature was not necessarily valid.) | **first stop for any 403**: it names the difference between "no token attached", "not granted", and "revoked" |
| `broker.out.log` | the broker process's own stderr (startup, proxy errors) | broker won't start, or gateway 502s (backend refused) |
| `broker.pid` | the daemonized broker's pid | `lever doctor` reads it for the alive check |

Agent-side, the source of truth is the session itself: `lever attach [name]` shows the full
scrollback including every incoming message and tool call. `lever doctor` should be your first
command whenever anything looks wrong — every check prints a specific fix hint.

## Troubleshooting quick table

| Symptom | Likely cause | Do |
|---|---|---|
| Tool call denied `missing capability` | agent didn't mint/attach | the agent should follow its `lever-operator` skill (mint via `lever-capability`, pass `_capability`); if the skill is missing, run `lever init` |
| Denied *with* a token attached | not granted, expired, or revoked | `tail .lever-state/broker.log` — the deny line names the reason; fix grants in `lever.yaml`, then stop+up |
| "unknown recipient" / new grove invisible | broker still running on the old config | `lever reload` |
| Gateway 502 on an external tool | the host-side server isn't listening | `lever doctor` (external-backends check), start your server |
| `lever up` fails: "resolve go toolchain … exit status 126" | version-manager shim, no real Go on PATH | `export PATH="$HOME/.asdf/installs/golang/<ver>/go/bin:$PATH"` (doctor prints the exact line) |
| Manager boots into a stale/odd state | suspect the tree, not the thread | see [security-model §5.1](/security-model/) — `--fresh` resets the conversation, not the tree |

## Upgrading lever

1. Pull and rebuild: `cd lever_to && make all` (host binary), and if the agent-side binaries
   changed, `make lever-image-bins` + rebuild your agent image, then `lever apply` to load it.
2. `lever init` — refreshes the scaffolded skills (your edited copies are left alone; `--check`
   to preview). `CHANGELOG.md` in the repo notes anything that needs more than this.
3. `lever reload` (or `lever stop && lever up` to also power-cycle the VM) — restart onto the new
   binaries/config.
4. `lever doctor` — every check green means the upgrade landed.
