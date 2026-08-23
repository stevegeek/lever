---
title: "Validation"
nav_order: 5.6
parent: Security model
permalink: /security-model/validation/
---
Part of the [security model](/security-model/). Sections keep their original § numbers.

## Coverage

Validated (by hand or by an automated gate):

- Containment primitives (§9 below).
- Capability broker, mTLS enrolment, CN-bound capability minting, the six-check `lever acceptance`
  gate.
- Api-key `/llm` strip-and-inject path end-to-end (`make test-apikey-e2e`): the broker verifies the
  capability token, strips it, and injects the real Console key host-side.
- Container boot enrols the agent and registers the broker tools over mTLS.
- Single-project model ([§4](/security-model/worker-isolation/)): one Scion project per instance,
  hub with dev-auth off, lifecycle driven only by a host-only controller PAT, worker isolation via
  Scion's relative `--workspace`.
- Mid-session leaf rotation: the agent's leaf is renewed every 12h and every long-lived broker
  client re-reads it per handshake ([§6.3](/security-model/credentials/), [agent identity](/agent-identity/)).
- `lima` backend (`make test-lima-e2e`): the six `lever acceptance` checks under both egress
  postures, guest port-forward suppression, idempotent closed re-bring-up, and teardown on a real VM.

Not validated:

- The in-container claude driving a first-party tool (`/mcp/db/`) end-to-end.
- A running session's pickup of a rotated LLM bearer token (`ANTHROPIC_AUTH_TOKEN`, api-key mode):
  `renewOnce` rewrites `settings.json`, but Claude reads it once at startup.
- A live acceptance check for the §4 isolation guarantee against a real `scion start` (sibling
  subdirectories, a stray ancestor `.git`, the controller PAT's exact scopes). The mechanism is
  implemented; `lever acceptance` does not exercise it.
- The project-tree mount's allow side (exactly the chosen tree is present and nothing else), the
  manager Claude agent under rootless podman, and the manager's MCP reachability.

## 9. Validation evidence

Validated by hand on macOS + OrbStack (Apple Silicon). What was demonstrated:

- **The escape exists without the jail.** From inside an ordinary agent container (no host
  filesystem, no Docker socket), using only a Scion hub **development-auth token** (the mode used
  for this test; the real hub runs `--dev-auth=false` and no agent is ever handed a hub
  credential, [§4.2](/security-model/worker-isolation/)), the hub accepted a new project + a **provider with an arbitrary host path (no
  path validation)**; starting an agent then drove the broker to bind-mount a sensitive host
  directory into a new container, whose files were readable. This is the threat the jail closes:
  even where an agent *did* hold a hub credential, there is no host filesystem outside the project
  tree for it to mount ([§2.1](/security-model/jail/)).
- **Filesystem containment holds with the jail.** An isolated machine has no host home; the host
  secret directories are absent, so the same escape mounts nothing.
- **LAN unreachable; host loopback reachable and clampable.** From inside the jail the LAN router
  was unreachable (100% packet loss) while a host loopback tool server answered via the alias (over
  both IPv4 and IPv6); an egress allowlist then permitted one tool port and dropped the rest, and
  the rule also held for a rootless `--network=host` container; agents run under pasta, whose
  egress re-emerges on the same `OUTPUT` chain ([§4.3](/security-model/worker-isolation/)).
- **The runtime runs inside the jail.** Rootless podman runs containers (native `overlayfs`); the
  Scion binary builds and runs; a rootless host-networked container obeys the egress allowlist.


> Validation was performed by checking reachability and file presence by size/permission, never by
> printing secret contents.
