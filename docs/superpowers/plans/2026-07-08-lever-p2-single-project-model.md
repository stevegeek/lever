# Lever P2 — single-project model Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collapse lever's per-worker Scion projects into ONE instance project: the manager and every worker become agents in the single `/lever` project, workers distinguished by an in-place subdirectory `--workspace` rather than a separate project.

**Architecture:** Phase 2 of the re-architecture (`docs/superpowers/specs/2026-07-08-lever-single-project-rearchitecture-design.md`, §3/§4/§7/§8/§11 + the big-picture corrections in commit `45ca8a6`). The manager already runs as agent `app.Name` in project `/lever` with workspace `/lever` (`internal/apply/run.go:348-353`) — that IS the instance project. P2 makes workers join it: the broker's overloaded `WorkerSpec.JailProject` (used today as BOTH `-g` and `--workspace`) splits into a constant instance project for `-g` and a per-worker subdir for `--workspace`; the per-worker `register-worker` fan-out collapses to a single `register-project`; the `list` fan-out collapses to one call. The Scion CLI seam (`internal/scion/{client,lifecycle}.go`) is already project-per-call and needs NO change — only the *values* the broker passes change. A config-time non-git guard and the Scion version-pin bump land here too.

**Tech Stack:** Go 1.26.4 (module `github.com/stevegeek/lever`), Cobra CLI, Scion CLI shell-out, mTLS capability broker. Unit tests use fake runtimes/runners; live single-project behavior is validated by an integration test here and the full `lever acceptance` gate in P4.

## Global Constraints

- **One Scion project per instance.** `-g` is a single constant = the jail mount root (`backend.MountDest()` = `/lever`), for the manager AND every worker. Workers differ only by `--workspace` (`/lever/<worker.Dir>`) and by their scion agent slug (`worker.Name`).
- **No structural regression of identity.** Per-agent enrolment/cert-CN/ticket/bootstrap stays per-worker (each worker still enrols separately via the broker with CN = its name). Only the *project* is shared.
- **Validation semantics preserved from P1.** The `dir:"."` rejection and both name-collision rules stay; P2 only rewrites the stale `config.go:485` comment reasoning and ADDS the non-git guard. Do not weaken any rule.
- **Green at every task boundary:** `go build ./...`, `go vet ./...`, `gofmt -l .` (empty), `go test ./...`. Unit tests use fakes; a task is not done if the module is red.
- **`.tool-versions` must contain `golang 1.26.4`** (asdf) — present at repo root.
- **Commits** end with the trailers:
  ```
  Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_016cLJM3rASJTCpvsep8BHi4
  ```
- **Clean break, teardown-assumed.** Prerelease; no migration from the multi-project layout. A full `lever destroy`→`up` re-bootstraps.
- **Live validation is deferred within P2** to Task 6 (integration test with a scripted fake) + P4's `lever acceptance`; the scion pin bump (Task 5) is a prerequisite for any *live* run but not for unit tests.

---

## Branch setup (coordinator, before Task 1 — not a task)

Cut the branch from current `main` (which has P1 merged at `a4a46db`): `git switch -c feat/p2-single-project`. Confirm clean tree + green baseline before Task 1. (The `stash@{0}` docs-site edits remain stashed and out of scope.)

---

## File Structure

Files changed (no new responsibilities; a couple of small new helpers):

- `internal/config/config.go` — add non-git guard + git-detection helper; rewrite the `:485` comment. (Task 1)
- `internal/config/gitguard.go` *(new)* — `treeInsideGitRepo(tree string) (root string, ok bool)` helper. (Task 1)
- `internal/broker/worker.go` — `WorkerSpec.JailProject` → `Workspace`; use `b.instanceProject` for `-g`; collapse `handleWorkerList`. (Task 2, 3)
- `internal/broker/broker.go` — `Broker`/`Config`: `managerProject` → `instanceProject` (the shared `-g`). (Task 2)
- `internal/broker/msg.go` — route via `b.instanceProject`. (Task 2)
- `internal/brokerctl/workerspecs.go` — `JailProject` → `Workspace` = `filepath.Join(jailMount, g.Dir)`. (Task 2)
- `internal/brokerctl/serve.go` — supply `cfg.InstanceProject = jailMount`. (Task 2)
- `internal/apply/plan.go` — drop the `register-worker` loop; `register-manager` → `register-project`. (Task 4)
- `internal/apply/run.go` — the register step arm (single project); worker-subdir pre-creation on dispatch is broker-side (Task 2), but confirm apply's register targets the one project. (Task 4)
- `lever.yaml`-loading + `scion.version` pin file (`go.mod`/vendor pin mechanism — locate in Task 5).
- Tests alongside each.

---

## Task 1: Config — non-git guard + git-detection helper + `:485` comment rewrite

Add a config-time guard that refuses a `tree` inside a git repo (R4's isolation targets non-git trees; per-worker git workflows are deferred, spec §13). Preserve all existing validation; rewrite only the now-stale `:485` comment.

**Files:**
- Create: `internal/config/gitguard.go`
- Modify: `internal/config/config.go` (call the guard in `Validate`/`Load`; rewrite `:484-489` comment)
- Test: `internal/config/gitguard_test.go`, and extend `internal/config/config_test.go`

**Interfaces:**
- Produces: `func treeInsideGitRepo(tree string) (gitRoot string, inside bool)` — walks from `tree` upward; returns the repo root and `true` if any ancestor (or `tree` itself) contains a `.git` entry (file or dir).

- [ ] **Step 1: Write the failing test for the helper**

`internal/config/gitguard_test.go`:
```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTreeInsideGitRepo(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if root, inside := treeInsideGitRepo(sub); !inside || root != repo {
		t.Fatalf("nested subdir: got (%q,%v), want (%q,true)", root, inside, repo)
	}
	if root, inside := treeInsideGitRepo(repo); !inside || root != repo {
		t.Fatalf("repo root: got (%q,%v), want (%q,true)", root, inside, repo)
	}
	plain := t.TempDir()
	if _, inside := treeInsideGitRepo(plain); inside {
		t.Fatalf("plain dir reported inside a git repo")
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `cd /Users/stephen/ai/lever_to && go test ./internal/config/ -run TestTreeInsideGitRepo -v`
Expected: FAIL (undefined `treeInsideGitRepo`).

- [ ] **Step 3: Implement the helper**

`internal/config/gitguard.go`:
```go
package config

import (
	"os"
	"path/filepath"
)

// treeInsideGitRepo walks upward from dir; if dir or any ancestor contains a
// `.git` entry (file or directory), it returns that repo root and true. lever's
// isolation model (R4) targets non-git trees — the guard keeps an operator from
// silently running the single-project model against a git working tree, whose
// per-worker git workflow is deferred (spec §13).
func treeInsideGitRepo(dir string) (string, bool) {
	d := filepath.Clean(dir)
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}
```

- [ ] **Step 4: Run the helper test — expect PASS**

Run: `go test ./internal/config/ -run TestTreeInsideGitRepo -v` → PASS.

- [ ] **Step 5: Write the failing config-validation test**

Add to `internal/config/config_test.go`:
```go
func TestValidateRejectsGitTree(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(root, "ws")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	a := &App{Name: "demo", Backend: "orbstack", Tree: tree}
	a.Broker.LLMAuth = "subscription"
	if err := a.validateNonGitTree(); err == nil {
		t.Fatalf("expected rejection: tree inside a git repo")
	}
}
```

- [ ] **Step 6: Run it — confirm it fails (undefined `validateNonGitTree`)**

Run: `go test ./internal/config/ -run TestValidateRejectsGitTree -v` → FAIL.

- [ ] **Step 7: Implement `validateNonGitTree` and call it from `Validate`**

In `internal/config/config.go`, add a method and call it inside `App.Validate` (after `Tree` is resolved absolute, near the existing Tree checks). `a.Tree` is already absolute (`config.go:412-415`).
```go
// validateNonGitTree refuses a tree that sits inside a git repository. R4's
// sibling isolation assumes a non-git tree; per-worker git workflows are
// deferred (spec §13). Once the pinned Scion carries the --workspace git guard
// a stray ancestor .git is harmless at mount time, but the model still targets
// non-git trees, so we fail loudly at config time rather than silently degrade.
func (a *App) validateNonGitTree() error {
	if root, inside := treeInsideGitRepo(a.Tree); inside {
		return fmt.Errorf("config: tree %q is inside a git repository (%s); lever targets non-git trees "+
			"(per-worker git workflows are deferred). Point tree at a non-git directory.", a.Tree, root)
	}
	return nil
}
```
Add `if err := a.validateNonGitTree(); err != nil { return err }` in `App.Validate` right after the `Tree` confinement check.

- [ ] **Step 8: Rewrite the stale `:485` comment**

Replace the `config.go:484-489` comment block (which justifies the `dir:"."` rejection via the now-deleted per-worker register step) with:
```go
		// A "." dir makes WorkerDir(g) == a.Tree, so the worker's workspace would
		// be the whole tree — mounting root defeats R4 sibling isolation (the
		// worker could read every sibling's subdir). Workers must occupy a strict
		// subdir of the shared instance project. (confinedRel rejects "." for
		// `tree` for the analogous root-is-the-mount reason.)
```
Keep the `return fmt.Errorf(...)` line and the rule itself unchanged.

- [ ] **Step 9: Run config tests — all pass**

Run: `go test ./internal/config/... -v 2>&1 | tail -30`
Expected: new tests PASS; the preserved rules' tests (`TestValidateRejectsWorkerDirDot`, the two collision tests) still PASS.

- [ ] **Step 10: Full green + commit**

```bash
go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test ./... 2>&1 | tail -8
git add -A && git commit -m "feat(config): non-git tree guard + git-detection helper; rewrite dir-dot comment (P2)
<trailers>"
```

---

## Task 2: Broker — split the overloaded project into constant `-g` + per-worker `--workspace`

Split `WorkerSpec.JailProject` (today both `-g` and `--workspace`) into a per-worker `Workspace` subdir and a broker-level constant `instanceProject` used for `-g` everywhere. Ensure the worker's host subdir exists before dispatch (spec §7).

**Files:**
- Modify: `internal/broker/worker.go` (`WorkerSpec`, `handleWorkerStart`, `phaseOf`, Resume/Stop/Suspend/EnvSet)
- Modify: `internal/broker/broker.go` (`Config`/`Broker`: `managerProject` → `instanceProject`; `New`)
- Modify: `internal/broker/msg.go` (routing project → `b.instanceProject`)
- Modify: `internal/brokerctl/workerspecs.go` (`JailProject` → `Workspace`; add `HostWorkspace`)
- Modify: `internal/brokerctl/serve.go` (`cfg.InstanceProject = jailMount`)
- Test: `internal/broker/worker_test.go`, `internal/broker/msg_test.go`, `internal/brokerctl/workerspecs_test.go`

**Interfaces:**
- `WorkerSpec` (`worker.go:42-48`): replace `JailProject string` with `Workspace string` (jail path `/lever/<dir>`, the `--workspace`) and add `HostWorkspace string` (host path `<tree>/<dir>`, ensured to exist before start). Keep `Name`, `BootstrapDir`, `Image`, `APIKey`.
- `Config`/`Broker`: add `InstanceProject string` (the constant `-g` = jail mount root). It replaces the per-agent role of `ManagerProject`; keep `ManagerSlug` (manager identity routing).

- [ ] **Step 1: Update the broker structs (write the new shape)**

In `internal/broker/broker.go`:
- `Config`: rename `ManagerProject` → `InstanceProject` (doc: "the single Scion project (-g) that the manager and all workers are agents in; = the jail mount root"). Keep `ManagerSlug`.
- `Broker`: rename field `managerProject` → `instanceProject`; set it in `New` from `c.InstanceProject`.

In `internal/broker/worker.go` `WorkerSpec`: replace `JailProject` with `Workspace` + `HostWorkspace` (see Interfaces).

- [ ] **Step 2: Update `handleWorkerStart` StartOpts + ensure host subdir**

`internal/broker/worker.go` (~177-181):
```go
	if err := os.MkdirAll(spec.HostWorkspace, 0o755); err != nil {
		b.audit("worker", b.manager, "error", "workspace dir: "+err.Error())
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	if err := b.runtime.Start(ctx, scion.StartOpts{
		Worker: spec.Name, Task: req.Task, Harness: "claude",
		Project: b.instanceProject, Workspace: spec.Workspace,
		Image: spec.Image, APIKey: spec.APIKey,
	}); err != nil {
```
(Add `"os"` to imports.)

- [ ] **Step 3: Update the remaining per-worker project calls**

Replace `spec.JailProject` / `s.JailProject` with `b.instanceProject` at: `phaseOf` List (`worker.go:117`), Resume in start-exists path (`:152`), EnvSet (`:172`), Stop (`:214`), Suspend (`:217`), Resume verb (`:220`). The worker is still identified by `spec.Name`/`s.Name` (slug) within the shared project.

- [ ] **Step 4: Update msg routing**

`internal/broker/msg.go` (`:63,79,88`): `spec.JailProject` → `b.instanceProject` (routing is now `agent:<slug>` within the one project).

- [ ] **Step 5: Update brokerctl spec construction**

`internal/brokerctl/workerspecs.go` (~17-24):
```go
		specs = append(specs, broker.WorkerSpec{
			Name:          g.Name,
			Workspace:     filepath.Join(jailMount, g.Dir),      // /lever/<dir> — the --workspace
			HostWorkspace: filepath.Join(app.Tree, g.Dir),       // <tree>/<dir> — ensured to exist before start
			BootstrapDir:  filepath.Join(app.Tree, g.Dir, ".lever"),
			Image:         app.WorkerImage(g),
			APIKey:        app.EffectiveWorkerLLMAuth(g) == config.LLMAuthAPIKey,
		})
```
`internal/brokerctl/serve.go`: change `cfg.ManagerProject = jailMount` → `cfg.InstanceProject = jailMount` (`:88`).

- [ ] **Step 6: Update the tests to the new model**

In `internal/broker/worker_test.go` / `msg_test.go` / `internal/brokerctl/workerspecs_test.go`: set `WorkerSpec{Workspace, HostWorkspace, ...}` instead of `JailProject`; assert `Start` receives `Project == <instanceProject>` and `Workspace == /lever/<dir>` (not equal to each other anymore); assert Resume/Stop/Suspend/List are called with the constant instance project. Use a temp dir for `HostWorkspace` so `MkdirAll` succeeds; assert the dir exists after a start. Confirm the message-routing tests use the instance project.

- [ ] **Step 7: Green + commit**

```bash
go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test ./internal/broker/... ./internal/brokerctl/... 2>&1 | tail -12
go test ./... 2>&1 | tail -6
git add -A && git commit -m "feat(broker): one instance project (-g) + per-worker subdir workspace (P2)
<trailers>"
```

---

## Task 3: Broker — collapse the `list` fan-out to a single call

`handleWorkerList` currently runs one `scion list` per worker project and concatenates. With one project, a single `list -g <instanceProject>` returns the whole fleet.

**Files:**
- Modify: `internal/broker/worker.go` (`handleWorkerList`, ~242-254)
- Test: `internal/broker/worker_test.go`

- [ ] **Step 1: Update the list test first**

In `internal/broker/worker_test.go`, adjust the list test so the fake runtime returns all agents from ONE `List(instanceProject)` call, and assert the handler calls `List` exactly once (not once-per-worker) and returns all agents. Add a counter to the fake runtime's `List` if not present.

- [ ] **Step 2: Run it — confirm it fails against the old fan-out**

Run: `go test ./internal/broker/ -run TestWorkerList -v` → FAIL (old code calls List N times).

- [ ] **Step 3: Replace the fan-out**

`internal/broker/worker.go` `handleWorkerList`:
```go
	agents, err := b.runtime.List(r.Context(), b.instanceProject)
	if err != nil {
		http.Error(w, "runtime error", http.StatusBadGateway)
		return
	}
	writeJSON(w, struct {
		Agents []scion.Agent `json:"agents"`
	}{Agents: agents})
```

- [ ] **Step 4: Green + commit**

```bash
go test ./internal/broker/... 2>&1 | tail -8 && go build ./... && go test ./... 2>&1 | tail -6
git add -A && git commit -m "feat(broker): collapse worker list to a single instance-project query (P2)
<trailers>"
```

---

## Task 4: Apply — collapse registration to one instance project (`register-project`)

Drop the per-worker `register-worker` fan-out; the single `register-manager` (which registers `/lever`) becomes `register-project` — the sole `scion init`/`hub link`. The register step's arm already keys everything on the translated jail path; with only one step it registers `/lever` once.

**Files:**
- Modify: `internal/apply/plan.go` (drop the worker loop at `:69-71`; rename `register-manager`→`register-project` at `:68`; update the `Kind` enum comment `:11`)
- Modify: `internal/apply/run.go` (rename the case `"register-manager"`→`"register-project"` at `:217`; the body is unchanged — it already operates on the one translated `jp`)
- Test: `internal/apply/plan_test.go`, `internal/apply/run_test.go`, `internal/apply/integration_test.go`

**Interfaces:** the plan `Kind` set loses `register-worker`; `register-manager` becomes `register-project`.

- [ ] **Step 1: Update plan_test expectations first**

In `internal/apply/plan_test.go`: the `want` step list becomes `... "scion-server", "register-project", "mint-manager-bootstrap", "start-manager"` (ONE registration, no per-worker entries); update the `banned` list (`register-worker` no longer emitted; add it to `banned` if the test asserts non-brokeronly kinds); drop the two-`register-worker`-at-indices-7/8 assertions. Update any index arithmetic in comments.

- [ ] **Step 2: Run it — confirm it fails**

Run: `go test ./internal/apply/ -run TestPlan -v` → FAIL (old plan still emits register-worker per worker).

- [ ] **Step 3: Collapse the plan builder**

`internal/apply/plan.go`: replace the `:68-71` block:
```go
	steps = append(steps, Step{Kind: "register-project", Target: a.Tree})
```
(Remove the `for _, g := range a.Workers { ... register-worker ... }` loop entirely.) Update the `Kind` enum comment at `:11`: `... | credential | register-project | mint-manager-bootstrap | start-manager` (drop `register-manager`/`register-worker`). Keep `brokerOnlyKinds` correct.

- [ ] **Step 4: Update the executor case**

`internal/apply/run.go:217`: `case "register-manager", "register-worker":` → `case "register-project":`. The body (`:218-278`) is unchanged — it observes-then-registers the single translated `jp` (= `/lever`).

- [ ] **Step 5: Update run_test / integration_test**

`internal/apply/run_test.go` + `integration_test.go`: any test dispatching or asserting `register-manager`/`register-worker` steps → `register-project` (single). Ensure `ScionProjectRegistered`/`RemoveScionProjectConfigs` fakes are now invoked once for `/lever` (not per worker). Update comments referencing the old step names.

- [ ] **Step 6: Green + commit**

```bash
go test ./internal/apply/... 2>&1 | tail -12 && go build ./... && go test ./... 2>&1 | tail -6
git add -A && git commit -m "feat(apply): one register-project step replaces register-manager + N register-worker (P2)
<trailers>"
```

---

## Task 5: Bump the Scion version pin to a commit carrying both fixes

Big-picture item A (spec §5): the pinned Scion must contain `fix/explicit-workspace-git-guard` (the `--workspace` git guard, now `e7f0ac7f` on `origin/stevegeek`) AND `fix/listprojects-cursor-pagination`. The current pin `666333f9` has neither.

**Files:**
- Modify: the `scion.version` pin (locate the mechanism in Step 1 — likely `go.mod` `replace`/`require` on the scion module, or a `scion.version` field consumed by the cross-compile; per memory, lever pins a scion commit via `scion.version` → `go mod download` + cross-compile).
- Verify: presence of both fixes in the pinned tree.

- [ ] **Step 1: Locate the pin mechanism**

```bash
cd /Users/stephen/ai/lever_to
grep -rn "scion.version\|GoogleCloudPlatform/scion\|stevegeek/scion" go.mod go.sum Makefile* *.mk 2>/dev/null | head
git grep -n "scion.version\|scion_version\|SCION_VERSION" -- ':!docs' ':!*.md' | head
```
Identify exactly where the commit is pinned and how the scion binary is built from it.

- [ ] **Step 2: Determine a fork commit that has BOTH fixes**

The two fixes are on separate fork branches. A single pin commit must contain both. Options (pick per what Step 1 reveals + repo access):
  (a) if a combined branch/commit already exists on `stevegeek/scion`, pin to it;
  (b) otherwise a combined commit must be produced on the fork (merge both branches) and pushed — **this may require Stephen** (push access to `git@github.com:stevegeek/scion`). If so, STOP and surface it as a blocker with the exact branches to combine; do not fabricate a pin.

- [ ] **Step 3: Update the pin + rebuild**

Set `scion.version` to the chosen commit; run the scion fetch/build the repo uses (from Step 1). Expected: build succeeds against the new pin.

- [ ] **Step 4: Verify both fixes are present in the pinned tree**

```bash
# In the downloaded/pinned scion source:
grep -rn "detectRepoRoot\|explicitWorkspace\|ExplicitWorkspace" <scion-src>/pkg/agent/run.go   # git guard present
grep -n "Cursor" <scion-src>/pkg/store/entadapter/project_store.go                              # pagination present
```
Both must be present.

- [ ] **Step 5: Green + commit**

```bash
go build ./... && go test ./... 2>&1 | tail -6
git add -A && git commit -m "build: pin Scion to a commit carrying the --workspace git guard + ListProjects pagination (P2)
<trailers>"
```

---

## Task 6: Integration test — single-project dispatch over a scripted fake

Prove the collapsed model end-to-end at the seam boundary with a scripted fake runner (no live VM): the manager and two workers all resolve to `-g /lever`, workers get distinct `--workspace /lever/<dir>`, `list` returns the whole fleet from one call, and registration runs once.

**Files:**
- Create/extend: `internal/apply/integration_test.go` or `internal/broker/*_test.go` — a focused test asserting the argv/opts the fake runner receives across a manager start + two worker starts + a list.

- [ ] **Step 1: Write the integration test**

Drive `apply.Plan` + the register/start path (with fake `Deps`) and the broker worker handlers (with a fake `WorkerRuntime`) for a config with two workers (`dir: workers/a`, `dir: workers/b`). Assert:
  - exactly ONE `register-project` step (Target `a.Tree`); `ScionProjectRegistered`/`InitProject`/`HubLink` invoked once for `/lever`.
  - worker A start opts: `Project == "/lever"`, `Workspace == "/lever/workers/a"`; worker B: `Workspace == "/lever/workers/b"`; both `-g /lever`.
  - `list` calls the runtime once with `/lever` and returns both agents.
  - each worker's `HostWorkspace` dir was created.

- [ ] **Step 2: Run it — PASS after Tasks 2–4**

Run: `go test ./internal/... -run 'SingleProject|InstanceProject' -v 2>&1 | tail -30` → PASS.

- [ ] **Step 3: Green + commit**

```bash
go test ./... 2>&1 | tail -6
git add -A && git commit -m "test: single-project dispatch integration (one -g, per-worker subdir, one list) (P2)
<trailers>"
```

---

## P2 acceptance criteria

- `go build ./... && go vet ./... && go test ./...` green; `gofmt -l` empty.
- Broker: `-g` is the constant instance project for manager + all workers; `--workspace` is the per-worker subdir; `list` is one call. No `JailProject` field remains.
- Apply: exactly one `register-project` step; no `register-worker`; registration runs once for `/lever`.
- Config: a `tree` inside a git repo is refused; the `dir:"."` + both collision rules still fire (reworded comment only).
- Scion pin contains both required fixes (or Task 5 is surfaced as a Stephen-blocker with the exact combine step).
- Integration test proves the single-project dispatch shape.
- Live single-project behavior (agents genuinely coexist in one project on a real hub) is validated by P4's `lever acceptance` — noted, not required here.

## Self-review notes (coordinator)

- **Spec coverage:** §3 collapse table rows → Tasks 2 (broker `-g`/JailProject), 3 (list fan-out), 4 (register-per-worker). §4 target model (subdir workspaces) → Task 2. §11 config (rule preserved + `:485` rewrite) + big-picture C (non-git guard) → Task 1. Big-picture A (pin) → Task 5. §7 worker-subdir-exists → Task 2 (Step 2, `MkdirAll` on dispatch).
- **Deferred to P3/P4 (not P2):** the throwaway dev-auth server + controller PAT + dev-auth-off (P3); messaging/reconciliation refinements + the `lever acceptance` §12 checks + live isolation validation (P4). P2 stays bootable on the current dev-auth-open loopback hub (spec, corrected).
- **Risk flags:** (1) Task 5 pin may need Stephen (fork push) — surfaced, not fabricated. (2) The non-git guard REFUSES; if a real instance tree turns out to be git, revisit (warn/override) during P2 live validation — noted here so it's a conscious decision, not a surprise. (3) Whether scion creates one project-configs entry per agent (workspace_path = each agent's `--workspace`) vs one per project affects the `scionProjectRegistered(/lever)` "exactly one entry" check; unit tests use fakes, so this is a LIVE-validation item (Task 6 asserts the lever-side shape; P4 acceptance confirms real scion behavior).
