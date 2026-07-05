# Changelog

All notable changes to lever are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Process: every merge
to `main` that changes behavior adds an entry under `## [Unreleased]`; a
version bump moves the block under the new version heading.

## [Unreleased]

### Added
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
