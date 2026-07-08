# Lever P1 — grove → worker rename + config schema Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rename lever's `grove` vocabulary to `worker` across code, config schema, wire surface, CLI, filesystem conventions, and user-facing docs — on the *current* grove-per-project structure, as the reviewed base for the single-project re-architecture.

**Architecture:** This is Phase 1 (P1) of the re-architecture in `docs/superpowers/specs/2026-07-08-lever-single-project-rearchitecture-design.md`. It is a vocabulary + schema rename only: it does **not** change lever's structure, isolation model, or validation *semantics*. The rename is split by tooling boundary — one repo-wide Go-identifier rename (compiler-verified), then a series of string-value renames (yaml keys, HTTP routes, CLI flags, apply step-names, filesystem paths, prose) that each keep the module building and all tests green.

**Tech Stack:** Go 1.26.4 (module `github.com/stevegeek/lever`), `gopls` for symbol renames, `git mv` for file/dir renames, Cobra CLI, YAML config, embedded Markdown skills, Jekyll docs-site.

## Global Constraints

- **Vocabulary map is exactly one token:** `grove`/`Grove`/`groves`/`Groves` → `worker`/`Worker`/`workers`/`Workers`. `GroveToGrove`/`grove_to_grove`/`grove→grove` → `WorkerToWorker`/`worker_to_worker`/`worker→worker`. No other renames.
- **Do NOT rename grove → project.** Scion's term is *Agent*; lever's user-facing term is *worker*. (Spec §10.)
- **Identity values are NOT changed.** Worker *names* (config `name:` values like `worker`, `todo`, `svc-a`) stay. The cert CN, ticket subject, and scion agent slug all equal the worker name and are unchanged in value. P1 changes the vocabulary token and the `groves/`→`workers/` path fragment only.
- **Validation SEMANTICS are preserved.** The `dir:"."` rejection, the name-vs-ManagerCN collision, and the name-vs-app-name collision rules stay in force (error strings reworded grove→worker). Their *removal* is coupled to the single-project model and is deferred to P2. Do not delete or weaken any validation rule in P1.
- **Structure is unchanged.** Each worker is still its own Scion project rooted at its subdir. Do not collapse projects, change `-g` scoping, or touch the register-per-worker apply logic beyond renaming the `register-grove` step string.
- **Clean break, teardown-assumed.** Lever is prerelease; no back-compat with `groves:` config is provided. P1 assumes a full `lever down`→`up` (no live migration), so runtime-value renames (`groves/` path fragment, HTTP routes rebuilt in lockstep) are safe.
- **Green at every task boundary.** After each task: `go build ./...`, `go vet ./...`, `gofmt -l` (empty), and `go test ./...` must all pass. A task that leaves the module red is not done.
- **`.tool-versions` must contain `golang 1.26.4`** in the working tree/worktree or `go` refuses to run (asdf). It is already present at repo root; if working in a git worktree, copy it in.
- **CHANGELOG.md history is immutable.** Do not rewrite past `grove` entries; add one new entry (Task 7). Historical docs (`docs/2026-07-04-followups-handover.md`, the spec/plan files) are excluded from the rename.

---

## Branch setup (coordinator, before Task 1 — not a task)

The working tree currently has two *unrelated* uncommitted edits (`docs-site/_guides/capabilities.md`, `docs-site/_reference/config.md`) from earlier broker doc-gap work. Land or set these aside first so P1 is clean:

1. Commit those two edits to `main` (or a separate small branch) on their own — they are not part of P1.
2. Cut the P1 branch from the resulting clean `main`: `git switch -c feat/p1-grove-to-worker`.
3. Confirm `git status` is clean and `go build ./... && go test ./...` is green on the fresh branch before dispatching Task 1.

---

## File Structure

No files change responsibility; several are renamed to match their renamed symbols. Files renamed via `git mv` (Task 1):

- `internal/broker/grove.go` → `internal/broker/worker.go`
- `internal/broker/grove_test.go` → `internal/broker/worker_test.go`
- `internal/brokerctl/grovespecs.go` → `internal/brokerctl/workerspecs.go`
- `internal/brokerctl/grovespecs_test.go` → `internal/brokerctl/workerspecs_test.go`
- `internal/cli/grove_client.go` → `internal/cli/worker_client.go`
- `internal/cli/grove_client_test.go` → `internal/cli/worker_client_test.go`

Example directory renamed (Task 6):
- `examples/hello-grove/` → `examples/hello-worker/` (and app `name: hello-grove` → `hello-worker`)

Worker subdir directories moved (Task 6): every `<example>/workspace/groves/<name>/` → `<example>/workspace/workers/<name>/`, and `internal/cli/testdata/acceptance/groves/worker/` → `.../workers/worker/`.

---

## Task 1: Rename all Go identifiers grove → worker (repo-wide, tool-assisted)

Rename every Go **symbol** (types, struct fields, methods, funcs, params, locals, test-function names) whose token is `grove`/`Grove`. Do **NOT** touch string literals, struct tags, route strings, CLI flag strings, or path literals — those are Tasks 2–7. This is one atomic, compiler-verified commit: the module builds and every test passes because all consumers of each renamed symbol move together, while the unchanged string values keep every test assertion valid.

**Files:** every `*.go` under `internal/` and `cmd/` that `git grep -l Grove` reports, plus the six `git mv` file renames listed in File Structure.

**Interfaces — exported symbol rename map (rename each via gopls; references update module-wide):**

Config package (`internal/config/config.go`, consumed everywhere):
- `Grove` (struct type) → `Worker`
- `App.Groves` (field) → `App.Workers`
- `Messaging.GroveToGrove` (field) → `Messaging.WorkerToWorker`
- `(*App).GroveDir` → `WorkerDir`
- `(*App).GroveImage` → `WorkerImage`
- `(*App).GroveByName` → `WorkerByName`
- `(*App).EffectiveGroveLLMAuth` → `EffectiveWorkerLLMAuth`
- `(*App).GroveToGroveMessaging` → `WorkerToWorkerMessaging`

Broker (`internal/broker/*.go`):
- `GroveRuntime` → `WorkerRuntime`; `GroveSpec` → `WorkerSpec`
- `Config.Runtime` type follows; `Config.Groves` → `Config.Workers`; `Config.GroveToGrove` → `Config.WorkerToWorker`
- `Broker.groves` → `Broker.workers`; `Broker.groveToGrove` → `Broker.workerToWorker`
- `groveBootstrap` → `workerBootstrap`; `groveStartRequest` → `workerStartRequest`; `groveResponse` → `workerResponse`; `groveSpec` (method) → `workerSpec`
- `requireManagerGrove` → `requireManagerWorker`; `groveVerb` → `workerVerb`
- `handleGroveStart|Stop|Suspend|Resume|List` → `handleWorkerStart|Stop|Suspend|Resume|List`
- `groveStartRequest.Grove` (field) → `.Worker`; `groveResponse` grove field → worker; `msgListRequest.Grove` → `.Worker`; `ProvisionRequest.Grove` → `.Worker`
- `msg.go` local `isGrove` → `isWorker`; `resolveListProject(caller, grove ...)` param → `worker`

Capability CA (`internal/cap/ca/ticket.go`):
- `ticket.grove` (field) → `ticket.worker`; `Issue(grove string ...)` param → `worker`; `Redeem(tk, grove string ...)` param → `worker`

brokerctl (`internal/brokerctl/`):
- `GroveSpecs` → `WorkerSpecs`; `groveBrokerURL` → `workerBrokerURL`

CLI (`internal/cli/`):
- `groveResult` → `workerResult`; `postGrove` → `postWorker`; `groveCall` → `workerCall`; `groveCallFn` → `workerCallFn`
- `msg.go` local `grove` → `worker` (var only; the `--grove` flag string is Task 4)

Scion (`internal/scion/lifecycle.go`):
- `StartOpts.Grove` (field) → `.Worker`; params `grove` in `Delete/Resume/Stop/Suspend/AttachArgv` → `worker`

Agent + cmd (`internal/agent/provision.go`, `cmd/lever-agent/main.go`):
- `agent.Provision`/`BootstrapFor` params `grove` → `worker` (the POST-body key `"grove"` is Task 3; the `-grove` flag string is Task 4)
- `cmd/lever-agent/main.go` local `grove` → `worker` var

Unexported helpers, remaining locals, and **test-function names** containing the token (e.g. `TestValidateRejectsGroveDirDot`, `TestGroveImageFallsBackToManagerImage`, `TestProvisionMintsGroveTicket`, `TestApplyLiveHelloGrove`, `TestGroveCall_postsAndDecodes`, `TestHostMsgSendToGroveWithInterrupt`, `TestPlanLoadsDistinctGroveImages`, `TestSecurityImagePolicyAppliesToGroves`, `TestEffectiveLLMAuthGroveOverride`, `TestGroveToGroveMessagingDefaultsTrue`) → rename the token. Test names are unreferenced; rename them manually or via gopls.

- [ ] **Step 1: Verify a clean green base**

```bash
cd /Users/stephen/ai/lever_to
go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test ./... 2>&1 | tail -20
```
Expected: build/vet clean, gofmt empty, all packages `ok`.

- [ ] **Step 2: Enumerate the Go symbol sites**

```bash
git grep -n -i grove -- '*.go' | wc -l   # baseline count to drive to ~0 after Tasks 1–7
gopls version                            # confirm gopls available; if not: go install golang.org/x/tools/gopls@latest
```

- [ ] **Step 3: Rename exported and unexported symbols via gopls**

For each symbol in the map, locate one definition site and rename module-wide. Pattern:

```bash
# Example: rename the Grove type from its definition in config.go
gopls rename -w internal/config/config.go:247:6 Worker
# Example: rename the App.Groves field
gopls rename -w internal/config/config.go:287:3 Workers
```

Work definition-first (types before the fields/methods that mention them is not required — gopls resolves each symbol independently). After every few renames, run `go build ./...` to catch any site gopls could not reach (e.g. names only in `_test.go` build-tagged files) and fix by re-targeting. Struct **tags** and **string literals** must remain untouched — verify with `git diff` that changed lines are identifier tokens only.

- [ ] **Step 4: Rename the six Go files**

```bash
git mv internal/broker/grove.go internal/broker/worker.go
git mv internal/broker/grove_test.go internal/broker/worker_test.go
git mv internal/brokerctl/grovespecs.go internal/brokerctl/workerspecs.go
git mv internal/brokerctl/grovespecs_test.go internal/brokerctl/workerspecs_test.go
git mv internal/cli/grove_client.go internal/cli/worker_client.go
git mv internal/cli/grove_client_test.go internal/cli/worker_client_test.go
```

- [ ] **Step 5: Rename remaining test-function names and stray locals by hand**

```bash
git grep -n -i grove -- '*.go'
```
For every remaining hit, confirm it is one of: (a) an identifier not reachable by gopls (test func name, local) → rename the token; or (b) a **string literal / struct tag / route / path / flag** → LEAVE IT (Tasks 2–7). Do not change category (b) in this task.

- [ ] **Step 6: Verify green**

```bash
go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test ./... 2>&1 | tail -30
```
Expected: build/vet clean, gofmt empty, all `ok`. (String-valued tests still pass: producers and consumers of `groves:`/`/grove/*`/paths are unchanged in this task.)

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "refactor: rename grove→worker Go identifiers (P1, no behavior change)"
```

---

## Task 2: Config schema keys + validation strings + yaml fixtures

Flip the config **wire keys** and reword grove-named validation error strings, updating every YAML fixture in lockstep so tests stay green. Validation *logic* is unchanged.

**Files:**
- Modify: `internal/config/config.go` (struct tags + error strings)
- Modify: `internal/config/config_test.go` (fixture YAML + assertion strings)
- Modify: `internal/config/examples_test.go` (failure string only; the `hello-grove` path is Task 6)
- Modify: every in-repo `lever.yaml` that uses `groves:`/`grove_to_grove:` — enumerate in Step 1. (Their `dir:` path values are handled here as a key/value edit; the on-disk directory moves are Task 6. To keep Task 2 green in isolation, change the key `groves:`→`workers:` but leave `dir:` values as `groves/<name>` for now; Task 6 moves the dirs and updates the `dir:` values together.)

**Interfaces — key + string map:**
- Struct tag `yaml:"groves"` → `yaml:"workers"` (config.go, `App.Workers` field)
- Struct tag `yaml:"grove_to_grove"` → `yaml:"worker_to_worker"` (config.go, `Messaging.WorkerToWorker` field)
- Validation error strings (config.go `App.Validate` + `validateBroker` + `validateBrokerGrants`): every `"config: grove ..."` → `"config: worker ..."`; the grant-label prefix `"grove "+w.Name` → `"worker "+w.Name`; the llm_auth error `"config: grove %s llm_auth ..."` → `"config: worker %s ..."`. Keep wording otherwise identical.

- [ ] **Step 1: Enumerate yaml key sites**

```bash
cd /Users/stephen/ai/lever_to
git grep -n 'groves:\|grove_to_grove:' -- '*.yaml' '*.yml' '*_test.go'
```
Expected: hits in `internal/config/config_test.go`, example `lever.yaml`s, `internal/cli/testdata/acceptance/lever.yaml`, and other CLI `_test.go` inline YAML (apply_test, hostmsg_test, initcmd_test). Note them all.

- [ ] **Step 2: Flip the struct tags**

In `internal/config/config.go`, change the two tags:

```go
Workers  []Worker    `yaml:"workers"`
```
```go
WorkerToWorker *bool `yaml:"worker_to_worker"`
```

- [ ] **Step 3: Reword validation error strings**

In `internal/config/config.go`, replace the token `grove` with `worker` inside every validation error string literal (the sites are, as of main: ~465, 468, 471, 478, 481, 490, 493, 521, 684). Example:

```go
return fmt.Errorf("config: worker needs name + dir (got %+v)", w)
```
```go
return fmt.Errorf("config: worker %q dir must be a subdir of the tree, not %q (which collides with the manager's mount root)", w.Name, w.Dir)
```
Locate them with `git grep -n '"config: grove' internal/config/config.go`. Do not change control flow.

- [ ] **Step 4: Update every YAML fixture key + config test strings**

Change `groves:` → `workers:` and `grove_to_grove:` → `worker_to_worker:` in all fixtures found in Step 1. In `config_test.go` also update the `replaceFirst(...)` search literals (the `"groves:\n  - {name: worker, dir: work, obtain: []}"` strings) to `"workers:"`, and the assertion/failure strings mentioning grove. In `examples_test.go` update the `"example %s: no groves"` string → `"...: no workers"`.

- [ ] **Step 5: Verify the schema round-trips and rejects unknown keys**

```bash
go test ./internal/config/... -v 2>&1 | tail -40
```
Expected: all config tests pass, including the collision/dir-dot rejection tests (now worded "worker"). If the loader uses strict decoding, a stray `groves:` anywhere would now error — Step 1's enumeration must have caught them all.

- [ ] **Step 6: Full green + commit**

```bash
go build ./... && go test ./... 2>&1 | tail -20
git add -A
git commit -m "refactor: rename config schema groves→workers key + validation strings (P1)"
```

---

## Task 3: Broker wire surface — HTTP routes + JSON keys

Rename the `/grove/*` mTLS routes and the `"grove"` JSON body/response keys. Producer (CLI/agent clients) and consumer (broker server) change together; the module is one repo so the rebuilt binaries are always in lockstep. No persisted state, so no teardown needed beyond the P1-assumed bring-up.

**Files:**
- Modify: `internal/broker/server.go` (route registration strings)
- Modify: `internal/broker/worker.go` (`json:"grove"` tags on request/response structs)
- Modify: `internal/broker/msg.go`, `internal/broker/provision.go` (`json:"grove"` tags)
- Modify: `internal/cli/agent.go` (route strings `/grove/start|stop|suspend|resume|list`, body key `"grove"`)
- Modify: `internal/cli/worker_client.go` (`json:"grove"` tag on `workerResult`)
- Modify: `internal/cli/msg.go`, `internal/cli/watch.go` (`"grove"` body key in `/msg/list` payload)
- Modify: `internal/agent/provision.go` (`map[string]string{"grove": worker}` → `{"worker": worker}`)
- Modify: matching `_test.go` wire assertions: `internal/broker/worker_test.go`, `internal/broker/msg_test.go`, `internal/broker/provision_test.go`, `internal/cli/worker_client_test.go`, `internal/cli/agent_test.go`, `internal/cli/msg_test.go`, `internal/cli/watch_test.go`, `internal/agent/enrol_test.go`, `internal/agent/provision_test.go`

**Interfaces — wire map:**
- Route strings: `"/grove/start"` → `"/worker/start"`, and likewise `stop|suspend|resume|list`. Update the doc comment enumerating `/grove/*`.
- JSON keys: struct tag `json:"grove"` → `json:"worker"` on `workerStartRequest`, `workerResponse`, `msgListRequest`, `ProvisionRequest`, `workerResult`; and the literal body maps in clients (`"grove": ...` → `"worker": ...`).

- [ ] **Step 1: Enumerate wire sites**

```bash
cd /Users/stephen/ai/lever_to
git grep -n '"/grove/' -- '*.go'
git grep -n 'json:"grove"\|"grove":' -- '*.go'
```

- [ ] **Step 2: Rename routes in the server, then the clients**

Change all `"/grove/<verb>"` → `"/worker/<verb>"` in `internal/broker/server.go`, then the identical route strings in `internal/cli/agent.go`. They must match exactly.

- [ ] **Step 3: Rename JSON keys (tags + client body literals)**

Change `json:"grove"` → `json:"worker"` on the structs listed, and `"grove":` → `"worker":` in the client body maps (`cli/agent.go`, `cli/msg.go`, `cli/watch.go`, `agent/provision.go`).

- [ ] **Step 4: Update wire assertions in tests**

In each `_test.go` listed, update route literals (`/grove/…` → `/worker/…`) and JSON body/response key assertions (`"grove"` → `"worker"`). These are the regression guard that producer and consumer moved together.

- [ ] **Step 5: Verify + commit**

```bash
go build ./... && go test ./internal/broker/... ./internal/cli/... ./internal/agent/... 2>&1 | tail -30
go test ./... 2>&1 | tail -10
git add -A
git commit -m "refactor: rename broker wire surface /grove→/worker + json keys (P1)"
```
Expected: all pass; broker/cli/agent packages exercise the routes+bodies end to end.

---

## Task 4: CLI flags — `--grove` → `--worker`, `-grove` → `-worker`

Two grove-named CLI flags, on different binaries. Rename each and its consumers/tests.

**Files:**
- Modify: `internal/cli/msg.go` (the `--grove` StringVar on `msg list`)
- Modify: `internal/cli/msg_test.go` (test passes `--grove`)
- Modify: `cmd/lever-agent/main.go` (the `-grove` flag on `provision`, its `-out` help text, error string)
- Modify: `cmd/lever-agent/main_test.go` (invokes `provision -grove worker`)
- Modify: `internal/cli/acceptance.go` (invokes `lever-agent provision -grove worker`)
- Modify: `tools/test/lima-e2e.sh` (if it invokes `-grove`; confirm in Step 1)

**Interfaces — flag map:**
- `lever-manager msg list --grove <name>` → `--worker <name>`. Update the flag name and help string (`"manager only: read this worker's project inbox"`).
- `lever-agent provision -grove <name>` → `-worker <name>`. Update flag name, `-out` help text (`"worker bootstrap JSON"`), and the empty-value error string.

- [ ] **Step 1: Enumerate flag sites**

```bash
cd /Users/stephen/ai/lever_to
git grep -n '"grove"\|-grove\|--grove' -- '*.go' 'tools/test/lima-e2e.sh'
```
(Distinguish flag registrations/uses from any remaining wire keys — those were Task 3.)

- [ ] **Step 2: Rename the `msg list` flag**

In `internal/cli/msg.go`:

```go
c.Flags().StringVar(&worker, "worker", "", "manager only: read this worker's project inbox")
```
Update `internal/cli/msg_test.go` to pass `--worker`.

- [ ] **Step 3: Rename the `lever-agent provision` flag**

In `cmd/lever-agent/main.go`:

```go
worker := fs.String("worker", "", "worker name to provision a ticket for")
```
Update the `-out` help text and the empty-value error string to say "worker". Update `cmd/lever-agent/main_test.go` and `internal/cli/acceptance.go` to pass `-worker worker`.

- [ ] **Step 4: Verify + commit**

```bash
go build ./... && go test ./internal/cli/... ./cmd/lever-agent/... 2>&1 | tail -30
go test ./... 2>&1 | tail -10
git add -A
git commit -m "refactor: rename CLI flags --grove/-grove → --worker/-worker (P1)"
```

---

## Task 5: Apply plan step name — `register-grove` → `register-worker`

One internal plan step-name string, coupled producer→consumer→test-assertions.

**Files:**
- Modify: `internal/apply/plan.go` (emits `Step{Kind: "register-grove", ...}` + doc-comment enum)
- Modify: `internal/apply/run.go` (the `case "register-manager", "register-grove":` switch arm)
- Modify: `internal/apply/plan_test.go` (step-name assertions `"register-grove"`)
- Modify: `internal/apply/run_test.go`, `internal/apply/integration_test.go` (step-name comments/assertions/prose that reference "register-grove"/"worker grove")

**Interfaces:** plan step Kind string `"register-grove"` → `"register-worker"`. The `"register-manager"` string is unchanged.

- [ ] **Step 1: Enumerate**

```bash
cd /Users/stephen/ai/lever_to
git grep -n 'register-grove' -- '*.go'
```

- [ ] **Step 2: Rename producer + consumer**

In `internal/apply/plan.go` change the emitted `Kind: "register-grove"` → `"register-worker"` and the doc-comment enumeration. In `internal/apply/run.go` change the switch case to `case "register-manager", "register-worker":`.

- [ ] **Step 3: Update test assertions**

Change every `"register-grove"` assertion in `plan_test.go` (and any in `run_test.go`/`integration_test.go`) to `"register-worker"`.

- [ ] **Step 4: Verify + commit**

```bash
go build ./... && go test ./internal/apply/... 2>&1 | tail -30
git add -A
git commit -m "refactor: rename apply step register-grove → register-worker (P1)"
```

---

## Task 6: Filesystem convention — `groves/<name>` → `workers/<name>` + example rename

Move the `groves/` subdir convention to `workers/` across example/testdata trees and every hardcoded path literal, and rename the `hello-grove` example. This is the runtime-path change (`/lever/groves/<name>` → `/lever/workers/<name>`) that flows purely from config `dir:` values and on-disk dirs — no hardcoded Go constant is involved. Do the directory moves and the path-literal edits in one commit so tests stay green.

**Files:**
- `git mv` example workspace dirs: `examples/assistant-demo/workspace/groves/todo/` → `.../workers/todo/`; `examples/hello-grove/workspace/groves/worker/` → (after the example rename below) `examples/hello-worker/workspace/workers/worker/`; `examples/multi-project/workspace/groves/{svc-a,svc-b,svc-c}/` → `.../workers/...`; `examples/two-agents-comms/workspace/groves/{producer,consumer}/` → `.../workers/...`
- `git mv examples/hello-grove examples/hello-worker`, then edit `examples/hello-worker/lever.yaml` app `name: hello-grove` → `hello-worker`
- `git mv internal/cli/testdata/acceptance/groves internal/cli/testdata/acceptance/workers`
- Modify `dir:` values in every example + testdata `lever.yaml`: `dir: groves/<name>` → `dir: workers/<name>`
- Modify `internal/config/examples_test.go` — `"hello-grove"` → `"hello-worker"`
- Modify path literals in `_test.go`: `internal/cli/apply_test.go`, `internal/cli/hostmsg_test.go`, `internal/cli/doctor_checks_test.go`, `internal/cli/initcmd_test.go`, `internal/cli/skills_scaffold_test.go`, `internal/apply/plan_test.go`, `internal/apply/run_test.go`, `internal/apply/integration_test.go`, `internal/scion/client_test.go`, `internal/config/config_test.go` (the `filepath.Join(tree,"groves","appa")` + inline `dir: groves/...` fixtures) — every `groves/` and `/lever/groves/` and `/t/groves/`-style literal → `workers/`
- Modify `tools/test/lima-e2e.sh` — `mkdir -p "$INST/groves/worker"`, the `touch .../groves/worker/.gitkeep`, and the heredoc `dir: groves/worker` → `workers/`

**Interfaces:** path fragment `groves/` → `workers/` in every filesystem path, config `dir:` value, and test path literal. Example identity `hello-grove` → `hello-worker`.

- [ ] **Step 1: Enumerate every path + dir**

```bash
cd /Users/stephen/ai/lever_to
git grep -n 'groves/' -- ':!docs-site' ':!CHANGELOG.md' ':!*.md'
git ls-files | grep '/groves/'   # actual tracked directories to move
git grep -rน 'hello-grove' 2>/dev/null; git grep -n 'hello-grove'
```

- [ ] **Step 2: Move the directories**

```bash
git mv examples/hello-grove examples/hello-worker
# then move each workspace groves/ dir under every example + the acceptance testdata:
git mv examples/hello-worker/workspace/groves examples/hello-worker/workspace/workers
git mv examples/assistant-demo/workspace/groves examples/assistant-demo/workspace/workers
git mv examples/multi-project/workspace/groves examples/multi-project/workspace/workers
git mv examples/two-agents-comms/workspace/groves examples/two-agents-comms/workspace/workers
git mv internal/cli/testdata/acceptance/groves internal/cli/testdata/acceptance/workers
```
(If a `groves` dir holds only a `.keep`/`.gitkeep`/`.scion`/`.lever` — move the whole dir as above; git tracks the contents.)

- [ ] **Step 3: Update config `dir:` values + app name + example test reference**

Change `dir: groves/<name>` → `dir: workers/<name>` in every example/testdata `lever.yaml`. Set `examples/hello-worker/lever.yaml` `name: hello-worker`. In `internal/config/examples_test.go` change `"hello-grove"` → `"hello-worker"`.

- [ ] **Step 4: Update path literals in tests + lima-e2e.sh**

Replace every `groves/` path fragment (including `/lever/groves/`, `/t/groves/`, `/tmp/foo/groves/`, `filepath.Join(tree,"groves",...)`) with `workers/` in the `_test.go` files listed, and the `groves/worker` paths + heredoc `dir:` in `tools/test/lima-e2e.sh`.

- [ ] **Step 5: Verify green (path-sensitive tests exercise the moved layout)**

```bash
go build ./... && go test ./... 2>&1 | tail -40
```
Expected: all pass, in particular `internal/config` (examples_test loads `examples/hello-worker`), `internal/cli` (testdata acceptance layout), `internal/apply`, `internal/scion`.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "refactor: move groves/<name> convention → workers/<name>; rename hello-grove example (P1)"
```

---

## Task 7: Prose, comments, audit ops, embedded skills, reference docs + CHANGELOG + completeness gate

Sweep the remaining cosmetic + user-facing text and add the changelog entry, then assert the rename is complete. **Deferred within P1:** the docs-site narrative *guides* (`docs-site/_guides/*.md`) are **not** renamed here — they describe the current architecture that P2 rewrites, so a grove→worker pass on them now is wasted churn; they are folded into the post-P2 docs update. This task renames only text that ships as product surface or documents the P1-changed config/CLI.

**Files:**
- Modify: audit-op + deny-reason + cobra help strings in Go: `internal/broker/worker.go` (audit op `"grove"` → `"worker"`), `internal/broker/msg.go` (deny-reason strings), `internal/cli/agent.go` `internal/cli/hostmsg.go` `internal/cli/doctor_checks.go` `internal/cli/skills_scaffold.go` `internal/cli/root.go` (cobra `Short`/help prose), plus any remaining Go doc-comments containing the token
- Modify: embedded skills `internal/skills/lever-agent/SKILL.md`, `internal/skills/lever-operator/SKILL.md` (vocabulary + taught command placeholders `<grove>` → `<worker>`)
- Modify: example prose `examples/*/manager.md`, `examples/*/README.md`, and the `internal/cli/testdata/acceptance/manager.md`
- Modify: reference docs `docs-site/_reference/config.md` (the `groves:`/`grove_to_grove`/`--grove` documentation), `docs-site/_reference/cli.md` (`--grove` flag, grove-name semantics), `docs-site/_reference/backends.md` (1 cosmetic hit)
- Modify: `README.md` (config snippets + `groves:`/`groves/` + prose)
- Add: a `CHANGELOG.md` entry (do not edit past entries)

- [ ] **Step 1: Enumerate remaining prose/text sites**

```bash
cd /Users/stephen/ai/lever_to
git grep -n -i grove -- '*.go'                          # audit ops, deny strings, comments, cobra Short
git grep -n -i grove -- internal/skills examples '*.md' ':!docs-site/_guides' ':!CHANGELOG.md' ':!docs/2026-07-04-followups-handover.md' ':!docs/superpowers'
git grep -n -i grove -- docs-site/_reference README.md
```

- [ ] **Step 2: Rename Go string/comment prose**

Replace the token in every remaining Go hit: audit op literals (`"grove"` → `"worker"`), deny-reason strings (`"...is not the manager or a declared worker"`, `"worker→worker messaging is disabled"`, `"unknown worker %q"`, `"a worker may only read its own inbox"`), cobra `Short`/flag help, and doc-comments. Keep meaning identical.

- [ ] **Step 3: Rename embedded skills + example prose + testdata manager.md**

Update `internal/skills/lever-agent/SKILL.md` ("grove agent" → "worker"), `internal/skills/lever-operator/SKILL.md` (section "Dispatching groves" → "Dispatching workers", command placeholders `--to <grove>`/`agent start <grove>` → `<worker>`), and the `examples/*/manager.md`, `examples/*/README.md`, `internal/cli/testdata/acceptance/manager.md` prose. (README tree diagrams already have their paths moved by Task 6; this is prose only.)

- [ ] **Step 4: Rename reference docs + README**

Update `docs-site/_reference/config.md` (`groves:` → `workers:`, `grove_to_grove` → `worker_to_worker`, `--grove` → `--worker`, `groves/` layout paths → `workers/`), `docs-site/_reference/cli.md` (`--grove` → `--worker`, grove-name → worker-name wording), `docs-site/_reference/backends.md`, and `README.md` config snippets + prose.

- [ ] **Step 5: Add a changelog entry**

Prepend a new entry under the unreleased/next section of `CHANGELOG.md`:

```markdown
### Changed
- Renamed the `grove` concept to `worker` throughout: config keys `groves:`→`workers:` and `grove_to_grove:`→`worker_to_worker:`, the `--grove`/`-grove` CLI flags → `--worker`/`-worker`, broker routes `/grove/*`→`/worker/*`, and the `groves/<name>` workspace convention → `workers/<name>`. Prerelease clean break — no migration. (P1 of the single-project re-architecture.)
```

- [ ] **Step 6: Completeness gate**

```bash
# No grove token left in code, active config, embedded skills, testdata, examples, or reference docs:
git grep -in grove -- '*.go' ':!CHANGELOG.md'                                          # expect: empty
git grep -in grove -- examples internal/skills internal/cli/testdata docs-site/_reference README.md   # expect: empty
# Allowed remaining hits (historical / narrative-deferred), for the record:
git grep -in grove -- docs-site/_guides CHANGELOG.md docs/2026-07-04-followups-handover.md docs/superpowers | wc -l
```
Expected: the first two commands print nothing. The third is non-zero (deferred guides + immutable history + spec/plan) and is acceptable.

- [ ] **Step 7: Full green + commit**

```bash
go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test ./... 2>&1 | tail -20
git add -A
git commit -m "docs: rename grove→worker in prose, skills, reference docs; changelog (P1)"
```

---

## P1 acceptance criteria

- `go build ./... && go vet ./... && go test ./...` green; `gofmt -l` empty.
- The Step-6 completeness gate: zero `grove` hits in Go, active config, embedded skills, testdata, examples, and reference docs.
- No validation rule removed or weakened (the `dir:"."` rejection and both name-collision checks still fire, reworded).
- No structural change: workers are still one-project-per-worker; `-g` scoping, register-per-worker apply, and isolation are untouched.
- `examples/hello-worker` loads and the acceptance testdata under `workers/` is exercised by the existing tests.

## Self-review notes (coordinator)

- **Spec coverage:** §10 (grove→worker, identity-aware, don't rename→project) → Tasks 1–7 + Global Constraints. §11 config schema (`App.Groves`→`App.Workers`, `Grove`→`Worker`, `grove_to_grove`→`worker_to_worker`) → Tasks 1–2. §11's *rule removal* is explicitly deferred to P2 (Global Constraints) because it depends on the single-project model — flagged so the reviewer does not treat the retained rules as a miss. §14 Q4 (rename first, on current structure) → this whole plan.
- **Deferred, on purpose:** docs-site `_guides/*` narrative (Task 7 note) and the two unrelated uncommitted docs-site edits (Branch setup).
- **Type consistency:** the exported map in Task 1 is the single source of the new names; Tasks 2–7 reference those names (`App.Workers`, `Worker`, `WorkerToWorker`, `WorkerSpec`, etc.) consistently.
