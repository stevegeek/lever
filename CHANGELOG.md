# Changelog

All notable changes to lever are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Process: every merge
to `main` that changes behavior adds an entry under `## [0.12.0] - 2026-07-31`; a
version bump moves the block under the new version heading.

## [0.15.1] - 2026-08-11

### Fixed
- **`lever attach` could dial a hub that no longer exists.** Minting the
  controller PAT starts a THROWAWAY dev-auth hub on its own port and `hub
  link`s the project against it, which persists that port into the jail's
  project config. The register-project step's re-init would overwrite it, but
  that path is deliberately skipped when the registration is already sound — so
  re-minting a PAT on an established instance (the documented recovery for a
  pre-0.14.0 PAT) left the project pointing at a dead port.

  It stayed invisible because every lever call passes the hub endpoint
  explicitly; `attach` is the one verb that **execs** scion instead, so it alone
  fell back to that file. Bring-up, `doctor` and `up --no-attach` all passed
  while `lever up` failed at the moment it handed over the terminal.

  Both halves are fixed: `attach` now pins `SCION_HUB_ENDPOINT` alongside the
  token, so it no longer depends on jail state lever does not own; and
  register-project repairs a stale endpoint on the skip path, so the config
  itself stops being wrong. A repair that cannot run fails the bring-up rather
  than leaving the project pointing at a dead hub.

  **If you hit this on 0.15.0**, upgrading fixes it on the next `lever apply`.

## [0.15.0] - 2026-08-11

### Upgrading

Replace the host binary, then run `lever init` to re-scaffold the operator
skills — `lever doctor` reports them stale until you do.

**If you are not moving your `scion` pin, that is the whole upgrade.** The
pre-role guard is silent on a Scion older than scion#1089, because there are no
role-derived scopes to widen there. `lever doctor`'s new **agent authorization
roles** line tells you where you stand on any pin, so check it before planning a
bump.

**Three changes can fail a bring-up that used to succeed:**

1. **`manager.credential_file` must now be `0600`-tight.** A `0640`/`0660` token
   was previously accepted; it now fails at apply. `chmod 600` it.

2. **Config validation is stricter.** Two workers whose `dir`s overlap (the same
   subtree, or one an ancestor of the other), and a worker named `manager`, are
   now rejected at load. Rename or re-scope them.

3. **A controller PAT minted before 0.14.0 lacks `project:update`**, so the
   scratchpad strip fails with `HTTP 403: Insufficient permissions`. Delete
   `.lever-state/controller.pat` and run **`lever apply`** — *not* `lever up`,
   whose phase probe deliberately refuses to guess on an authentication failure
   and exits before reaching the re-mint step. Apply re-mints the PAT with the
   current scope set.

**Crossing scion#1089 needs a decision first.** Every agent record created by an
older Scion stores no role, and this release refuses to resume one rather than
let scion#1102 resolve it to `full`. lever cannot repair the record: the role is
written on the create path only, is immutable after, `scion resume` takes no
`--role` flag, and the hub exposes no route to set one. So either delete each
agent and let lever recreate it with `--role baseline` — **losing its
conversation** — or stamp the role onto the stored records yourself, with the
hub stopped:

```sh
scion server stop                       # in the jail; apply restarts it
cp ~/.scion/hub.db ~/.scion/hub.db.bak  # keep a way back
python3 - <<'EOF'
import json, os, sqlite3
db = sqlite3.connect(os.path.expanduser("~/.scion/hub.db"))
for agent_id, slug, applied in list(db.execute("select id, slug, applied_config from agents")):
    config = json.loads(applied) if applied else {}
    if config.get("agentRole"):
        continue
    config["agentRole"] = "baseline"
    db.execute("update agents set applied_config = ? where id = ?", (json.dumps(config), agent_id))
    print(f"{slug}: stored no role -> baseline")
db.commit()
EOF
```

`baseline` is what lever stamps on a fresh create, so this only ever narrows
what a record resolves to — it cannot widen one, and a record that already
stores a role is left alone. Re-run `lever apply` afterwards and confirm with
`lever doctor` that every record now carries a role.

### Security
- **A pin bump can no longer promote an existing agent to full hub authority.**
  0.14.0 made lever stamp `--role baseline` on every agent it *starts*. That
  covers creation and nothing else: Scion writes the role into the agent record
  on the create path only, it is immutable afterwards, and `scion resume` takes
  no `--role` flag. An agent record created by a Scion older than scion#1089
  therefore stores **no role at all** — and scion#1102 resolves an unset stored
  role to `full` (agent create, agent lifecycle, project-secret-read), at
  dispatch and, since scion#1101, on every token refresh.

  Bumping a pin across that boundary silently promoted every pre-existing agent,
  the manager included. `lever apply` now reads the hub's record before keeping
  an agent and **fails the bring-up** when the record stores no role while the
  installed Scion understands roles. An already-`running` agent is not exempt:
  it refreshes its own token on the same rule.

  lever cannot repair the record — the hub exposes no route to set a stored role
  — so the refusal is the whole remedy. The ways out are to delete the agent and
  let lever recreate it with `--role baseline`, **losing its conversation**, or
  to stay on a pin older than scion#1089. The guard deliberately does not fall
  into apply's delete-and-recreate recovery: that recovery is what destroys the
  session, so taking it automatically would spend the very thing the operator is
  being protected into keeping. It fails closed on anything it cannot answer,
  including an unreadable record or an unanswerable capability probe.

- **`/msg/list` no longer hands every agent the fleet's notification feed.**
  `resolveListProject` returned the instance project for every caller and the
  underlying `scion notifications` call is scoped by the hub to the authenticated
  **user** — always the host controller PAT — so the project never scoped
  anything. Any worker read the manager's and every sibling's events, and the
  manager's documented `--worker <name>` selector was inert. lever now resolves
  whose inbox the caller may read to an agent *slug*, resolves that to the hub's
  agent id, and returns only the events the hub attributes to it. An event with
  no `agentId` is dropped, and an id lever cannot resolve fails the request:
  returning the raw feed is the leak this closes.

- **Bootstrap staging followed agent-planted symlinks.** `wire.Stage` wrote
  `bootstrap.json` with `MkdirAll`/`WriteFile`/`Chmod` — all of which resolve
  symlinks — into a directory inside the agent's own read-write mount, as the
  host operator. An agent that replaced its `.lever` with a symlink chose where
  the host wrote, and chmod'd 0600: the instance root's `lever.yaml`, the
  broker's `revocation.json` (which decodes as a zero `RevocationState`, undoing
  `lever revoke`), the controller PAT. A lapsed leaf alone reaches it through the
  auto-re-enrol healer, with no operator action at all. Staging now takes the
  tree as a confinement anchor, runs through `os.Root`, and refuses a symlink at
  the staging directory or at `bootstrap.json` even when it stays inside the
  tree.

- **A message body could not smuggle flags into `scion message`.** The body was
  passed as a bare positional and cobra parses flags interspersed with
  positionals, so a body of exactly `-b` bound to `--broadcast` and delivered to
  every agent in the project — past the worker-to-worker deny, which had checked
  only the recipient. `-a` widened it to every project on the hub. Flags now
  precede a `--` terminator.

- **The `--role` capability probe no longer memoises its answer.** It cached the
  verdict for the client's lifetime, justified by `ConfigHash` bouncing the
  broker on a scion change. That does not hold for `scion.source` and
  `scion.binary`: both are *paths*, so replacing the artifact behind one leaves
  the hash byte-identical and a long-lived broker keeps its verdict. A stale "no
  `--role`" disarmed the stamp and the new pre-role guard together, silently.

- **Overlapping worker `dir`s are rejected at config load.** Validation checked
  each worker's dir in isolation, so two workers could declare the same subtree,
  or one could declare an ancestor of another, and both passed. That voids
  sibling isolation for the pair: the outer worker reads and writes the inner
  one's whole workspace, including the fresh, unspent enrolment ticket the
  broker stages there on every resume — redeem it first and it enrols as the
  inner worker's CN, taking its capability grants. Comparison is component-wise
  on cleaned paths, so `workers/x` and `workers/xy` stay legal.

- **A worker may no longer be named `manager`.** The operator-directive channel
  resolves that literal name to the manager whatever `broker.manager_identity`
  is set to, so a worker holding it was silently shadowed and a directive aimed
  at the worker was delivered to the most privileged agent. The existing
  collision checks did not cover it, because a custom `manager_identity` moves
  the manager's CN off that word.

- **`manager.credential_file` must now be `0600`-tight.** It rejected only
  world-readable modes, so a `0640` OAuth token — the longest-lived,
  highest-value secret lever handles in subscription mode — was accepted, read,
  and projected into every agent container, while `lever doctor` was already
  failing the same file. It now rejects any group or other bit, matching
  `api_key_file`, the controller PAT and the staged bootstrap.

### Added
- **`lever doctor` reports agent records that store no role.** It reports them
  on *any* pin, so the bump above can be planned rather than discovered: on a
  Scion that predates roles the line passes with an explicit warning that a
  later pin will refuse these records, and on a roles-aware Scion it fails,
  naming each record. An unreachable hub stays "not checked"; a hub that
  answered unusably (403, unknown project) is a finding, matching the
  shared-directories check.

### Fixed
- **Docs: `scion.agent_role` was missing from the configuration reference.** The
  one key that overrides the role lever stamps was documented only in the
  security model.

- **Docs: two security claims the code did not make good on.** The
  worker-isolation guide said the controller PAT was "re-minted, not blindly
  reused" and that lever re-runs the mint window when the hub rejects it — no
  such path exists. `lever apply` short-circuits on any PAT already on disk,
  never validates it, and only `lever destroy` clears it; a rejected PAT is a
  hard bring-up failure the operator resolves. The credentials guide claimed the
  admin listener's loopback bind was "enforced twice" and cited
  `resolveAdminAddr`, a function deleted in this range; the live enforcement is
  a single fail-closed check in `Broker.ServeListeners`, which now reads as
  such so nobody adding a second bind path assumes a helper still guards it.

### Changed
- **Docs: upstream closed the hub-wide scratchpad gap.** scion#1103 answered
  lever's scion#1098 with a `project_defaults.default_scratchpad` key that works
  in file/SQLite mode, so the worker-isolation guide no longer claims the
  section has no `settings.yaml` key. The key acts at project creation time
  only, so lever's per-project removal stays. Two traps found while verifying it
  are recorded there: Scion reads the top-level `project_defaults` section only
  when the same `settings.yaml` also carries a `server:` section, and the commit
  carrying the key is not fetchable through the Go module proxy.

## [0.14.0] - 2026-08-10

### Security
- **Agents are pinned to the `baseline` role automatically (scion#1089/#1090).**
  Upstream replaced per-template scope lists with named role bundles, and then
  **flipped the default for an unspecified role from `baseline` to `full`**
  (scion `2181aff6`, #1090). On any pin at or after that commit, an agent
  started without an explicit role receives agent create, agent lifecycle and
  project-secret-read — breaking lever's core invariant that no agent holds hub
  authority.

  lever now decides by asking the scion **binary** whether it accepts
  `start --role`, not by reasoning about the pin — a commit hash says nothing
  about which side of #1090 it sits on. When roles exist, lever stamps
  `baseline`; when they do not (pre-#1089), it omits the flag, which widens
  nothing because the roles system is absent there. A probe that cannot answer
  fails the start rather than guessing, since guessing wrong grants full
  authority. The probe runs once per client, not once per dispatch.

  Baseline means heartbeat and self-token-refresh, with no agent create,
  lifecycle or secret scope — worker dispatch runs host-side under the
  controller PAT, never an agent's own token. Note it is wider than lever's
  previously documented three scopes: it also grants `project:read` and
  `agent:port:forward`.

### Added
- **`scion.binary` accepts an already-built scion binary (#27).** `scion.source`
  and `scion.version` both *compile* scion on the machine hosting the jail, on
  every bring-up, so that host needs a Go toolchain, a ~1.3 GB module cache and
  egress for module fetches. `scion.binary: ./dist/scion-linux-amd64` installs a
  binary built elsewhere and needs none of them. It is mutually exclusive with
  the other two, and is checked against the guest's architecture — via the ELF
  header, before anything is written into the jail — so a workstation arch
  mix-up fails with "binary is arm64, but the guest is amd64" rather than as
  `exec format error` at manager start. `scion.binary` and `scion.source` are
  rejected when they resolve **inside the mounted tree**: whatever they name is
  installed as root as the engine every agent runs under, and the tree is the
  one place agents can write.
- **`scion.agent_role` overrides the role lever picks.** Rarely needed — the
  default above is the safe one. Naming a role on a scion with no `--role` flag
  is a hard error rather than a silent downgrade, and an unknown value is
  rejected at config load rather than as an opaque bring-up failure.

### Changed
- **Scion `91c26b34` (163 commits) verified on pure upstream, but NOT shipped**
  — the pin is unfetchable, see above. Every patch lever previously carried on
  its scion fork is upstream by content at that commit, so the fork stops being
  necessary as soon as the module can be fetched again. When it can, the
  cutover needs a full `lever stop` + `lever up`, never a hub-binary swap: the
  hub applies one-way DB migrations on first boot, and agent tokens minted at
  the old pin 403 on newly scope-gated read endpoints until they refresh.
  Snapshot the hub DB first. The examples stay pinned at `68507153`.
- **scion is no longer re-streamed into the jail when it has not changed.** The
  binary is 158 MB and was piped in on every `lever up`, in all modes. lever now
  hashes the installed binary in the guest and skips the copy only when it
  matches — the file itself, not a record of what was installed once, so a
  binary that was deleted, truncated or replaced is reinstalled and `lever up`
  stays self-healing. Every failure mode (no such file, no `sha256sum`,
  unreadable) falls through to installing.
- **The broker restarts when the `scion` config block changes.** It holds a
  long-lived client that caches what the installed scion supports, and the
  reuse check previously ignored `scion:` entirely — so that cache could outlive
  the binary it described across a pin bump.

### Fixed
- **New projects no longer carry scion's cross-agent `scratchpad` shared dir
  (scion#925).** Upstream stamps a writable `scratchpad` on every new project
  and mounts it read-write into EVERY agent of that project — a channel between
  the manager and every worker, which defeats lever's subtree isolation. On
  lever's file/SQLite hub the server-side default cannot be turned off, so
  `lever apply` now strips the directory from the hub's project record after
  registration, on both the fresh and the already-registered path, then
  re-reads the record to confirm it is gone — the hub answers `404` for "no
  such shared dir", "no such project" and "no such route" alike, so the delete
  status alone cannot prove the removal happened. A failure fails apply rather
  than starting a fleet that silently shares a directory. The request runs
  inside the jail, like every other scion call: the hub binds the jail's
  loopback, and the Lima backend suppresses guest→host port forwarding by
  design. `lever doctor` gained a matching check. The controller PAT now also
  carries `project:update`, which the shared-dirs endpoint requires. The strip
  refuses to guess when a hub lists more than one project of the same name, and
  requires the `sharedDirs` field in the response — an absent field would
  otherwise decode as an empty list and pass the verify vacuously, which is the
  exact failure the verify exists to catch.
- **`go test ./...` no longer fork-bombs the machine.** Two broker tests
  re-exec'd `os.Args[0]` — the *test binary* under `go test` — as a detached
  (`Setsid`) `broker serve`, which outlived the run, accumulated across runs and
  re-spawned. A full suite left 724 stray processes behind.

## [0.13.0] - 2026-08-04

### Fixed
- **Directive tools no longer silently vanish from Claude Code sessions
  (#24).** `directive_consume` / `directive_check` advertised an input schema
  with a top-level `anyOf` combinator (added in the 0.9.1 both-spellings fix).
  The Anthropic API rejects a tool input schema carrying a top-level
  combinator, and Claude Code then drops the tool from the session with no
  error — so validly-signed, delivered operator directives could not be
  consumed, and the model's `request`/CLI fallback surfaced a misleading
  `policy: may not obtain/delegate` denial. The schema is now a plain object
  declaring both `id` and `directive_id`; the exactly-one-spelling rule was
  already enforced caller-side by `directiveID()` (JSON-RPC -32602 before any
  broker call), so nothing is lost. NOTE: the bug lives in the baked
  `lever-agent` binary — the fix reaches a running instance only via an agent
  **image rebuild + restart**, not a host lever upgrade.
- **Agent renewal no longer re-points Claude at the mTLS broker for LLM
  traffic.** The 12-hourly leaf renewal (`RefreshLLMToken`) wrote
  `ANTHROPIC_BASE_URL` from the broker URL while boot writes the loopback
  gateway URL — a drift introduced when the `aa63f9f` gateway leaf-hot-reload
  fix moved only boot. In api-key mode this re-introduced the 24h cached-leaf
  LLM outage that fix closed, at the first renewal after boot. Renew now pins
  the same loopback gateway URL as boot.

### Changed
- **Internal refactoring pass (no behavior change).** Executed the 2026-08-01
  whole-codebase refactoring plan: deduplicated the broker security-path
  handler preambles, unified the bootstrap wire envelope and agent HTTP
  helpers into single sources, split the oversized orchestration functions
  (`handleWorkerStart`, `start-manager`, `buildApplyDeps`, `brokerctl.Serve`),
  extracted a shared backend base for orbstack/lima, introduced typed
  constants for the agent-phase / step-kind / directive-kind / config-enum
  string vocabularies, and deleted vestigial exports. Test count rose
  (838 → 905); the full suite and `go vet` stay green, race-clean on the
  concurrency-sensitive packages. One deliberate, documented micro-change rode
  along: the `handleWorkerList` certless-caller audit line now carries the
  underlying error like its siblings.

## [0.12.0] - 2026-07-31

### Added
- **Broker-side auto-re-enrol after natural leaf lapse (#22).** An agent whose
  24h mTLS client leaf aged out while renewal couldn't run (host asleep,
  instance idle) previously stayed dead until a manual `lever up`. The
  broker's mTLS handshake verification now classifies that exact failure
  cryptographically — the presented cert is our CA's signature over a
  configured CN, wrong **only** in time — and a policy-gated healer re-stages
  a fresh one-use ticket and bounces the agent (suspend+resume; `resume
  --force` from the error phase), so boot re-enrols and the conversation is
  preserved. Gated by `broker.auto_reenrol: all | manager | off` (default
  `all`); revoked identities are NEVER healed (`lever revoke` stays a real
  kill-switch); attempts are audited (`op=reenrol`) and bounded (10 min
  per-CN cooldown, 3 per burst, burst resets on success or an hour of quiet).
  A leaf that is not-yet-valid (backward clock step) is rejected but never
  classified as a lapse — no automated bounce on clock skew.
- **Opt-in declared operation params reject typo'd mint constraints (#21).**
  An op may declare `params:` (its argument names) alongside `caveat_param`;
  when declared, `/request` rejects capability constraints on any other key
  AT MINT TIME with an accurate error — previously a mistyped key minted a
  strictly over-narrowed token that failed closed only at call time
  (`constraint not satisfied`), far from the mistake. Undeclared ops keep
  the permissive behavior; params come from config only (a self-registering
  tool cannot alter them).
- **doctor/init surface stale adopted-skill baselines (#16).** An adopted
  skill's `lever-version:` frontmatter stamp is treated as the owner's
  attestation of the framework baseline it was reviewed against: doctor now
  FAILS (never auto-overwrites) when it lags the current version (or is
  absent), naming both versions; `lever init` marks the line with `!`.
  Previously an adoption pinned an instance to its adoption-era baseline —
  through security-relevant skill changes — with doctor green throughout.

### Fixed
- **apply no longer discards an error-phase manager without trying to save
  it (#3).** `start-manager` now re-stages a ticket and attempts `scion
  resume --force` (scion#895) on a `phase=error` record BEFORE the loud
  delete+fresh fallback — the 2026-07-31 conversation loss (VM reboot →
  corrupted container state → resume failure → deletion) would have been a
  silent recovery. The loud fallback and its wording are unchanged when the
  forced resume also fails — but a failed resume now RE-OBSERVES the record
  first (with the broker-unavailable retry), so apply cannot delete a session
  the broker's healer concurrently recovered.
- **apply restarts the broker when its binary or tool config is stale
  (#19).** The M2 broker-reuse shortcut kept ANY serving broker, so a
  re-apply with a changed tool set — or a lever upgrade — left the old
  broker (old tools, old routes, no healer) silently running while apply
  reported success. `/epoch` now reports the broker's binary version + a
  digest of the broker-relevant config; apply restarts on mismatch (old
  brokers report neither — always restarted).
- **`up --fresh` discards a record in ANY phase.** Now that apply preserves
  an error-phase record whose forced resume comes up dead (see #3 above),
  `--fresh` is the escape hatch for a genuinely-bricked record — but it only
  triggered for running/suspended, so `up --fresh` resumed the very record
  the user asked to discard (caught in 0.12.0 live validation).
- **Worker resume re-stages a fresh enrolment ticket** (mirroring the
  manager's 0.10-era fix) and uses `resume --force` for error-phase records:
  a worker resumed after its ticket/leaf lifetime no longer wedges into
  `phase=error` on a spent ticket (live-hit 2026-07-31), and an
  already-wedged record heals on the next dispatch instead of requiring
  `lever worker purge`.

Requires `scion.version` >= `68507153` (already the 0.11.0 pin) for
`resume --force`.

## [0.11.0] - 2026-07-31

### Changed
- **Scion pin bumped `b4c9911d` → `68507153`** (82 upstream commits,
  2026-07-22 → 2026-07-31; examples and e2e-script defaults updated). A full
  review of the range found every lever-critical surface intact: relative
  `--workspace` subtree isolation (#815), `pre-start.d` script hooks and their
  ordering, `--dev-auth=false` + UAT token flow, `--map-host-loopback`
  per-agent netns, default agent-token scopes, and the `list --format json`
  fields lever parses. Instance-visible upstream deltas to know about:
  - **scion#894**: the hub now applies a default `--cpus 2` limit to agent
    containers with no explicit resource spec (`runtime.enforce_resource_defaults`,
    default on). Set it to `false` in the jail hub settings, or give agents an
    explicit resource spec, to keep previous unlimited behavior.
  - **scion#908**: agents with no model configured now get `ANTHROPIC_MODEL`
    resolved to `opus` — set `SCION_MODEL`/a model in the harness config if you
    want a different default.
  - **scion#847**: scion's *claude-harness base image* now ships a tool deny
    list (including `SendMessage`) in its baked `settings.json`. This arrives
    only when you rebuild the `scion-claude` base image, not with this pin
    bump — when you do, verify the deny list doesn't remove tools your agents'
    workflows need.
  - **scion#895**: upstream adds `scion resume --force` to recover agents
    stuck in the error phase — not yet used by lever's recovery paths
    (candidate follow-up).

## [0.10.1] - 2026-07-22

### Fixed
- The 0.10.0 release shipped with the `lever version` constant still reading
  `0.9.2` (binaries self-report `0.9.2 (v0.10.0)` — the commit suffix is
  correct, the semver is not). The release workflow now refuses to publish a
  tag whose version does not match `internal/cli/root.go`'s `Version` constant.

## [0.10.0] - 2026-07-22

### Changed
- **The Scion fork dependency for worker subtree isolation is gone.** Scion
  merged relative `--workspace` paths resolved against the project root with
  the same containment guard
  ([scion#815](https://github.com/GoogleCloudPlatform/scion/pull/815), merge
  commit `b4c9911d`), superseding the fork-only `--workspace-subdir` flag
  (fork branch `feat/per-agent-workspace-subpath`, upstream PR #699 — closed).
  Worker dispatch now emits `scion start --workspace workers/<name>` (relative
  form). Requires `scion.version` >= `b4c9911d`; the shipped example pins are
  bumped accordingly. On an older Scion, worker dispatch regresses to the old
  whole-tree-mount enrolment failure (fails closed: the worker 403s at
  enrolment) — bump the pin, or keep building from the fork until you do.
- The e2e test scripts' default `SCION_VERSION` bumped `666333f9` → `b4c9911d`
  ([#11](https://github.com/stevegeek/lever/issues/11)).

## [0.9.2] - 2026-07-22

### Fixed
- `delegate` silently minted a **self-bound** token when no recipient was named,
  and reported success. Both mint surfaces were affected: the capability MCP
  tool (`delegate` with `to` absent, blank, or given under the sibling spelling
  `bound_to`) and the in-jail CLI (`lever-agent delegate` with no `-to`). Each
  passed an empty bind target to the broker, which defaults an empty target to
  the caller (self-obtain), while on the MCP path an unrecognised key survived
  as a bogus narrowing constraint — so an agent could believe it had handed a
  capability to another agent when it had only minted one for itself, with
  nothing saying otherwise. Both surfaces now validate `tool`, `op` and the
  recipient from the caller's own arguments before any broker call (JSON-RPC
  `-32602` on the MCP path), and both reject naming *yourself* as the
  recipient — which hands nothing off and, because `MayObtainRule` treats
  requester == recipient as a self-obtain, succeeded even for an agent holding
  no delegate grant. `request`'s `bound_to` stays optional and still defaults to
  self. Not a privilege issue — `MayObtain` remains the authoritative gate — but
  the same class of silent argument bug as 0.9.1's directive fix. (#20)

  Compatibility note: the MCP tools now reserve **both** bind-target spellings,
  so a constraint key literally named `to` or `bound_to` is rejected with
  `-32602` instead of reaching the token. No shipped tool config uses either
  name. It matters because constraint keys are tool argument names, and `to` is
  a plausible one (mail, message, transfer) — but the two readings of `to` on
  `request` are indistinguishable from the caller's arguments alone, and a loud
  refusal is recoverable where a confidently wrong mint is not. A tool that
  needs such a constraint can rename it in `lever.yaml` via
  `caveat_param: {recipient: to}`. The CLI is unaffected: its flags and its
  positional `key=value` constraints occupy separate namespaces.

  Known limitation: on the MCP path only the sibling spelling is recognised as a
  mis-named recipient. Any other near-miss (`agent`, `To`, `recipient`) is still
  accepted as a narrowing constraint — the general case is indistinguishable
  from a legitimate constraint key. Such a token fails closed at call time
  rather than granting anything extra.

## [0.9.1] - 2026-07-22

### Fixed
- An agent could fail to consume a **valid** operator directive and conclude
  none existed. The `directive_consume`/`directive_check` MCP tools declared
  their argument as `id`, but every other spelling an agent sees is
  `directive_id` (the signed statement's field, `lever directive send`'s
  output, the docs), so a model that sent `directive_id` had its argument
  dropped, posted an empty id, and got the broker's deliberately opaque 404 —
  byte-identical to "unknown id / wrong target / already consumed / expired".
  Both spellings are now accepted and both are declared in the advertised
  schema. A missing, blank, over-long, non-string, or self-contradicting id
  now fails locally with JSON-RPC `-32602` before any broker call, so a
  caller-side mistake can never again masquerade as a directive verdict. The
  opaque-404 contract is untouched for everything genuinely about directive
  state — the new error is decided without any lookup, so it is not an oracle.
  The pointer notification now spells out the call form (`directive_consume`
  with `id="<the id>"`) instead of naming only the tool. **Requires an
  agent-image rebuild** (`make lever-image`, `LEVER_IMAGE_FORCE=1`) plus a
  container recreate (`lever stop && lever up`): `lever-agent` is baked into
  the image, so upgrading the host CLI alone does not fix a running instance.

## [0.9.0] - 2026-07-22

### Added
- Configurable Lima guest disk size (`disk:`, default 24GiB) — the guest disk
  was previously a hardcoded 100GiB sparse file, which could wedge a
  constrained host even though the sparse file itself grows lazily (#14).
- `lever doctor` reports the agent image's baked Claude Code version, read
  from a `claude_code_version` image label baked in at build time — no
  container run needed. A pre-label image reports informationally rather
  than failing the check (#6).
- Per-tool supervisor logs: each supervised tool (e.g. `lever-tool-db`) now
  writes its own `.lever-state/tool-logs/<tool>.log` instead of sharing one
  file, plus new `logrotate` guidance in the operations guide since none of
  `.lever-state/`'s logs rotate on their own (#10).
- `lever worker purge NAME`: host-side deletion of a worker's scion record
  and staged bootstrap ticket so it can be redispatched fresh with a new
  task. Never touches the worker's `HostWorkspace` (its work product);
  requires `--force` (#7).
- Prebuilt release binaries via GoReleaser, wired into a GitHub Actions
  release workflow; curated release notes are preserved rather than
  overwritten (#1).

### Fixed
- A supervised tool command that doesn't resolve on the supervisor's PATH is
  now rejected loudly at config-load instead of failing opaquely at spawn
  (or silently on a later re-apply); the same check also rejects an
  absolute command path that is a directory or non-executable. `lever
  doctor` now probes every configured tool backend — both external
  (dialed) and supervised (command-resolved), previously untested for the
  supervised case (#9).
- Broker denials on `/msg/send`, `/msg/list`, and `/request` now return the
  specific policy-deny reason in the HTTP body instead of a bare
  `forbidden`, and the CLI's broker client surfaces it in the returned
  error — so a denied agent can see *why* instead of guessing. Scion-runtime
  errors (as opposed to policy denies) are deliberately left opaque (#4a).
- Worker start/resume now polls the worker's own scion record for a
  `running` phase and a live container before reporting success, instead of
  trusting scion's start/resume call, which could report success for a
  harness that then dies moments later (#7).
- `lever stop`'s scion-suspend failure is no longer silently swallowed — it's
  now printed as a warning, so a recurrence of the Lima conversation-loss gap
  (#3) is diagnosable instead of invisible.

### Changed
- Dispatching an **existing** worker with a **new** task now fails with
  HTTP 409 instead of silently resuming the worker's original,
  creation-time task (scion pins a worker's task at creation and its
  `Resume` takes no task argument, so a new task was previously discarded
  without warning). To resume an existing worker, use `lever-manager agent
  resume NAME` (the `/worker/resume` path) — `agent start` cannot resume, as
  its `--task` defaults to a non-empty prompt so it always carries a task. To
  run a *new* task, run `lever worker purge NAME` first to start it fresh (#7).
  The `lever-operator` skill now teaches this vocabulary (resume an existing
  worker, `msg send` to give a running worker new work, `worker purge` is
  operator-only), so re-scaffold instances with `lever init` after upgrading.

### Docs
- Honest security-model note on the in-jail scion Hub API residual: the
  2026-07-06 create/lifecycle vector is closed *structurally* (the real hub
  always starts `--dev-auth=false`, hardcoded, no config knob enables it —
  so there's no runtime toggle for a `lever doctor` check to guard), but
  agent `DELETE` is not yet scope-checked by scion and remains callable by
  an agent's own token — tracked as a scion-side fix, not something lever's
  controller-PAT model can close from outside. Also corrects an egress
  overclaim: `egress: closed` still admits loopback for the in-machine hub,
  so this residual is identical under `open` or `closed` (#8).

## [0.8.1] - 2026-07-21

### Fixed
- Operator directives now reach agents on instances **upgraded** to 0.8.x, not
  only freshly-created ones. The directive generation that `target_agent` binds
  to was established only in the `/enrol` handler, but an agent that restarts with
  a persisted cert (or whose cert predates 0.8.0) refreshes via `/renew` and never
  re-hits `/enrol` — so its generation stayed `0` and `lever directive send`
  failed at resolve with `agent not yet enrolled`. `/renew` now establishes the
  generation at 1 when unset, without bumping an existing one (bumping stays
  reserved for genuine re-enrolment, so a 12 h cert refresh never invalidates an
  agent's own active directives).

## [0.8.0] - 2026-07-21

### Added
- **Operator directives** — an authenticated human-operator→agent channel. Until
  now every human→agent instruction (`lever msg`, relayed email/file text) arrived
  as an unauthenticated `sender:"user:…"` string, so a well-hardened agent
  correctly refused out-of-band instructions it could not attribute to the
  operator. Directives give the operator a channel an agent can treat as
  authoritative **without** creating a prompt-injection backdoor:
  - The operator signs a structured directive with an SSH key
    (`lever directive send <agent> --instruction … | --action …`); the host-side
    broker verifies it (`ssh-keygen -Y verify` against an `allowed_signers` file,
    exact-byte verify-then-parse, duplicate-key rejection) and delivers only a
    **pointer** (a directive id) into the agent's inbox — never the content.
  - The agent, if it independently decides to act, fetches the directive with a
    `directive_consume` MCP tool over its own mTLS channel. All cryptography is
    host-side; the model never checks a signature. A directive binds to the
    caller's mTLS identity **and** enrolment generation (`{cn, generation}`), so a
    recycled worker slug can never inherit a predecessor's directive and one agent
    cannot consume another's — consume is a single-use atomic compare-and-swap and
    every miss returns an identical opaque `not found`.
  - New host CLI: `lever directive send/list/revoke/selftest`, over a **0600
    unix-domain-socket** admin channel (unreachable from the VM); every admin op is
    operator-signed. New config block `operator:` (`allowed_signers`, `signing_key`,
    `directive_expiry` default 10 min, capped at 24 h). Directive state (active set,
    replay tombstones, per-CN generations) persists across broker restarts
    (`directives.json`, atomic writes, fail-closed). A `lever doctor` check and a
    `selftest` round-trip catch `allowed_signers` misconfiguration before it is
    needed. Depends on the per-agent netns isolation shipped in 0.7.0.
  - **Scope (honest):** Phase 1 delivers authenticated, integrity-protected,
    replay-proof **delivery and verification** of an operator-signed action bound to
    the target agent. It does **not** yet enforce the bound action at tool-call time:
    on consume the broker returns the validated action and the agent triggers the
    call through its ordinary, independently grant-checked capability path, so a
    directive grants **no new capability** — its value is that the request is
    provably from the operator. Host-enforced call-time binding (and the `approval`
    kind's operator-approval gate) is deferred to a later phase with its own review.
  - Bootstrap prompts (manager + worker) gain a directive carve-out: only an action
    returned by the agent's own `directive_consume` this turn carries operator
    authority; messages claiming or quoting a "verified" directive are data.
    Manager→worker authority does not launder (directives are signed for the worker
    directly). Existing instances show a scaffold-drift warning until re-scaffolded.

## [0.7.0] - 2026-07-21

### Changed
- Agents now run in their **own per-agent network namespace** (rootless podman's
  default pasta networking) instead of a shared jail netns (`--network=host`).
  Each container's `127.0.0.1` is now private, which closes a cross-agent
  escalation: the in-container agent gateway proxy (`127.0.0.1:8462`, holds the
  agent's mTLS leaf, trusts whoever reaches its loopback) was jail-wide under
  host networking, so a co-resident worker could `POST` the manager's gateway
  and be authenticated to the broker **as the manager** with no credential.
  Hub reachability across the netns boundary is preserved by a pasta
  `--map-host-loopback` option staged in the guest `containers.conf.d`
  (Scion's auto-computed container hub endpoint, `host.containers.internal:PORT`
  for podman, resolves to it); egress containment is unaffected because pasta's
  userspace egress still re-emerges on the jail `OUTPUT` chain (`LEVER_EGRESS`),
  not a bypassing `FORWARD` path. No Scion change required. Escape hatch:
  `LEVER_FORCE_HOST_NETWORK=1` on the host restores `--network=host` for
  debugging (reopens the shared-loopback gap; not isolation-safe). **Cutover
  note:** switching a running instance requires `lever stop` + `lever up` — the
  long-running Scion server process caches the old force-host env, so recreating
  only the agent is not enough. See the new worker-isolation §4.3.

## [0.6.0] - 2026-07-17

### Changed
- Agent images are now tagged **by architecture** (`scionlocal/lever-claude:arm64`
  / `:amd64`) instead of a shared `:latest`, and a **tagless** `manager.image` (or
  worker `image:`) auto-resolves to the jail's arch at apply time. A host that
  cross-builds both arches — an arm64 laptop producing an amd64 server image — no
  longer clobbers one arch's image with the other's under `:latest`, the failure
  mode where the jail loads a wrong-arch image that dies at boot with `exec format
  error`. `make lever-image LEVER_IMAGE_ARCH=<arch>` builds `FROM scion-claude:<arch>`
  and tags the output `:<arch>`; an explicitly-tagged or digest-pinned image ref is
  left untouched (the escape hatch). Instances that pinned `…:latest` should drop
  the tag to opt into arch-resolution.

### Fixed
- The capability-minting sidecar (`lever-agent serve-capability`) now re-reads
  the rotating agent leaf per broker handshake instead of freezing the boot
  cert. It built its mTLS client once via the static `Identity.Client()`, so
  after the leaf's 24h TTL every capability mint failed the broker handshake
  (`certificate has expired`) — taking down every brokered tool (each mints a
  capability first) while the broker itself stayed healthy, recurring roughly
  daily. The 2026-07-13 gateway fix covered Claude's proxied MCP/LLM traffic but
  not this second, direct-to-broker client. A new `agent.NewReloadingClient`
  (reusing the gateway's per-handshake `clientCertSource`) closes it, and
  `Identity.Client()` is now documented short-lived-only so no future long-lived
  holder reintroduces the trap.

## [0.5.0] - 2026-07-16

### Added
- `lever version` now appends build provenance to the release string when the
  binary carries it: the commit it was built from (short) plus `-dirty` for an
  uncommitted tree (any `go build` / `make install` from a git checkout), or the
  module version for a `go install …@vX` build. A make-install binary that lags
  its source no longer hides behind the bare hardcoded version string.
- `lever init --adopt`: record owner-customized scaffolds (the operator/agent
  SKILL.md files and the whole tree-root CLAUDE.md) as an accepted baseline in
  `.lever-state/skills-adopted.json`. Doctor's "operator skills" check and
  `lever init --check` then treat the adopted content as OK, and a plain
  `lever init` leaves it alone (including not appending the CLAUDE.md block).
  Previously a customized scaffold read `skipped-modified` forever. Adoption
  is deliberately a recorded baseline rather than a mute: the scaffolds live
  inside the agent-writable tree, so the check doubles as tamper detection —
  any change PAST the adopted baseline fails doctor as "modified since
  adoption", and the baseline itself lives host-side where an agent cannot
  re-bless its own edits. Only genuine customizations qualify: framework-
  current files and stale-but-unmodified scaffolds (plain-`init` refresh
  territory) never get a record. `lever init --force` still restores the
  framework content — for CLAUDE.md, by re-ensuring the marker block in place
  — and clears the now-stale adoption record.

### Fixed
- `lever up` no longer needs a second run to clear the first-boot
  `start-manager` race on a cold VM. The scion workstation daemon registers its
  runtime broker asynchronously after its Hub API comes up, so the first
  create/resume could act before the broker was ready — failing the apply (on a
  cold VM as a hub "context deadline exceeded") so only a second `up`
  reconciled. `start-manager` now waits for the runtime broker to be registered
  and online (via `scion hub brokers`) before acting, closing the window at the
  source; the wait is fail-soft, so it can never fail the bring-up on its own.
  As a backstop, the transient-broker retry also now treats a hub "deadline
  exceeded" as the same race, and the initial observe `scion list` rides that
  bounded, ctx-checked retry instead of failing on the first blip.
- `lever destroy` now clears the persisted controller PAT
  (`.lever-state/controller.pat`). The PAT is minted against the hub DB that
  lives inside the jail, so destroying the machine leaves it stale; the next
  `lever up` reused it (ensureControllerPAT no-ops when a PAT is already
  persisted) and the new hub's fresh DB rejected it, failing the readiness
  probe with "authentication failed" until the file was removed by hand. Only
  the current-instance teardown (no `--machine`) clears it, alongside the
  broker stop and staged-ticket cleanup it already does.
- `lever apply` no longer re-imports every agent image into the jail on each
  run. The `load-image` step now first compares the jail's podman image ID
  against the host docker image ID and skips the multi-GB `docker save |
  podman load` re-stream when they match (the config digest is stable across
  save/load, so equal IDs mean the exact bytes are already present). The check
  is fail-open — any uncertainty (a not-yet-loaded or rebuilt image, an inspect
  failure) falls through to a load, so it can never wrongly skip and leave a
  stale image. This matters most under the first-boot retry loop, where any
  step failing re-runs the *entire* plan: previously each retry re-streamed
  every image; now unchanged images are near-no-ops. After a load, the step also
  prunes dangling (untagged, unreferenced) jail images — so a rebuilt image,
  whose old copy the load leaves untagged, no longer ratchets the grow-only jail
  disk up by a full image size (a no-op when nothing was superseded). Pruning
  never touches a tagged or container-referenced image.
- A tool whose broker backend carries a path (e.g. qmd's `[::1]:3101/mcp`) now
  reaches that path exactly on the tool root, instead of a trailing-slash
  variant (`/mcp/`) that a strict streamable-HTTP endpoint 404s. The trailing
  slash was an artifact of the broker's subtree mux; qmd was the only tool with
  a path-suffixed backend and so the only one that couldn't connect. Path-less
  backends (every other tool) and sub-path requests are unaffected.
- `lever doctor`'s "agent certificate" check no longer cries wolf right after
  a healing restart: an expired-leaf rejection logged before the current
  broker started (pid-file mtime) is reported as healed rather than as an
  active failure. Previously any rejection inside the 15-minute window failed
  the check even when the restart that fixed it had already happened.
- The broker's mTLS serving cert now self-rotates. It was minted once at
  startup with the 24h leaf TTL, so a broker running longer than a day served
  an expired cert and every gateway handshake failed — tools down, and the
  agents' own `/renew` calls failed with it, so their leafs decayed too (the
  only remedy was a `lever stop && lever up` power-cycle). The broker holds the
  CA key, so it re-mints its serving cert in-process via a rotating
  `GetCertificate` source when less than 6h of validity remains; agent leafs
  were already kept fresh by the 12h renew sidecar once the broker stays
  reachable.
- The agent's mTLS client leaf now hot-reloads on the live MCP/LLM path. Claude
  Code read `CLAUDE_CODE_CLIENT_CERT`/`_KEY` once at process start and cached the
  leaf for its whole lifetime, so after ~24h the boot cert expired and every
  gateway call failed with `tls: certificate has expired` — despite the renew
  sidecar rewriting a fresh leaf every 12h — until the manager was restarted. A
  new `lever-agent gateway` sidecar now runs a loopback (127.0.0.1) reverse-proxy
  that terminates plaintext from Claude and re-presents the always-current leaf
  to the broker over mTLS, re-reading `agent.{crt,key}` per TLS handshake. Claude
  no longer holds the rotating cert: boot points its MCP `--transport http` URLs
  and `ANTHROPIC_BASE_URL` at the loopback gateway. The proxy caps idle broker
  connections at 5m so a rotated leaf reaches the broker well before the 24h
  expiry, uses `FlushInterval = -1` for MCP's SSE streaming, and binds loopback
  only (it holds the agent key). Boot-time discovery and llm-token calls still go
  direct to the broker (the gateway isn't up during pre-start).

## [0.4.0] - 2026-07-10

The single-project re-architecture (P1–P4): one Scion project per instance,
the manager and all workers as agents in it, the real hub running dev-auth-off
behind a host-side controller PAT. **Worker subtree isolation depends on
Scion's `--workspace-subdir` feature, which is not yet upstreamed** (fork branch
`feat/per-agent-workspace-subpath`); dispatching workers requires building Scion
from that fork (`scion.source`) until it lands — see the Fixed entry below.

### Added
- Single-project model: the manager and all workers now share one Scion
  project (the jail mount root), with workers living as in-place subdir
  workspaces (`workers/<name>`) instead of separate per-agent projects.
  Collapses `register-manager` + N×`register-worker` into one
  `register-project` apply step and the worker list into a single
  instance-project query. (P2 of the single-project re-architecture.)
- Controller-PAT bootstrap: `lever apply` mints a scoped controller PAT
  (`agent:manage`, `agent:attach`, `project:read`) via a throwaway dev-auth
  server, persists it `0600` under `.lever-state/`, and threads it into every
  scion client (including attach). The real hub now runs with
  `--dev-auth=false` by default. (P3 of the single-project re-architecture.)

### Changed
- Renamed the `grove` concept to `worker` throughout: config keys `groves:`→`workers:` and `grove_to_grove:`→`worker_to_worker:`, the `--grove`/`-grove` CLI flags → `--worker`/`-worker`, broker routes `/grove/*`→`/worker/*`, and the `groves/<name>` workspace convention → `workers/<name>`. Prerelease clean break — no migration. (P1 of the single-project re-architecture.)
- The agent image (`image/lever-claude`) pins Claude Code explicitly (`ARG
  CLAUDE_CODE_VERSION`) instead of inheriting whatever the scion base image
  baked. Bump the ARG + rebuild + `lever apply` to upgrade; the in-container
  auto-updater remains disabled (updates by rebuild, never at runtime).

### Fixed
- Workers now mount **only their own subtree** at `/workspace`. `lever`
  dispatches each worker with `scion start --workspace-subdir workers/<name>`
  (project-relative, containment-guarded) instead of an absolute `--workspace`,
  so a worker can no longer see the manager's tree — and the dispatch-time
  enrolment failure where a worker read the manager's bootstrap and inherited a
  spent ticket is gone with it. This relies on Scion's `--workspace-subdir`
  subtree-isolation feature (fork branch `feat/per-agent-workspace-subpath`,
  not yet upstreamed); it is **not in the pinned `scion.version`**, so
  dispatching workers today requires building Scion from that fork
  (`scion.source`) until the addition lands upstream.
- `lever up` self-heals an expired agent mTLS leaf: resume now re-stages a
  fresh enrolment ticket before reconnecting, so an instance left down longer
  than the leaf's lifetime no longer needs a full `lever destroy && lever up`
  to recover. Adds a `lever doctor` check that detects the expired-leaf
  handshake failure in the broker log.
- `lever attach <worker>` and `lever msg send --to <worker>` now target the
  single instance project instead of a stale per-worker project path,
  fixing worker addressing under the single-project model. (P4 §9)

## [0.3.1] - 2026-07-06

### Changed
- Module path is now `github.com/stevegeek/lever`, matching the repository —
  `go install github.com/stevegeek/lever/cmd/lever@latest` works. (The old
  declared path `github.com/lever-to/lever` never resolved: no such repo, so
  v0.3.0 and earlier were build-from-clone only.)

## [0.3.0] - 2026-07-06

### Added
- Audit mint ledger: every capability token now carries a random 128-bit id
  inside the signed payload. The broker's `/request` allow line records the
  id, the matched policy rule (`obtain:<agent>:<tool>.<op>` /
  `delegate:<agent>-><recipient>:<tool>.<op>`), expiry, epoch, and the baked
  constraints (JSON); gateway and `/llm` lines carry the same id on allows AND
  on every post-decode deny (revoked replay included), so any use of a token
  — permitted or refused — greps back to its mint: `grep id=<id>
  .lever-state/broker.log`. Token bytes are never logged; deny-line ids are
  the token's claimed id (signature not necessarily valid). Tokens minted by
  earlier builds verify but log an empty id until they expire.
- `lever reload`: apply config changes (new grove, tool, or grant) to a running
  instance without a VM power cycle — restarts the broker on the current config
  while leaving the manager container (and its conversation) running.
- `make lever-image`: build the generic, instance-agnostic agent image
  (`scionlocal/lever-claude:latest`) in-repo — scion's stock harness plus the
  lever binaries and boot hook. Instances extend it `FROM lever-claude:latest`.
  The examples are now buildable from a clean checkout.
- `examples/assistant-demo`: a runnable mini personal-assistant instance (a
  morning-standup manager + a todo grove) that demonstrates both tool models in
  one place — a first-party capability tool (`lever-tool-todo`, reads a CSV) the
  broker supervises, and an external MCP (`weather-stub`, canned data) the broker
  only proxies — plus grove dispatch and per-agent grants. Offline, no API key.

### Fixed
- Revocation now fails closed on every acting path. Previously only a revoked
  agent's tool calls were denied (at the gateway/`/llm`), so it could still mint
  or delegate tokens, message other agents, dispatch/tear-down groves (as the
  manager), issue enrolment tickets, or renew its cert. `lever revoke <agent>`
  now denies all of these; the agent's existing cert simply expires (renew is
  refused), making revocation terminal.

### Docs
- New "Capabilities" and "Operations & recipes" guides; a "CLI" reference page;
  a security-model section on what a compromised agent can and can't do;
  disclosures on token-in-LLM-context (safe via CN-binding), the subscription
  vs api-key trade, and tree-resident boot material persisting across `--fresh`.

## [0.2.0] - 2026-07-04

### Added
- Operator skills: framework-authored `lever-operator` (manager) and
  `lever-agent` (grove) SKILL.md files teaching agents the capability flow
  (mint via `lever-capability` `request`, attach as `_capability`), messaging,
  and grove dispatch.
- `lever init [--force] [--check]`: scaffolds the skills into the instance
  tree (tree root + each declared grove dir) with hash-guarded updates
  (locally-modified files are skipped with a warning unless `--force`) and an
  idempotent marker block in the tree-root CLAUDE.md.
- `lever doctor` check "operator skills": present / current / unmodified /
  CLAUDE.md block present.
- Skill files carry a `lever-version` frontmatter stamp.

### Changed
- Version is now `0.2.0` (was `0.0.0-dev`).

## [0.1.0] - pre-changelog era

Everything before this changelog: the containment jail (OrbStack/Lima
backends, egress allowlisting), the capability broker and mTLS gateway
(enrolment, typed tokens, MCP-aware `_capability` gating, `/llm` api-key
proxy), external MCP tools, broker-routed messaging, resume-reconciliation
(`lever stop`/`up` restores the manager conversation), and `lever doctor`.
See git history for details.
