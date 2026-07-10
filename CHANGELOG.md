# Changelog

All notable changes to lever are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Process: every merge
to `main` that changes behavior adds an entry under `## [Unreleased]`; a
version bump moves the block under the new version heading.

## [Unreleased]

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
