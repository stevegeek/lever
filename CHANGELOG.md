# Changelog

All notable changes to lever are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Process: every merge
to `main` that changes behavior adds an entry under `## [Unreleased]`; a
version bump moves the block under the new version heading.

## [Unreleased]

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
