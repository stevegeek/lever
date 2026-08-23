---
title: "Config trust"
nav_order: 5.3
parent: Security model
permalink: /security-model/config-trust/
---
Part of the [security model](/security-model/). Sections keep their original § numbers.

## 5. The operator boundary: the config is host-side code, kept out of the mount

Sections 1-4 harden the *inside* of the jail against hostile agents. But the host-side operator
binary (`lever`) reads its config and acts on it **before the jail exists**: which host directory to
bind-mount, which host file to read as a credential, which image to run, what text to inject as the
manager's task. The config is, in effect, host-side code, so two things matter: **where it comes
from** (can an attacker substitute it?) and **whether the agents it constrains can rewrite it**.

Both are closed:

### 5.1 The config never enters the mount (no in-jail tamper → host escalation)

The instance **root** holds `lever.yaml` and the boot prompt and is **not** mounted; only a `tree:`
**subdirectory** is bind-mounted. `tree` is validated as a confined relative subdir, not `.` (the
root itself), not absolute, no `..`, so it cannot be widened to `/`, `$HOME`, or the root. The boot
prompt is resolved at the root too (`ManagerPromptPath` joins the root, not the tree) and confined
there.

If the config or prompt lived *inside* the mount (the natural "config at the
project root, root == mount" layout), a compromised manager or worker could **rewrite the config the
host trusts on the next `lever apply`**, an in-jail-compromise → host-escalation persistence
channel (point it at `credential_file: ~/.ssh/id_rsa`, `tree: /`, an attacker image, etc.). Keeping
the root unmounted removes that channel: agents can't see or edit what the host re-reads.

**The boundary this draws — and what deliberately sits on the other side of it.** Operator-owned
boot material (`lever.yaml`, `prompt_file`) is host-side and tamper-proof from inside the jail.
Tree-resident material — `CLAUDE.md`, the skills scaffolded by `lever init`, the working files
themselves — is *inside the mount* and therefore inside the agent's own blast radius: a
compromised agent can rewrite its own `CLAUDE.md`, and Claude Code will auto-load the modified
version in every later session. This is not a containment failure (the file is within what the
agent already controls, and CLAUDE.md-loading is how the harness works — an agent could always
create one), but it has one consequence: **tree-resident instructions persist across
restarts**. `lever up --fresh` gives you a fresh conversation, not a fresh tree — if you suspect
an agent was prompt-injected, audit what it wrote to the tree (CLAUDE.md included), don't just
restart the thread. When you need boot instructions an agent cannot alter, `prompt_file` is the
mechanism; `lever init`'s scaffolds are working material, not a trust anchor.

For the scaffold files specifically (the SKILL.mds and CLAUDE.md), `lever doctor` gives you a
concrete drift signal to start that audit from: it flags any content that matches neither the
framework scaffold nor a baseline you accepted with `lever init --adopt`. The adopted baseline is
recorded **host-side** (`.lever-state/skills-adopted.json`, outside the mount), so an agent that
rewrites its own instructions cannot re-bless the edit — doctor reports it as "modified since
adoption" until you either re-adopt it yourself or restore with `lever init --force`. This narrows
the persistence window to files doctor doesn't watch (working files, scripts); it does not replace
the audit.

### 5.2 No walk-up discovery (no planted-parent config)

Config is resolved from the **current directory only**; there is deliberately **no walk-up**. A
`lever.yaml` planted in a parent directory of wherever you happen to `cd` can never be picked up and
trusted. Run `lever` from the instance root, or pass an explicit (trusted) path.

### 5.3 Field validation (defence in depth, even for a trusted config)

`config.Validate()` and the credential read enforce:

| Field | Check |
|---|---|
| `name`, worker `name` | `^[a-z0-9][a-z0-9-]{0,62}$` (it becomes the jail machine name and a shell token). |
| `tree` | confined relative subdir (not `.`/absolute/`..`); also rejected if it is itself a git repository (an ancestor `.git` is allowed), see [§4.1](/security-model/worker-isolation/). |
| `manager.prompt_file` | confined relative path under the root (no `..`, not absolute). |
| `manager.image`, worker `image` | safe OCI-ref charset; plus **opt-in** `security.allowed_image_registries` (run only images from trusted registries/namespaces) and `security.require_image_digest` (require `@sha256:`-pinned images, no mutable tags). |
| `credential_file` | read with a **permission check** (rejected unless mode is 0600: any group or world bit fails) and a **size cap**, defence in depth for the secret it becomes ([§6](/security-model/credentials/)). |
| worker `dir` | rejected if absolute or containing `..`; two workers' dirs must not overlap, and the name `manager` is rejected ([§4.1](/security-model/worker-isolation/)). |
| `scion.binary`, `scion.source` | must not resolve inside `tree` (an agent could otherwise supply the engine on the next bring-up); `binary`, `source`, `version` are mutually exclusive (checked in `config.Load`). |
| `scion.binary` | regular file, Linux ELF, architecture matches the guest's; checked host-side at bring-up (`verifyELFArch`) before the file is copied into the jail. |

**`scion.binary` is a trust decision, not only a convenience.** The bytes at that path are installed
as root at `/usr/local/bin/scion` and become the engine every agent runs under. That sits inside the
operator boundary this section describes — the config already names container images and mount
paths, and `scion.source` already builds arbitrary host code — so it grants nothing new in kind.
It does drop one property, and the drop is worth stating plainly: `scion.version` is fetched through
the Go module proxy and is **checksum-verified** against `sum.golang.org`, whereas a `binary:`
artifact carries no integrity guarantee lever can check. The architecture check catches an honest
mistake, not a substituted file. Choosing `binary:` makes its provenance yours to guarantee.

**Execution plumbing:** argv-clean, no shell injection in the hot paths; the `bash -c` scripts in
internal/backend/guest (scion install, scion settings write, web-assets staging) single-quote every
dynamic value via `shellSingleQuote`; the remaining two (agent template, login-forwarder disable)
interpolate only compile-time constants; `jailPath` never fabricates an in-jail path for an
out-of-tree target; the credential value is scrubbed from error output at its one call site (by
literal match, so a value that parses as a flag is masked too). The value travels as plaintext argv;
the scion CLI stores it verbatim (`encoding=raw`). A `scion.version` pin that does not support this
fails at apply with an explanatory error.

### 5.4 The manager holds no worker-dispatch authority

Worker lifecycle is owned by the host-side capability broker, not the in-jail manager, and the
broker itself is the only holder of the controller PAT ([§4.2](/security-model/worker-isolation/)) — the manager has no Scion hub
credential of its own, in-jail or otherwise. The manager's `agent start/stop/suspend/resume`
commands are thin mTLS clients of the broker's `/worker/*` endpoints. Each request is authenticated
by the manager's certificate CN and authorized against the config: only a worker **declared in the
config** can be dispatched, and the manager passes a worker **name**, never a filesystem path — the
broker resolves the subdirectory, image, and LLM-auth mode from the config host-side, within the
one instance project (there is no separate per-worker project to mount instead). A compromised
manager therefore cannot start an agent against an arbitrary path, widen a worker's mount beyond
its declared subdirectory, or inject a host path; the worst it can do is (re)dispatch a worker it
was already permitted to dispatch. Because the broker (not the mount) is the source of worker
configuration, there is no in-jail config file for a compromised manager to tamper with.

### 5.5 Residual

Image **registry allowlist** and **digest pinning** are opt-in `security:` policy
(§5.3), enable them to bound *which* registry an image comes from and to require vetted, immutable
images. `redactArgs` (internal/scion/client.go) masks by argv position, not by secret key name. The
dominant in-jail risks are [§6](/security-model/credentials/) (the projected credential) and [§8](/security-model/compromise/) (open-egress exfiltration): **closed
in api-key mode** (the default) by the capability broker ([§6.1](/security-model/credentials/)) plus `egress: closed`, and
still present under the subscription opt-in.
