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
boot material (`lever.yaml`, `prompt_file`, `instructions_file`, `image_tar`) is host-side and
tamper-proof from inside the jail.
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

### 5.1.1 Host-run code in the tree: `manager.read_only`

The tree is the agents' working area, but an operator may also keep code there that the **host**
runs (a CLI that works on the notes the agents write, scripts, hooks). Whatever the manager can
write, it can turn into code that runs on the host the next time you run it. `manager.read_only`
closes that for the directories it names:

- Each entry is bind-mounted **read-only over itself** in the manager container. Each directory
  between the tree root and an entry is bind-mounted read-write over itself (a *pin*): a mount point
  cannot be renamed or removed (`EBUSY`), so the manager cannot rename a parent away and create a
  fresh, writable directory at the protected host path.
- While `read_only` is set, every worker `dir` and its ancestors are **pinned** in the manager too,
  and must be reached through real directories only (apply refuses a symlink and creates a missing
  worker dir; the broker refuses a symlink anywhere on a worker's path and a workspace that is,
  contains or lies inside an entry). Without that, the manager could swap a worker dir for a link to
  a protected directory and dispatch the worker, which mounts its dir **read-write**. A worker dir
  that overlaps an entry is rejected at config load.
- Every entry must be a real directory reached through no symlink, and no symlink inside an entry
  may point outside it (code loaded through such a link would read agent-writable content). apply
  checks this before it acts on the manager record and again right before the create, after a
  `--fresh` delete has removed the old manager.
- Comparisons fold case: on a case-insensitive host (APFS by default) `Assistant/tools` and
  `assistant/tools` are one directory. Inside the container, a case alias of a protected path
  (`TOOLS`, `kb/TOOLS`) reaches the same mounted directory: writes are refused and a rename answers
  `EBUSY`. This was verified on OrbStack; it has **not yet been verified on Lima**.

- Every entry must hold no file with more than one hard link (another name for the inode, made
  before the protection, would be writable), no link that cannot be resolved, and must be ASCII
  (APFS treats composed and decomposed spellings as one name). While `read_only` is set the broker
  re-checks a worker's path on every resume and healer bounce too, because the runtime resolves the
  bind source again on each container start.
- **Edit protected directories in place; never replace the directory.** The read-only mount is
  bound to the directory that existed at the manager's create. If the host replaces it (`rm -rf` and
  recreate, a rename-based deploy, a `git checkout` that removes and re-adds it), podman still lists
  the read-only mount but it covers the old directory, and a write from the manager lands in the new
  one (verified on OrbStack). The protection is gone until the next fresh create. `lever doctor` and
  `lever apply` probe each entry live (a write test inside the running manager) and fail or warn when
  it is writable; a probe that cannot run is reported as unconfirmed, never as protected. The same
  holds for a worker dir (or an entry's parent) recreated on the host: the manager's pin stays on
  the old directory, and this is **not** detected — doctor checks pins by inspect only, because no
  non-mutating live test of a pin is known to be reliable. Never recreate those on the host.
- The hard-link refusal also catches a pnpm `node_modules` (hard-linked from its store) and the
  objects of a `git clone --local`; keep such things out of protected directories.

**Scope: it protects the entry directories, nothing else.** Whatever the host code loads from
**outside** an entry is still agent-writable and is still an escalation path: a `require`/import path
that reaches out of the entry, a `Gemfile`, `.bundle/config`, `package.json` or lockfile kept beside
(not inside) it, the tree's `.git` (hooks, `core.fsmonitor` and other config run code when **you** run
`git` there), `mise`/`asdf`/`direnv` files a shell picks up on `cd`. Move such files inside an entry,
or do not trust them. **Create-time only:** scion keeps a record's mounts for life, so the mounts reach
a manager created after the setting; `lever up --fresh` recreates an existing one (it discards the
manager's conversation, back it up first). `lever apply` warns, and `lever doctor`'s *manager
read-only paths* row fails, when the running manager lacks a mount. The same holds the other way:
removing an entry leaves it mounted read-only until a fresh create, and its directory must stay until
then (a resume of a record whose mounted directory is gone fails; apply refuses it up front and names
the directory).

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
| `manager.prompt_file` | confined relative path under the root (no `..`, not absolute), and rejected if it resolves inside the mounted `tree`. |
| `manager.instructions_file`, `workers[].instructions_file` | confined relative path under the root, like `prompt_file`, and — like it — rejected if it resolves inside the mounted `tree`. The contents become the agent's standing user-level `~/.claude/CLAUDE.md`; scion re-projects its staged copy on every container start, so an agent that rewrites the managed block gets the host's text back. |
| `manager.image_tar`, `workers[].image_tar` | confined relative path under the root, like `prompt_file`, and — like it — rejected if it resolves inside the mounted `tree`: the archive is the code the agent runs, so an agent must not be able to author the next bring-up's image. Needs a tag-bearing `image` (a digest pin cannot be matched against a tar's tags), the archive must carry that tag (a mismatch is a named error before a byte is streamed), and one image ref may come from only one archive. |
| `manager.read_only` | each entry a clean ASCII relative path inside `tree` (no `..`, `./`, trailing slash, `.`, `$`, `~`, `:`, `,`); no duplicates or nested entries (case-folded); no worker `dir` equal to, containing or inside an entry; at bring-up, every entry a real directory reached through no symlink, with no symlink inside it pointing out or failing to resolve and no hard-linked file, and every worker dir reached through no symlink (§5.1.1 above). While `read_only` is set, every worker `dir` is held to the same character and ASCII rules, because it becomes a pin. |
| `manager.image`, worker `image` | safe OCI-ref charset; plus **opt-in** `security.allowed_image_registries` (run only images from trusted registries/namespaces) and `security.require_image_digest` (require `@sha256:`-pinned images, no mutable tags). |
| host-run programs and host secrets | rejected at config load if they resolve inside `tree` (§5.5 below): `manager.credential_file`, `broker.api_key_file`, `operator.signing_key`, `operator.allowed_signers`, each supervised `broker.tools` `command`, and the path arguments in it. |
| `credential_file` | read with a **permission check** (rejected when any group or world bit is set) and a **size cap** (64 KiB), defence in depth for the secret it becomes ([§6](/security-model/credentials/)). |
| `broker.api_key_file` | must exist at mode exactly 0600, checked at config load for an instance with any api-key agent. |
| worker `dir` | rejected if absolute or containing `..`; two workers' dirs must not overlap, and the name `manager` is rejected ([§4.1](/security-model/worker-isolation/)). |
| `scion.binary`, `scion.source` | must not resolve inside `tree` (an agent could otherwise supply the engine on the next bring-up); `binary`, `source`, `version` are mutually exclusive (checked in `config.Load`). |
| `scion.binary` | regular file, Linux ELF, architecture matches the guest's; checked host-side at bring-up (`scionbin.VerifyELFArch`) before the file is copied into the jail. |

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
dynamic value via `shellSingleQuote`; the others interpolate only compile-time constants, plus
the run user name the guest itself reports (`whoami`) in the subuid/linger step; `jailPath` never fabricates an in-jail path for an
out-of-tree target; the credential value is scrubbed from error output at its one call site (by
literal match, so a value that parses as a flag is masked too). The value travels as plaintext argv;
the scion CLI stores it verbatim (`encoding=raw`). A `scion.version` pin that does not support this
fails at apply with an explanatory error.

### 5.4 The manager holds no worker-dispatch authority

Worker lifecycle is owned by the host-side capability broker, not the in-jail manager, and the
broker itself is the only holder of the controller PAT ([§4.2](/security-model/worker-isolation/)) — the manager has no Scion hub
credential of its own, in-jail or otherwise. The manager's `agent start/stop/suspend/resume`
commands (and `agent recycle`) are thin mTLS clients of the broker's `/worker/*` endpoints. Each request is authenticated
by the manager's certificate CN and authorized against the config: only a worker **declared in the
config** can be dispatched, and the manager passes a worker **name**, never a filesystem path — the
broker resolves the subdirectory, image, and LLM-auth mode from the config host-side, within the
one instance project (there is no separate per-worker project to mount instead). A compromised
manager therefore cannot start an agent against an arbitrary path, widen a worker's mount beyond
its declared subdirectory, or inject a host path; the worst it can do is (re)dispatch a worker it
was already permitted to dispatch, and, for a worker the operator marked `recyclable: true`,
discard that worker's record and conversation (never its workspace; never while it runs; at most
once a minute per worker). Without that key only the operator's `lever worker purge` deletes a
worker record. Because the broker (not the mount) is the source of worker
configuration, there is no in-jail config file for a compromised manager to tamper with.

**Host daemons keep the config they started with.** The broker and the remote proxy read the
config once, at start. `lever apply` compares a host-side stamp of the lever version and config each one runs
(the hash the broker reports on its loopback `/epoch` route, the proxy's stamp file under `.lever-state/`, which the running proxy
itself writes) with the current config, and restarts the daemon on a difference. Until that
apply runs, a config edit has no effect on them; `lever doctor` names a stale one where a row
covers the key (for example `agent messages`).

### 5.5 Host-run programs and host secrets

An agent can read and write anything inside the mounted tree. A program the host runs from there
runs agent code outside the jail, and a secret the host reads from there is readable (and
replaceable) by the agent. Config load therefore refuses these paths when they resolve inside
`tree`, in addition to `scion.binary`, `scion.source` and the boot files (§5.3):

| Kind | Paths | Inside `tree` |
|---|---|---|
| Secret or trust anchor | `manager.credential_file`, `broker.api_key_file`, `operator.signing_key`, `operator.allowed_signers`; in a tool `command`, the value of `-app-key`, `-token-file` and `-state` | always refused: a `manager.read_only` mount still lets the manager **read** the file |
| Private host path | in a `lever-tool-whisper` `command`, the value of `-models` and `-dictate-socket` (its model directory and dictation socket); `remote.voice.socket` while voice is on | always refused: an agent could replace the model whisper-server loads, or reach or replace the socket, and a `manager.read_only` mount does not excuse it |
| Program outside the tree | in a `lever-tool-whisper` `command`, the value of `-server` (the `whisper-server` program it runs) | always refused: a `manager.read_only` mount does not excuse it |
| Program | a tool `command`'s program when given as a path (a bare name is looked up on the supervisor's fixed `PATH`), also after an `env` prefix (its flags and `NAME=value` words are skipped); the value of `-fizzy`; the code an interpreter runs (`python`, `ruby`, `node`, `perl`, `sh`, `bash`, `dash`, `zsh`, `deno`, `bun`, a version suffix such as `python3.12` allowed): the script, which is the first argument after the leading flags (and after `run` for `deno` and `bun`; a relative one after `env -C dir` resolves against `dir`), the value of every value-taking flag before it (`python -W`, `node --env-file`, `deno --config`, `bun --cwd` and the like: a value read as the script must not hide the real one), the value of a flag that loads code (`ruby -I`/`-r`, `perl -I`, `node --require`/`--import`/`--loader`, `bun --preload`), and the paths in inline code (`python -c`; `ruby`/`perl`/`node -e`; for the `sh` family, the command string of `-c` in any option group such as `-ec`, `-ce` or `-euo pipefail -c`, and, as a fallback, the word after any `-c` group anywhere in the command) | refused unless it lies under a `manager.read_only` entry, reached through no symbolic link inside the tree, and in no worker `dir` (a worker mounts its dir read-write) |

**This is a best-effort guard, not a sandbox.** Lever cannot know what an unknown flag's value is: a
secret, a program, or a data file the agent is meant to edit (the example todo tool's `-csv`). So it
checks only the command itself, the path flags of the tools it ships (above; `-tree`, `-dsn` and
`-csv` are read and deliberately not checked as paths) and interpreter scripts. `-app-key`,
`-token-file`, `-state` and `-fizzy` are read in every supervised tool's command, so your own
tool's `-state` is checked the same way. `lever-tool-whisper`'s flags (`-server`, `-models`,
`-dictate-socket`, `-model`, `-whisper-port`, `-max-seconds`, `-agent-max-seconds`) are read only in
the command of a whisper tool: one whose program, after an `env` prefix, has the base name
`lever-tool-whisper`. Another tool's `-server` or `-models` means something else and is not read.
A word that starts with `-` is never taken as the previous flag's value, so `-model -state x` still
checks `x`. For a whisper tool, config load also requires `-tree` and refuses one that is not the
instance's tree, checks `-max-seconds` (1 to 600) and `-agent-max-seconds` (1 to `-max-seconds`), and
refuses a `-whisper-port` the jail may reach (`manager.allow_ports`, the broker's
jail port, the login port) or that collides with the broker's admin port, the remote proxy's port,
port 8446 or a tool backend. It does not check any other
flag or argument. **For your own tool, keep its secrets and the programs it runs outside the tree
yourself.** Only the leading flags of an interpreter are read: what follows the script is the
script's own business. Lever knows which of an interpreter's flags take a value from a fixed table; a
value-taking flag missing from it makes its value read as the script and hides the real one. Inline code is split on whitespace, quotes, shell punctuation, `=`, `:` and
`,`; a piece with a `/` in it counts (a relative one against the instance root, the tool's working
directory), and so does a flag with a path glued on (`-I/path`). A path written with a `..` component is
refused: the kernel resolves `..` after it follows links, so `ws/link/../tools/x` can reach somewhere
other than it reads. A path the check cannot inspect (a directory you may not search) is refused too,
with the path and the error.

The check follows the path one component at a time, as the kernel does. It recognises the tree, the
`read_only` entries and the worker dirs by identity, not by spelling, so a case alias on a
case-insensitive host, a link at the instance root that points into the tree, and a link inside the
tree that points out (an agent can repoint it) are all caught. A path that does not exist yet is
placed below its deepest existing parent. Relative paths in a tool `command` resolve against the
instance root: the broker supervises every tool with the instance root as its working directory,
whatever directory `lever apply` ran from, and resolves a bare command name on the supervisor's
fixed `PATH`, the one config load checks. The error names the key, the path and the fix: move the
file outside the tree, or, for a program, cover it with `manager.read_only`.

A program under `manager.read_only` is safe only while the running manager carries the read-only
mount, and that mount is create-time only (§5.1.1). So the broker does not start a tool whose program
relies on it until it has read the manager container's mounts and found the whole `read_only` plan in
place (entries read-only, their parents and the worker dirs pinned), and then checked the running
container from the guest side, without running any program inside it (the agent is root there and
could replace it): the kernel's `/proc/<pid>/mountinfo` for the container's process must list every
planned directory as a mount point, the entries read-only, and `stat` must find the same device and
inode at the guest path and at `/proc/<pid>/root/<target>`, so a protected directory replaced on the
host (the mount stays on the old one) is caught. It fails closed: no container yet, a stopped manager,
or anything it cannot read or parse holds the tool back. The reasons it logs are fixed text, never
container output. It asks again every 30 seconds (apply starts the broker
before the manager, so a fresh create gains the mounts after the first ask), and writes the reason
into the tool's log once per reason. `lever apply` names the held tools when it keeps or resumes a
manager that lacks the mounts. The fix is the one for the mounts themselves: back up the
conversation, then `lever up --fresh`. A manager that lacked the mounts before could already have
rewritten the program; check it before you trust it again. `lever-tool-github` refuses its `-app-key`
and `-state` inside its `-tree` on its own as well, and `lever-tool-whisper` its `-server`,
`-models` and `-dictate-socket`.

### 5.6 Residual

Image **registry allowlist** and **digest pinning** are opt-in `security:` policy
(§5.3), enable them to bound *which* registry an image comes from and to require vetted, immutable
images. `redactArgs` (internal/scion/client.go) masks by argv position, not by secret key name. The
dominant in-jail risks are [§6](/security-model/credentials/) (the projected credential) and [§8](/security-model/compromise/) (open-egress exfiltration): **closed
in api-key mode** (the default) by the capability broker ([§6.1](/security-model/credentials/)) plus `egress: closed`, and
still present under the subscription opt-in.
