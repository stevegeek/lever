# Handover: a worker enrolment-ticket channel the manager cannot read

Date: 2026-09-07. Status: design + plan for a fresh session. Nothing implemented.
Origin: residual P2 of `docs/audits/2026-09-06-security-review-0.19-to-0.21.md`
(that directory is gitignored; the report is local to Stephen's checkout).
Everything else from that review is merged on `main` (`59e2144`, unreleased).

## 1. Problem

A compromised manager can hold any worker's identity. Two paths, both in the
broker:

1. **Staged ticket in the manager's mount.** `stageFreshTicket(cn, dir)`
   (`internal/broker/worker.go:142`) writes a fresh single-use enrolment
   ticket to `<tree>/<dir>/.lever/bootstrap.json` on every fresh start
   (`worker.go:408`) and every resume (`worker.go:358`).
   `BootstrapDir` is `filepath.Join(app.Tree, g.Dir, ".lever")`
   (`internal/brokerctl/workerspecs.go:21`). The manager mounts the whole
   tree in place (`apply/run.go` `Workspace: jp`), so it reads the ticket in
   the window between staging and the worker's boot.
2. **`/provision` on the jail listener** (`internal/broker/provision.go`,
   route in `server.go` ~701): manager-gated, returns a fresh ticket for any
   declared worker. Its only legitimate caller is the acceptance harness,
   which runs `lever-agent provision` INSIDE the VM as the manager
   (`internal/cli/host/acceptance.go:262-268`).

`/enrol` (`internal/broker/enrol.go:37-63`) is certless by design: it signs
whatever CSR carries the ticket's CN. Nothing ties a ticket to the container
that should redeem it.

## 2. Consequence (why it matters)

The manager redeems the worker's ticket with its own key and receives a leaf
certificate in the worker's name. It then:

- obtains the worker's `obtain:` grants — matters exactly when a worker is
  allowed something the manager is not (the least-privilege pattern the docs
  recommend);
- reads the worker's inbox and consumes operator directives addressed to the
  worker (`enrol.go:60-63` bumps the directive generation on enrol), which
  contradicts `security-model-operator-directives.md` §11.6 ("authority does
  not launder");
- wedges the real worker (ticket spent → its boot enrol fails → `error`
  phase). That is also the audit signal: a worker enrolment with no matching
  dispatch, plus a worker that never came up.

It gives NO host access, no hub credential, no LLM key, no VM escape, and
nothing beyond grants declared for that worker in `lever.yaml`. For the
assistant instance (one worker, no grants the manager lacks) the exposure is
nil today; it is real for any instance that withholds a capability from the
manager.

## 3. Invariants to keep

- The instance root stays unmounted; the tree is the ONE writable mount.
- No container holds a hub credential. Tickets are 32 random bytes,
  single-use, CN-bound (`internal/cap/ca/ticket.go`); keep that.
- `wire.Stage` (`internal/wire/bootstrap.go`) — `os.Root`, refuses symlinks,
  0600 — stays the writer for the MANAGER's own ticket at
  `<tree>/.lever/bootstrap.json` (the manager may read its own).
- The worker's boot path: scion's pre-start hook
  (`cmd/lever-agent/scionhook/pre-start`) runs `lever-agent boot` as root,
  which reads the bootstrap from `$LEVER_BOOTSTRAP` else
  `./.lever/bootstrap.json` (`cmd/lever-agent/main.go:70-72`), enrols, writes
  `~/.lever-id`. Since 2026-09-06 that read is `os.Root`-confined to the tree
  (`internal/agent/boot.go` `LoadBootstrap`); it will need a second anchor
  for the new location.
- `lever acceptance` is the live merge gate (Makefile); it must keep working.
- Worker resume re-stages a ticket (leaf may have lapsed) — the new channel
  must serve resume as well as create.

## 4. Current flow (fresh start)

```
lever-manager agent start w --task T            (in manager container)
  → broker /worker/start (mTLS, manager CN)
    → ensureWorkspaceDir (os.Root at tree)        worker.go ~193
    → stageFreshTicket(w, <tree>/<dir>/.lever)    worker.go:408   ← readable by manager
    → runtime.Start(StartOpts{Workspace: <dir>, Image, Model, Instructions, ...})
        scion start ... [--config -] -- w T       (instructions JSON on stdin)
  worker container boots, pre-start hook: lever-agent boot
    reads /workspace/.lever/bootstrap.json → /enrol → leaf
```

## 5. Options

**A. Per-agent scion secret.** Rejected: scion secrets are project-wide
("projected to every container", see memory `scion secret injection
contract`), so every agent including the manager would see it.

**B. Stage the ticket in the GUEST outside the tree, bind-mount it into the
worker only. Recommended.**
- The broker (host) writes the ticket through the jail runner to a run-user
  private dir in the guest: `$XDG_RUNTIME_DIR/lever/tickets/<worker>/bootstrap.json`
  (0700 dir, 0600 file, tmpfs). Same mechanism the attach PAT staging uses
  (`internal/jail/hubtoken.go` `StageHubToken`: fixed script, value on stdin,
  `umask 077`). The manager container cannot see `/run/user/<uid>`; it mounts
  only `/lever` (its tree) and its own home.
- The worker gets that file via scion inline config on stdin, which lever
  already sends for instructions (`internal/scion/lifecycle.go` ~564,
  `--config -`, struct at ~574): add `volumes: [{host: <that dir>, container:
  /run/lever, ro: true}]` and `env: {LEVER_BOOTSTRAP: /run/lever/bootstrap.json}`.
  The pinned scion (`ce96122c`, also `68507153`) honours both:
  `pkg/api/types.go:439-476` (`Env`, `Volumes`), `pkg/agent/provision.go:780,
  1104`. VERIFY FIRST that the `--config` inline path (not only the harness
  config dir) reaches those lines for `volumes` — the P1 reviewer traced it to
  `cmd/common.go` `ResolveContent` + provision; confirm with a throwaway
  start on `tmp-remote-e2e` (stopped; `lever up` in `~/ai/tmp-remote-e2e`).
- Rootless podman bind-mounts a run-user path fine (same uid). Check the lima
  backend has the same `XDG_RUNTIME_DIR` (the jail env sets it on every
  command: `internal/jail/runner.go` `jailEnv`).
- Resume: re-stage the file at the same guest path before `scion resume`; the
  container keeps its mount. A worker created BEFORE this change has no
  mount → its resume must fall back to the old in-tree path OR the operator
  recreates it (`lever worker purge` + dispatch). Decide; recreate is simpler
  and honest (changelog + doctor row).
- The tmpfs is cleared on VM reboot; that is fine because every start and
  resume re-stages.

**C. Bind the ticket to the container (boot nonce / container identity).**
Rejected for now: needs a secret channel anyway to deliver the nonce, so it
reduces to B.

## 6. Plan (B)

1. `internal/jail`: `StageWorkerTicket(ctx, r, prefix, uid, worker, payload)`
   → fixed script (`mkdir -p` 0700 `$XDG_RUNTIME_DIR/lever/tickets/<w>`,
   `umask 077`, `rm -f`, `cat > bootstrap.json`), payload on stdin like
   `StageHubToken`. Worker name is `nameRE`-validated config, never a
   request value. `TicketMountSpec(worker)` returns the guest host path.
   Tests: argv has no ticket bytes; real-shell test writes 0600 under a temp
   `XDG_RUNTIME_DIR`.
2. `internal/scion`: `StartOpts` gains `Volumes []VolumeMount` and
   `Env map[string]string`; `inlineConfig` (the `--config -` JSON) carries
   them beside `agent_instructions`; send `--config -` whenever ANY of the
   three is set. `Resume` needs nothing new (mounts persist) — but confirm
   scion's resume redispatch keeps `volumes` from the stored config.
3. `internal/broker/worker.go`: replace `stageFreshTicket(spec.Name,
   spec.BootstrapDir)` with the guest staging (broker runs on the host and
   already has the jail runner for scion — check `Broker.runtime`/how it
   reaches the guest; `brokerctl` builds it). Delete `WorkerSpec.BootstrapDir`
   and the in-tree `.lever` dir for workers. Keep the manager's `wire.Stage`.
4. `internal/agent/boot.go` `LoadBootstrap`: allow the `/run/lever` anchor
   (read-only mount, root-owned dir inside the container — confine with
   `os.Root` at `/run/lever`, regular file, size cap).
5. Remove `/provision` from the jail listener, `lever-agent provision`
   (`cmd/lever-agent/main.go` ~364), `agent.Provision`, `wire.PathProvision`,
   the handler + tests. Update `docs-site/_guides/agent-identity.md:23` and
   `capabilities.md`.
6. Acceptance harness (`internal/cli/host/acceptance.go` step 4): mint the
   worker ticket host-side (a broker admin-socket op, or call
   `stageFreshTicket`'s minting directly from the host process that already
   owns the broker) and stage it through step 1's channel; drop the in-VM
   `lever-agent provision` call. `make acceptance` must pass on a live VM
   (use `tmp-remote-e2e`, NOT the assistant).
7. Doctor: a row that fails when a worker record's container lacks the
   `/run/lever` mount (pre-change worker) with fix `lever worker purge NAME`
   then re-dispatch. `scion list` JSON may not expose mounts; podman inspect
   through the jail runner does (`podman inspect --format '{{json .Mounts}}'`).
8. Docs: remove the residual paragraphs added 2026-09-06 in
   `security-model-worker-isolation.md` (§4.1) and
   `security-model-operator-directives.md` (§11.6); state the new channel.
   CHANGELOG under Unreleased.
9. Live validation on `tmp-remote-e2e`: dispatch a worker, `podman exec` as
   the MANAGER container and prove `/run/user/<uid>/lever/tickets` is not
   visible; prove the worker enrolled; `lever acceptance` green.

## 7. Tests that pin the property

- Broker unit: after `/worker/start`, no file exists under `<tree>/<dir>/.lever`
  (grep the fake runner calls for the staging script + stdin instead).
- Broker unit: `/provision` returns 404 from the jail mux.
- Scion unit: inline config JSON carries `volumes` + `env` and the argv keeps
  the `--` terminator shape (`TestStartArgvPutsFlagsFirstThenTerminatorThenPositionals`).
- Agent unit: `LoadBootstrap` reads `/run/lever/bootstrap.json` via
  `LEVER_BOOTSTRAP` and refuses a symlink there.

## 8. Process notes for the session

- Work in a worktree under `~/ai/lever_to-worktrees/`, TDD, independent
  subagent review before merge, never force-push, no backticks in
  `git commit -m` (use `-F`). Do not run anything against the live assistant
  (`~/ai/assistant`); use `~/ai/tmp-remote-e2e` (currently stopped).
- Related, unreleased on `main`: the 2026-09-06 security fixes. Cut a
  release (0.22.0) before or after this; the assistant needs `make install`,
  `lever init`, an agent image rebuild (`make lever-image`) and
  `lever up --fresh` to pick everything up.
