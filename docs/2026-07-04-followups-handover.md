# Handover: upstream Scion issues + Lever follow-up backlog (2026-07-04)

Written at the close of the resume-reconciliation arc (lever_to `main` @ `d117cf1`, pushed).
Context for every item below: `lever stop` → `lever up` now restores the manager's
conversation across a VM power-off; apply is observe-first (idempotent register,
state-driven start-manager, post-start liveness verification, create-path bootstrap
re-arm). Full arc record: `.superpowers/sdd/progress.md` (ledger),
`.superpowers/sdd/resume-branch-review.md` (adversarial review + fix wave + re-review),
`drafts/2026-07-04-resume-reconciliation-plan.md` (gitignored plan with the evidence base).

---

## Part 1 — Upstream Scion issue candidates

All were live-diagnosed on 2026-07-04 against the pinned Scion build
(instance `scion.version: 666333f9`, cross-compiled per `lever.yaml`). None are
blockers for Lever — we shipped workarounds — but each is a genuine upstream
defect or sharp edge worth reporting. Ordered by severity.

### Issue 1 — RETRACTED (measurement error), replaced by a real start-vs-stopped bug

**Original claim:** "CLI exits 0 on a 409 agent-already-exists error." **WRONG —
retracted 2026-07-04 evening.** The live capture piped scion through `head`
(`scion … | head -6; echo RC=$?`), so RC measured `head`, not scion. An upstream
investigation at HEAD `4a9c8767` proved the 409 propagates through every layer
(cmd/common.go:1122 → :828 wrapHubError → RunAgent :418 → cmd/root.go:214
unconditional `os.Exit(1)`) and reproduced the exact reported output with a
mocked 409 — scion exits 1. Lever's historical false-success came from OUR
`AlreadyRunning` predicate matching the 409's "already exists" error TEXT in
the retry loop; the `waitManagerLive` liveness verify guards that class either
way. Lever code comments carrying the false claim were corrected same day.

**The REAL upstream bug the investigation surfaced instead:**
`scion start <agent>` auto-RESUMES a *suspended* agent (CLI sets `Resume=true`,
cmd/common.go:809) but hard-409s a *stopped* one — even though the server
supports stopped-restart when `Resume=true`
(pkg/hub/handlers_agent_create_helpers.go:420) and `scion resume` handles
stopped agents fine. The CLI asymmetry (suspended→resume, stopped→409) is the
actual trigger of the confusing conflict, and the fix is to treat stopped like
suspended in start's resume-detection. Fix + test developed on the fork
(branch `fix/start-resumes-stopped`, see Part 1 status below).

### Issue 2 — `start`/resume reports success without verifying the harness survives

**Severity:** medium-high (false success on the primary happy path).

**Repro** (performed live):
1. Suspend an agent (`scion suspend <agent>`), then hard-kill its container
   out from under it — e.g. power off the VM (container shows `Exited (255)`).
2. Power the VM back on, restart the scion server.
3. `scion -g <project> start <agent> ...` (same argv as above).

**Observed:**
```
Resuming agent 'assistant'...
Agent 'assistant' resumed via Hub.
Agent Slug: assistant
Phase: resumed
RC=0
```
scion `podman start`s the exited container and returns success immediately. If
the relaunched harness dies moments later (we observed claude exiting 1 within
~6s when its enrolment pre-start hook failed), the CLI has already reported
"resumed" with rc=0. There is no liveness window, no `--wait` option.

**Nuance worth including in the report:** the resume mechanism itself is GOOD —
it relaunches with `claude --continue`, which restores the conversation from the
agent home. The defect is only the unverified success report.

**Impact on Lever (historical):** the other half of the false-success class.
Same durable workaround (`waitManagerLive`).

**Suggested report:** "start/resume should verify the container (and ideally the
harness process) is still alive after a grace window, or offer `--wait`; today a
resume of a container that exits 1 immediately still prints success and exits 0."

### Issue 3 — `list --format json` containerStatus vocabulary is inconsistent

**Severity:** low (sharp edge for machine consumers).

**Observed values for the SAME field across states (live captures):**
| state | `containerStatus` |
|---|---|
| live container | `"Up 6 seconds"`, `"Up Less than a second"`, `"Up About a minute"` (podman status TEXT, passthrough) |
| stopped record, no container | `"stopped"` (canonical token) |
| dead container present | `"Exited (1) 4 minutes ago"` (podman TEXT) |

A JSON consumer cannot equality-match; it must prefix-match `"Up"` AND know the
canonical tokens. Lever's `containerLive()` (internal/apply/run.go) does exactly
that — and its comment documents the observed shapes.

**Suggested report:** "list --format json should emit a canonical
containerStatus enum (running/stopped/exited/absent) and, if the raw podman text
is useful, a separate `containerStatusRaw` field."

### Issue 4 (design note, optional) — `list` performs a mutating lazy Hub sync

`scion list` (any format) detects Hub records whose broker/container is gone and
REMOVES them, prompting `Proceed with sync? (Y/n)` — auto-accepted on non-tty.
A read verb with a destructive side effect surprised us twice during diagnosis
(each inspection `list` erased the evidence being inspected). It is benign for
Lever's observe-first flow (container-less stale records correctly read as
absent) and `--non-interactive` makes it deterministic, but "read mutates state,
interactively" is worth raising as a design question — perhaps sync belongs
behind an explicit `scion sync` verb or a `--no-sync` escape hatch on list.

---

## Part 2 — Lever follow-up backlog (small, none blocking)

### B1 — Go-toolchain auto-resolution for `lever up`/`apply`

**Problem:** apply's scion build step (pin → `go mod download` + cross-compile)
needs a REAL Go toolchain on PATH. An asdf/mise SHIM resolves in an interactive
shell but fails with exit 126 when invoked from lever's sub-process context.
Bit us live 2026-07-03.

**Current state:** `lever doctor` has a dedicated check (runs `go version`,
prints the exact `export PATH="$HOME/.asdf/installs/golang/<ver>/go/bin:$PATH"`
fix), and getting-started documents the export. So it is *diagnosed* well, just
not *solved*.

**Proposed fix:** lever resolves the toolchain itself before shelling out:
try `go` on PATH; on failure (or shim-detect), probe well-known install roots
(`~/.asdf/installs/golang/*/go/bin`, `~/.local/share/mise/installs/go/*/bin`,
`/usr/local/go/bin`, Homebrew) and prepend the newest hit to the child's env.
Acceptance: `lever up` succeeds from a shell where `go` is only an asdf shim;
doctor check downgrades to informational.

**Where:** the scion build invocation (grep for where `scion.version` drives
`go mod download`) + a small `internal/toolchain` resolver with table tests.

### B2 — `lever-capability` request vocabulary / discoverability

**Problem:** agents (groves especially) guess wrong request shapes when minting
capabilities through the in-container `lever-capability` MCP tool — wrong
`tool`/`op` combinations, fine-shaped requests against coarse tools. The broker's
coarse-coercion fix (op → `"*"` when the tool is coarse-gated) absorbed one class;
the rest still surface as opaque 403s the agent cannot self-correct from.

**Proposed fix, two halves:**
1. **Discoverability:** `lever-capability` gains a `list`/`describe` verb backed
   by broker `GET /tools` (+ per-tool gate + operations when fine) so an agent can
   ASK what shapes exist before requesting. Expose it in the MCP tool description
   so the harness sees it without tribal knowledge.
2. **Error ergonomics:** broker `/request` deny responses should echo the
   expected shape for the named tool (gate, valid ops, an example request),
   not just `denied`. Audit-log parity already exists (deny audits carry
   tool/op/bound_to since the messaging arc) — this extends the same courtesy
   to the requester.

**Security note for the implementer:** the describe surface must only reveal
tools the caller could plausibly obtain (its own grants), not the full registry —
check `MayObtain` before listing.

### B3 — Assistant CLAUDE.md stale Ruby section (instance repo, not lever_to)

`~/ai/assistant/workspace/assistant/CLAUDE.md` still instructs "ensure Ruby is
present for asst / install on demand with rv; no Ruby in the image". WRONG since
2026-07-04: the lever-claude image BAKES Ruby 4.0.1 at `/opt/lever/mise`
(shadow-proof, symlinked into `/usr/local/bin` — see assistant repo commit
`ddfca02` and the Dockerfile comments). Fix: rewrite that section to "Ruby is
baked into the image (4.0.1); `ruby`/`gem`/`bundle` are on PATH" and delete the
on-demand install steps. One-file doc change in the assistant repo.

### B4 — `watch.go` stale "operator inbox" comment (cosmetic)

`cmd/lever-manager` watch.go carries a comment describing the pre-messaging-arc
"operator inbox" model. Messaging is broker-routed since 2026-07-03
(`/msg/send`+`/msg/list`, identity-derived policy). Update the comment; no
behavior change.

### B5 — Watch item: one transient first-`up`-after-stop failure (unreproduced)

During golden-cycle validation, the FIRST `lever up` after a `lever stop` exited
1 exactly once; the immediate retry succeeded and the conversation was restored.
The failing run's stderr was lost (controller piped it to /dev/null — lesson
recorded). A second full stop→up cycle immediately after passed first-try, as
did every later cycle.

**If it recurs:** capture the full log (`lever up --no-attach > up.log 2>&1`).
Prime suspects, in order: (a) scion's in-jail runtime broker warming up slower
than the resume retry budget right after VM boot (`brokerStartAttempts` ×
`brokerStartInterval` in internal/apply/run.go — resume rides this retry since
the fix wave, but the budget was tuned for the create path); (b) hub
`waitHubReady` racing the first post-boot list; (c) host-broker restart timing
when RearmBootstrap fires. The failure mode is SAFE (loud error, retry heals,
nothing destroyed) — which is why it's a watch item, not a bug ticket.

### B6 — (inherited, unchanged) broker `Setsid` was DONE; grove-side notes

For completeness against older handovers: broker daemonization (Setsid +
serve-owned pidfile + out-log) shipped 2026-07-04 and is live-proven — the
recurring "broker dies after apply" workaround is obsolete. Grove lifecycle
paths in `internal/broker/grove.go` still call `runtime.List` and were verified
tolerant of the `--non-interactive` List change (branch review, point c).

---

## Where everything stands (quick recovery map)

- lever_to `main` @ `d117cf1`, PUSHED to `github.com/stevegeek/lever`.
- Assistant instance repo (`~/ai/lever`): lever.yaml migration + .gitignore
  committed (`855d3cb`); tree clean.
- Instance machine: UP at handover time, manager running with the golden-cycle
  test conversation (attach with `lever attach`, or `lever up --fresh` for a
  clean thread — `--fresh` now hard-fails if the old record can't be deleted,
  by design).
- Ledger: `~/ai/lever_to/.superpowers/sdd/progress.md` (arc-by-arc, incl. this one).
- Adversarial review + fix wave + re-review: `.superpowers/sdd/resume-branch-review.md`.
- Task reports: `.superpowers/sdd/resume-t{1,3,4}-report.md`, `rearm-report.md`.
