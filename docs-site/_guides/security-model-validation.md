---
title: "Validation"
nav_order: 5.6
permalink: /security-model/validation/
---
Part of the [security model](/security-model/). Sections keep their original § numbers.

## Validation status

> **Validation status.** *Shipped and validated:* the containment primitives (§9); the capability
> broker, mTLS enrolment, CN-bound capability minting, the six-check `lever acceptance` gate,
> and the **api-key `/llm` strip-and-inject path end-to-end** (broker verifies the capability token,
> strips it, injects the real Console key host-side), guarded by `make test-apikey-e2e`; container boot
> enrols the agent and registers the broker tools over mTLS; the **single-project model** (§4) — one
> Scion project per instance, the real hub running dev-auth off, lifecycle driven only by a host-only
> controller PAT, and worker↔worker isolation by defense-by-absence — is implemented (worker
> isolation currently relies on a Scion `--workspace-subdir` addition carried on our fork, not yet
> upstreamed; see §4.1).
> *Mid-session cert/leaf rotation is now built and live:* the agent's leaf
> is short-lived and renewed every 12h, and every long-lived broker client re-reads it per handshake
> rather than caching the boot cert — see §6.3 and [architecture.md §7](/architecture/).
> *Still pending:* the full in-container claude driving a first-party tool (`/mcp/db/`) end-to-end;
> a running session's pickup of a rotated **LLM bearer token** (`ANTHROPIC_AUTH_TOKEN`, api-key mode
> only — `renewOnce` rewrites `settings.json`, but Claude reads it once at startup); and a **dedicated
> live acceptance gate for the single-project isolation guarantee**:
> the mechanism (§4) is implemented, though worker isolation currently requires the fork-only
> `--workspace-subdir` addition (not yet in the pinned `scion.version`; see §4.1), and the checks
> that would exercise it against a real `scion start` (sibling subdirectories,
> a stray ancestor `.git`, the controller PAT's exact scopes) are not yet wired into
> `lever acceptance`.

## 9. Validation evidence

Validated by hand on macOS + OrbStack (Apple Silicon). What was demonstrated:

- **The escape exists without the jail.** From inside an ordinary agent container (no host
  filesystem, no Docker socket), using only a Scion hub **development-auth token** (the mode used
  for this test; the real hub now runs `--dev-auth=false` and no agent is ever handed a hub
  credential, §4.2), the hub accepted a new project + a **provider with an arbitrary host path (no
  path validation)**; starting an agent then drove the broker to bind-mount a sensitive host
  directory into a new container, whose files were readable. This is the threat the jail closes:
  even where an agent *did* hold a hub credential, there is no host filesystem outside the project
  tree for it to mount (§2.1).
- **Filesystem containment holds with the jail.** An isolated machine has no host home; the host
  secret directories are absent, so the same escape mounts nothing.
- **LAN unreachable; host loopback reachable and clampable.** From inside the jail the LAN router
  was unreachable (100% packet loss) while a host loopback tool server answered via the alias (over
  both IPv4 and IPv6); an egress allowlist then permitted one tool port and dropped the rest, and
  the rule still held for a rootless `--network=host` container (the topology agents actually use).
- **The runtime runs inside the jail.** Rootless podman runs containers (native `overlayfs`); the
  Scion binary builds and runs; a rootless host-networked container obeys the egress allowlist.

What is **not** yet validated (pending the full-system test): the project-tree mount's *allow* side
(that exactly the chosen tree is present and nothing else), the real manager Claude agent under
rootless podman, the manager's MCP reachability in practice, and a live run of the §4
single-project isolation guarantee against a real `scion start` (the lever code is implemented and
was live-validated once by hand, but worker isolation currently depends on the fork-only
`--workspace-subdir` Scion addition, not yet in the pinned commit, and there is no wired acceptance
check for it yet, see §4.1 and §4.2).

> Validation was performed by checking reachability and file presence by size/permission, never by
> printing secret contents.
