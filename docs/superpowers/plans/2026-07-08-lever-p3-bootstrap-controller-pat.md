# Lever P3 — bootstrap + controller PAT + dev-auth-off Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the real Scion hub with dev-auth OFF, driven by a minted, persisted **controller PAT** — replacing today's reliance on open loopback dev-auth. Bring-up mints the PAT via a throwaway dev-auth server during an agent-free window, then starts the real hub locked down.

**Architecture:** Phase 3 of the re-architecture (spec `docs/superpowers/specs/2026-07-08-lever-single-project-rearchitecture-design.md` §6 + big-picture corrections in `45ca8a6`). Today: `scion server start` runs dev-auth default-ON and every lever scion client passes no token (`internal/scion/client.go`), so the hub is admin-open on loopback. P3 adds: a new `bootstrap-token` apply step that starts a **throwaway** `scion server start --port <random> --dev-auth=true` in the jail, does `project init` + `hub link` + mints a controller PAT (`scion hub token create --scopes agent:manage,agent:attach,project:read`), persists it `0600` under `.lever-state/`, then kills the throwaway and deletes the residual `~/.scion/dev-token`; the `scion-server` step then starts the real hub `--port 8080 --dev-auth=false`; and every subsequent scion verb carries the PAT via `SCION_HUB_TOKEN`. The throwaway and real server share the jail's `~/.scion` DB **by construction** (same jail home), so the minted project + PAT carry over. `agent:message` is intentionally omitted from the scope list (every interactive verb, message included, gates on `agent:attach`).

**Tech Stack:** Go 1.26.4, Scion CLI shell-out (`internal/scion`), `.lever-state/` host persistence (`internal/brokerctl`), jail exec (`internal/jail`). Unit tests use fake `exec.Runner`. **Live scion-CLI behavior (does the pinned scion support `--port`, `--dev-auth=false`, `server stop`, `hub token create --scopes`) is validated in P4 / on a live run — flagged, not assumed.**

## Global Constraints

- **Controller PAT scope is exactly `agent:manage,agent:attach,project:read`** — no `agent:message` (verified in the scion authz review: attach gates message too).
- **The real hub runs `--dev-auth=false`.** The throwaway runs `--dev-auth=true` on a **random** port, host/jail-only, and is dead before any agent container exists ("agent-free window").
- **PAT persists in `.lever-state/`** (survives `down`→`up`; `clearStagedRuntimeState` only wipes `tree/.lever/*`). Re-mint only when absent/invalid (spec §8). A mid-life 403 fails loud → operator does `stop`→`up`.
- **Shared DB is by construction** (both `scion server start` run in the same jail `~/.scion`); do not add a data-dir control point — just document the reliance.
- **Green at every task boundary:** `go build ./...`, `go vet ./...`, `gofmt -l .` (empty), `go test ./...`. Unit tests fake the scion runner.
- **Commits** end with:
  ```
  Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_016cLJM3rASJTCpvsep8BHi4
  ```
- **`.tool-versions`** pins `golang 1.26.4` (asdf) — present.
- **Live-validation items (P4, not P3):** scion CLI flag support (`--port`/`--dev-auth`/`server stop`/`hub token create`); the throwaway→real hub-link endpoint reconciliation (bootstrap-token links to the random port; `register-project` reconciles to `:8080` — confirm the observe-gate re-links, or make register-project force a re-link); real dev-auth-off end-to-end.

---

## Branch setup (coordinator, before Task 1 — not a task)

`git switch -c feat/p3-controller-pat` off current `main` (P2 merged). Confirm clean tree + green baseline.

---

## File Structure

- `internal/scion/bringup.go` — `ServerStart` gains port + dev-auth options; add `ServerStop`. (Task 1)
- `internal/scion/hubtoken.go` *(new)* — `HubTokenCreate(ctx, scopes) (string, error)`. (Task 1)
- `internal/scion/client.go` — `Options.HubToken`/`HubTokenSource` + `env()` injection. (Task 2)
- `internal/scion/lifecycle.go` + `internal/jail/attach.go` — carry `SCION_HUB_TOKEN` into the attach exec path. (Task 2)
- `internal/brokerctl/keys.go` — `State.ControllerPAT()` accessor + write/read helpers. (Task 3)
- `internal/apply/plan.go` + `internal/apply/run.go` — `bootstrap-token` step + `scion-server` dev-auth-off; new `Deps` fields. (Task 4)
- `internal/cli/apply.go` — wire the throwaway/mint closures + thread the PAT source into all 5 `scion.New` sites. (Task 5)
- Tests alongside each; integration test (Task 6).

---

## Task 1: Scion verbs — server port/dev-auth, server stop, hub token create

Add the three missing scion CLI wrappers. All build argv + `env()` and delegate to the injected runner (unit-testable with a fake), matching `internal/scion/bringup.go`/`project.go` style.

**Files:**
- Modify: `internal/scion/bringup.go` (`ServerStart` → options; add `ServerStop`)
- Create: `internal/scion/hubtoken.go`
- Test: `internal/scion/bringup_test.go`, `internal/scion/hubtoken_test.go`

**Interfaces:**
- `ServerStart(ctx, opts ServerOpts) error` where `type ServerOpts struct { Port int; DevAuth bool }`. Argv: `server start` + `--port <n>` (if Port>0) + `--dev-auth=<true|false>` (always emitted so the real hub is explicitly locked). Keep the `AlreadyRunning` tolerance + `waitHubReady`.
- `ServerStop(ctx) error` → `scion server stop` (tolerate not-running). NOTE (live): if the pinned scion lacks `server stop`, Task 4's throwaway kill falls back to a jail-process kill; keep `ServerStop` as the seam.
- `HubTokenCreate(ctx, scopes []string) (string, error)` → `scion hub token create --scopes <csv>`; parse the token from stdout (trimmed; if JSON, decode `{"token":...}` — inspect real output in a live spike or accept both: try JSON, else trimmed line). Returns the opaque PAT string.

- [ ] **Step 1: Write failing tests**

`internal/scion/bringup_test.go` — assert `ServerStart(ctx, ServerOpts{Port: 41000, DevAuth: false})` runs argv `["server","start","--port","41000","--dev-auth=false"]` (via a fake runner capturing argv), and `ServerStart(ctx, ServerOpts{DevAuth: true})` → `["server","start","--dev-auth=true"]` (no port). `ServerStop` → `["server","stop"]`.
`internal/scion/hubtoken_test.go` — `HubTokenCreate(ctx, []string{"agent:manage","agent:attach","project:read"})` runs `["hub","token","create","--scopes","agent:manage,agent:attach,project:read"]` and returns the fake runner's stdout token (test both a bare-token stdout and a `{"token":"..."}` stdout).

- [ ] **Step 2: Run — confirm fail**

Run: `cd /Users/stephen/ai/lever_to && go test ./internal/scion/ -run 'ServerStart|ServerStop|HubTokenCreate' -v` → FAIL (undefined / wrong argv).

- [ ] **Step 3: Implement**

`bringup.go` — change `ServerStart`:
```go
type ServerOpts struct {
	Port    int
	DevAuth bool
}

func (c *Client) ServerStart(ctx context.Context, o ServerOpts) error {
	args := []string{"server", "start"}
	if o.Port > 0 {
		args = append(args, "--port", strconv.Itoa(o.Port))
	}
	args = append(args, fmt.Sprintf("--dev-auth=%t", o.DevAuth))
	if _, err := c.run(ctx, "", args...); err != nil && !AlreadyRunning(err) {
		return err
	}
	return c.waitHubReady(ctx)
}

func (c *Client) ServerStop(ctx context.Context) error {
	if _, err := c.run(ctx, "", "server", "stop"); err != nil && !AlreadyRunning(err) {
		return err
	}
	return nil
}
```
`hubtoken.go`:
```go
package scion

import (
	"context"
	"encoding/json"
	"strings"
)

// HubTokenCreate mints a personal access token with the given scopes against the
// current hub endpoint (dev-auth admin), returning the opaque token string.
func (c *Client) HubTokenCreate(ctx context.Context, scopes []string) (string, error) {
	out, err := c.run(ctx, "", "hub", "token", "create", "--scopes", strings.Join(scopes, ","))
	if err != nil {
		return "", err
	}
	// Accept either a bare token line or {"token":"..."} JSON.
	var j struct {
		Token string `json:"token"`
	}
	if perr := parseJSON(out, &j); perr == nil && j.Token != "" {
		return j.Token, nil
	}
	return strings.TrimSpace(out), nil
}
```
(Add `strconv` import to bringup.go. Update the ONE existing `ServerStart` call site — `internal/apply/run.go:205` `scion-server` arm — in Task 4; for now update it to `ServerStart(ctx, ServerOpts{Port: 8080, DevAuth: false})` so the build stays green, and Task 4 refines the wiring.)

- [ ] **Step 4: Run — PASS**; **Step 5: full green + commit**

```bash
go test ./internal/scion/... && go build ./... && go test ./...
git add -A && git commit -m "feat(scion): server --port/--dev-auth opts, server stop, hub token create (P3)
<trailers>"
```

---

## Task 2: Client `SCION_HUB_TOKEN` injection (incl. the attach path)

Thread the controller PAT into the scion client env, and — because the attach/TTY path bypasses `Client.env()` — into the attach exec argv too.

**Files:**
- Modify: `internal/scion/client.go` (`Options`, `Client`, `New`, `env()`)
- Modify: `internal/scion/lifecycle.go` (`AttachArgv` carries the token) + `internal/jail/attach.go` if needed
- Test: `internal/scion/client_test.go`, `internal/scion/lifecycle_test.go`

**Interfaces:**
- `Options` gains `HubToken string` AND `HubTokenSource func() string` (lazy — read at call time, for the mint-mid-apply case). `env()` prefers the source if set, else the static value; emits `SCION_HUB_TOKEN` when non-empty.
- `AttachArgv(worker, project string) []string` → `AttachArgv(worker, project, hubToken string) []string` (or read from the client) — the returned argv must embed `SCION_HUB_TOKEN=<pat>` so `syscall.Exec`'d attach authenticates.

- [ ] **Step 1: Write failing tests**

`client_test.go` — a client built with `Options{HubToken: "pat123"}` emits `SCION_HUB_TOKEN=pat123` in `env()`; with `HubTokenSource: func() string { return "dyn" }` emits `SCION_HUB_TOKEN=dyn`; with neither, no `SCION_HUB_TOKEN` key. `lifecycle_test.go` — `AttachArgv` includes `SCION_HUB_TOKEN=<pat>` in the exec argv when a token is present.

- [ ] **Step 2: Run — fail. Step 3: implement.**

`client.go`:
```go
type Options struct {
	Bin            string
	HubEndpoint    string
	DevToken       string
	HubToken       string        // SCION_HUB_TOKEN (static)
	HubTokenSource func() string // SCION_HUB_TOKEN (lazy; wins over HubToken)
}
```
Add `hubToken string` + `hubTokenSource func() string` to `Client`; thread in `New`; in `env()` after the devToken block:
```go
	if tok := c.currentHubToken(); tok != "" {
		m["SCION_HUB_TOKEN"] = tok
	}
```
```go
func (c *Client) currentHubToken() string {
	if c.hubTokenSource != nil {
		return c.hubTokenSource()
	}
	return c.hubToken
}
```
Add an exported accessor `func (c *Client) HubToken() string { return c.currentHubToken() }` so callers (attach) can embed it. In `lifecycle.go` `AttachArgv`, append `"env", "SCION_HUB_TOKEN="+tok` into the exec argv when non-empty (match how the jail env is already embedded for attach — see `internal/jail/attach.go`). Update the attach call site (`internal/cli/attach.go`) in Task 5 to pass the token.

- [ ] **Step 4: PASS; Step 5: green + commit**

```bash
go test ./internal/scion/... ./internal/jail/... && go build ./... && go test ./...
git add -A && git commit -m "feat(scion): SCION_HUB_TOKEN client env + attach-path injection (P3)
<trailers>"
```

---

## Task 3: Persist the controller PAT under `.lever-state/`

Mirror the `keys.go` write / `build.go` read-with-perm-check pattern.

**Files:**
- Modify: `internal/brokerctl/keys.go` (add `State.ControllerPAT()` + `SaveControllerPAT`/`LoadControllerPAT`)
- Test: `internal/brokerctl/keys_test.go`

**Interfaces:**
- `func (s State) ControllerPAT() string` → `filepath.Join(s.Dir, "controller.pat")`.
- `func (s State) SaveControllerPAT(tok string) error` → `MkdirAll(s.Dir,0700)` then `os.WriteFile(path, []byte(tok), 0600)`.
- `func (s State) LoadControllerPAT() (string, error)` → read; enforce `Perm()==0600`; `TrimSpace`; empty-or-absent → `("", nil)` so callers can branch on "" = need-to-mint.

- [ ] **Step 1: failing test** — write a PAT, load it back (perm 0600 asserted); absent → `("", nil)`; wrong perms → error. **Step 2: run/fail. Step 3: implement per Interfaces. Step 4: PASS.**

- [ ] **Step 5: green + commit**

```bash
go test ./internal/brokerctl/... && go build ./... && go test ./...
git add -A && git commit -m "feat(brokerctl): persist controller PAT 0600 under .lever-state (P3)
<trailers>"
```

---

## Task 4: `bootstrap-token` apply step + real-hub dev-auth-off

Insert the mint window into the plan and executor; lock the real hub.

**Files:**
- Modify: `internal/apply/plan.go` (new `bootstrap-token` step between `config-registry` and `scion-server`; Kind enum comment)
- Modify: `internal/apply/run.go` (`Deps` gains the throwaway/mint funcs; `runStep` arm; `scion-server` arm → `ServerStart(ServerOpts{Port:8080,DevAuth:false})`)
- Test: `internal/apply/plan_test.go`, `internal/apply/run_test.go`

**Interfaces (new `Deps` fields):**
- `EnsureControllerPAT func(ctx) error` — the whole mint window as one injected op (keeps `run.go` scion-agnostic; the CLI wires the real logic in Task 5). It must be **idempotent**: if a valid PAT is already persisted, no-op; else run the throwaway → mint → persist → kill → delete-dev-token.

- [ ] **Step 1: plan_test first** — `want` becomes `... "config-registry", "bootstrap-token", "scion-server", ...`; assert `bootstrap-token` present exactly once, ordered before `scion-server`; `brokerOnlyKinds` unchanged (excludes it).
- [ ] **Step 2: run/fail. Step 3: plan.go** — emit `Step{Kind:"bootstrap-token", Target: a.Tree}` between `config-registry` and `scion-server`; update the Kind enum comment.
- [ ] **Step 4: run.go** — add the arm:
```go
	case "bootstrap-token":
		if d.EnsureControllerPAT == nil {
			return nil // dev-auth-open mode (e.g. tests / legacy); skip
		}
		return d.EnsureControllerPAT(ctx)
```
Change the `scion-server` arm (`run.go:205`) to `d.Scion.ServerStart(ctx, scion.ServerOpts{Port: 8080, DevAuth: false})`. Add `EnsureControllerPAT func(context.Context) error` to `Deps`.
- [ ] **Step 5: run_test** — a fake `EnsureControllerPAT` counter asserts it's invoked once, before `scion-server`; assert `scion-server` starts with dev-auth off (fake scion records `ServerStart` opts). Keep existing apply tests green (they can leave `EnsureControllerPAT` nil → skip, preserving dev-auth-open behavior for unit tests).
- [ ] **Step 6: green + commit**

```bash
go test ./internal/apply/... && go build ./... && go test ./...
git add -A && git commit -m "feat(apply): bootstrap-token step + real hub dev-auth-off (P3)
<trailers>"
```

---

## Task 5: Wire the mint window + thread the PAT into every scion client

Implement `EnsureControllerPAT` in the CLI and give all five scion clients the PAT via a lazy source (so verbs after the mint pick it up).

**Files:**
- Modify: `internal/cli/apply.go` (the `EnsureControllerPAT` closure; add `HubTokenSource` to the shared `scion.New`)
- Modify: `internal/cli/attach.go`, `internal/cli/hostmsg.go`, `internal/cli/stop.go`, `internal/brokerctl/serve.go` (add `HubTokenSource` reading `state.ControllerPAT()`)
- Test: `internal/cli/apply_test.go` (or a focused test for the closure)

**Interfaces:**
- The PAT source is `func() string { tok, _ := state.LoadControllerPAT(); return tok }` — passed as `scion.Options.HubTokenSource` at all five sites. After `EnsureControllerPAT` persists the PAT, subsequent verbs read it live.

- [ ] **Step 1: `EnsureControllerPAT` closure in apply.go** (near `mintManagerBootstrap`):
```go
ensureControllerPAT := func(ctx context.Context) error {
	if tok, _ := state.LoadControllerPAT(); tok != "" {
		return nil // already minted; survives down→up
	}
	port, err := freeJailPort() // random high port for the throwaway (see below)
	if err != nil { return err }
	tw := scion.New(jr, scion.Options{HubEndpoint: fmt.Sprintf("http://127.0.0.1:%d", port)})
	if err := tw.ServerStart(ctx, scion.ServerOpts{Port: port, DevAuth: true}); err != nil {
		return fmt.Errorf("bootstrap-token: throwaway server: %w", err)
	}
	defer tw.ServerStop(ctx) // best-effort kill
	jp := jailPath(app.Tree, app.Tree, b.MountDest())
	if err := tw.InitProject(ctx, jp); err != nil { return err }
	if err := tw.HubLink(ctx, jp); err != nil { return err }
	pat, err := tw.HubTokenCreate(ctx, []string{"agent:manage", "agent:attach", "project:read"})
	if err != nil { return err }
	if err := state.SaveControllerPAT(pat); err != nil { return err }
	if err := tw.ServerStop(ctx); err != nil { /* log; also fall back to jail-pid kill if needed */ }
	_ = b.RemoveJailFile(ctx, "/home/scion/.scion/dev-token") // delete residual admin token (best-effort)
	return nil
}
```
(Choose the throwaway port: a fixed high port distinct from 8080 and the broker admin port is simplest and deterministic for now; a truly random free port needs a jail-side probe — a fixed non-8080 high port such as `48080` is acceptable since it's jail-internal and the server is killed immediately. Document the choice.) Add `EnsureControllerPAT: ensureControllerPAT` to the `Deps{...}` literal.

- [ ] **Step 2: Thread the PAT source** into the shared client (`apply.go:129`): `scion.New(jr, scion.Options{HubEndpoint: "http://127.0.0.1:8080", HubTokenSource: func() string { t, _ := state.LoadControllerPAT(); return t }})`. Do the same at `attach.go:73`, `hostmsg.go:50`, `stop.go:67`, `serve.go:85` (each already has `state`/can build it via `brokerctl.StateDir(filepath.Dir(path))`). For attach, also pass the token into `AttachArgv` (Task 2) so the exec path carries it.

- [ ] **Step 3: Test** — a unit test for `ensureControllerPAT` with a fake runner + temp state dir: first call mints (throwaway start/init/link/token-create/stop in order, PAT saved 0600, dev-token removal attempted); second call (PAT present) is a no-op (no throwaway start). Assert the scopes string. Keep it hermetic (fake `jr`).

- [ ] **Step 4: green + commit**

```bash
go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test ./...
git add -A && git commit -m "feat(cli): mint controller PAT via throwaway dev-auth; thread PAT into all scion clients (P3)
<trailers>"
```

---

## Task 6: Integration test — bootstrap-to-locked-hub flow

Prove the whole P3 shape with fakes: apply runs `bootstrap-token` (throwaway → init+link → mint `agent:manage,agent:attach,project:read` → persist → kill → dev-token delete) before `scion-server --dev-auth=false`, and subsequent scion verbs carry `SCION_HUB_TOKEN`; a re-apply with a persisted PAT skips the throwaway.

**Files:** extend `internal/apply/integration_test.go` (or `internal/cli/apply_test.go`).

- [ ] **Step 1:** drive `apply.Run` (or `runStep` over the plan) with a fake scion runner + fake `EnsureControllerPAT` bound to the real `ensureControllerPAT` over a fake `jr` + temp `.lever-state`. Assert: `bootstrap-token` precedes `scion-server`; `scion-server` opts = `{Port:8080, DevAuth:false}`; the PAT file exists `0600` with the minted value; a second `apply.Run` does NOT restart the throwaway (PAT reused). Assert the `env()` of the post-mint client contains `SCION_HUB_TOKEN`.
- [ ] **Step 2: PASS; Step 3: green + commit.**

---

## P3 acceptance criteria

- Green (`build`/`vet`/`gofmt`/`test`).
- New verbs: `ServerStart(port,devAuth)`, `ServerStop`, `HubTokenCreate(scopes)` with correct argv; scopes = `agent:manage,agent:attach,project:read` (no `agent:message`).
- `bootstrap-token` step present, ordered before `scion-server`; `scion-server` starts dev-auth-off; PAT minted → persisted `0600` in `.lever-state/` → threaded into all five scion clients (incl. attach); residual `~/.scion/dev-token` deletion attempted.
- Idempotent: a persisted valid PAT skips the throwaway (survives `down`→`up`).
- Integration test proves the flow with fakes.
- **Live items (P4):** scion CLI flag/verb support; throwaway→real hub-link endpoint reconciliation; real dev-auth-off end-to-end; `server stop` vs jail-pid-kill for the throwaway.

## Self-review notes (coordinator)

- **Spec coverage:** §6.1 throwaway→mint→persist→kill+dev-token-delete → Tasks 1,3,4,5. §6.2 SCION_HUB_TOKEN plumbing → Task 2,5. §6.3 revised apply order → Task 4. Big-picture D (shared DB) → free by construction (documented). Big-picture E (dev-token delete) → Task 5 Step 1. Big-picture F (drop `agent:message`) → Global Constraints + Task 1/5 scopes.
- **Design wrinkles resolved:** (1) mint-mid-apply → `HubTokenSource` lazy read; (2) attach bypasses `env()` → token also embedded in `AttachArgv`; (3) no server-stop guarantee → `ServerStop` seam with a jail-pid-kill fallback noted for live.
- **Parallelizable:** Tasks 1 (scion verbs), 2 (client env), 3 (persistence) are disjoint → run in parallel worktrees. Tasks 4+5 depend on 1–3 (sequential). Task 6 last.
- **Deferred to P4:** all live scion-behavior validation + the `lever acceptance` §12 gate. P3 is code-complete + unit-tested; it does not itself run a live hub.
