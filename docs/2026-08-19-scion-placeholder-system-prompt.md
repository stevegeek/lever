# Scion's `# Placeholder` system prompt — findings, options, recommendation

Date: 2026-08-19
Scope: read-only investigation. No code changed, no lever command run, no live mutation.

Scion module read at
`/Users/stephen/go/pkg/mod/github.com/!google!cloud!platform/scion@v0.0.0-20260812140559-e82a2a082bbc`
(abbreviated `$M` below). Lever worktree
`/Users/stephen/ai/lever_to-worktrees/remote-access` (`$L`).
Live read-only inspection of the OrbStack machine `lever-assistant`.

---

## Findings

### F1. The flag is emitted only when the staged prompt is non-empty after trimming

`$M/pkg/harness/container_script_harness.go:176-213`

```go
if prompt := c.readSystemPrompt(); prompt != "" {
    args = append(args, cmd.SystemPromptFlag, prompt)
}
```

`readSystemPrompt()` returns `""` unless ALL of these hold:

1. `command.system_prompt_flag` is set in the harness `config.yaml`
   (`container_script_harness.go:188`);
2. the harness has an agent home (`:191`);
3. one of two files exists AND has non-whitespace content
   (`:198-211`), tried in order:
   - `<agentHome>/.scion/harness/inputs/system-prompt.md` (the staged input), then
   - `<agentHome>/<system_prompt_file>` — for claude, `.claude/system-prompt.md`.

The content check is `strings.TrimSpace(string(data))`, and an unreadable file is
skipped (`continue`), not an error.

**So: an ABSENT file and an EMPTY (or whitespace-only) file both mean NO FLAG.**
This is the crux the task asked about, and it makes the "make it empty" class of
fix viable.

### F2. …but DELETING the template file is actively worse than emptying it

The stock `default` template's `scion-agent.yaml` names the file explicitly:

`$M/resources/templates/default/scion-agent.yaml`
```yaml
agent_instructions: agents.md
system_prompt: system-prompt.md
```

Provisioning resolves that name across the template chain
(`$M/pkg/agent/provision.go:1051-1071` → `config.ResolveContentInChain`), and
`ResolveContentInChain` ends with:

`$M/pkg/config/templates.go:82-83`
```go
// No template had the file; treat as inline content
return []byte(field), nil
```

If `system-prompt.md` is removed but `system_prompt: system-prompt.md` stays in
the template config, scion stages the **literal string `system-prompt.md`** as
the system prompt and the agent launches with
`--system-prompt 'system-prompt.md'`. Emptying the file is safe; deleting it is a
trap.

### F3. Blast radius: the flag is claude-only; the placeholder file is not

Only one bundled harness declares a native system-prompt flag:

| harness | `system_prompt_mode` | `system_prompt_file` | `command.system_prompt_flag` |
|---|---|---|---|
| claude | `native` | `.claude/system-prompt.md` | **`--system-prompt`** |
| gemini-cli | `native` | `.gemini/system_prompt.md` | *(none — uses `GEMINI_SYSTEM_MD`)* |
| antigravity | `prepend_to_instructions` | `GEMINI.md` | none |
| codex, copilot, hermes | `prepend_to_instructions` | *(none)* | none |
| opencode | `prepend_to_instructions` | `AGENTS.md` | none |

(`$M/harnesses/*/config.yaml`; claude's flag at `harnesses/claude/config.yaml:63`.)

The placeholder content itself is shipped in three places, all containing exactly
`# Placeholder`:

- `$M/resources/templates/default/system-prompt.md` (14 bytes) — the copy the hub
  materializes to disk;
- `$M/pkg/config/embeds/templates/default/system-prompt.md` (14 bytes) — the copy
  `SeedAgnosticTemplate` writes;
- `$M/harnesses/gemini-cli/home/.gemini/system_prompt.md` (13 bytes).

So gemini-cli carries the same placeholder, but consumes it through an env var
that it guards (see F4), and the `prepend_to_instructions` harnesses would only
get a stray "# System Prompt / # Placeholder" heading in their instructions file.
**Only claude has its entire built-in system prompt replaced.**

### F4. Upstream knows the placeholder is not a real prompt — for gemini only

`$M/harnesses/gemini-cli/provision.py:140-147`

```python
def _is_meaningful_system_prompt(text: str) -> bool:
    """Return True if text has substantive content, not just a placeholder."""
    stripped = text.strip()
    if not stripped:
        return False
    if stripped.lower() in ("# placeholder",):
        return False
    return True
```

Gemini's provisioner writes the file but refuses to export `GEMINI_SYSTEM_MD`
when the content is the placeholder (`provision.py:224-231`). The claude path has
no equivalent guard: the argv is built host-side in Go
(`container_script_harness.go:176`) and only tests for emptiness.

This is the strongest available evidence on the "intended or oversight?"
question, and it points at **oversight** — a guard exists for exactly this string,
in exactly the sibling harness, and was not applied to the Go path that
materialised later. Supporting circumstantial evidence:

- `$M/changelog/2026-07-12-changelog.md:22` — "**[Harness]:** Apply
  `system_prompt_flag` in `ContainerScriptHarness.GetCommand` (#682)." The claude
  flag only started reaching argv in July 2026; the placeholder file predates it.
  It was inert scaffolding until #682 turned it into a live CLI argument.
- `$M/harnesses/claude/provision.py` never writes `.claude/system-prompt.md` at
  all (its only system-prompt reference is
  `scion_harness.project_instructions(..., system_prompt_mode="none")` at
  `:304-307`). Verified live: that file does not exist in the assistant's agent
  home. So claude's declared `system_prompt_file` is a read-fallback path only,
  and `HasSystemPrompt()` — which stats it — always returns false for claude.
- `HasSystemPrompt()` has **no production call sites** in the whole module
  (definitions in `pkg/harness/{generic,declarative_generic,container_script_harness}.go`
  and `pkg/api/harness.go:30`; every other hit is a test mock). The authoring
  guide already lists it as a known wart
  (`$M/harnesses/authoring-guide.md:687-689`).
- Neither `$M/docs-site/src/content/docs/supported-harnesses.md` (claude section,
  lines 44-76) nor `$M/harnesses/authoring-guide.md` mentions that
  `--system-prompt` REPLACES the tool's built-in prompt. The authoring guide says
  only "If `system_prompt_flag` is set, the host appends it with the staged
  system-prompt content at launch" (`:127-128`).

Honest caveat: no design doc, issue, or comment says "this placeholder is
deliberate scaffolding for operators", and none says "this is a bug" either. The
inference above is from the gemini guard plus the timeline, not from an explicit
statement.

### F5. `system_prompt_mode` values — and none of them suppress the flag

Enum is fixed at `$M/pkg/config/schemas/settings-v1.schema.json:369-372`:
`["native", "prepend_to_instructions", "none"]`.

Behaviour (`$M/pkg/harness/declarative_generic.go:163-190` for the declarative
path; `$M/harnesses/scion_harness.py:769-836` `project_instructions()` for the
container-script path):

- `native` — write the staged prompt to `system_prompt_file` (declarative path) /
  leave it to the harness script (container-script path).
- `prepend_to_instructions` — inject a `# System Prompt` section at the top of the
  instructions file.
- `none` — skip the instructions projection entirely.

**There is no append mode and nothing maps to `--append-system-prompt`.** Scion
has no reference to `--append-system-prompt` anywhere in the module.

Critically: `readSystemPrompt()` (F1) does **not** consult `system_prompt_mode` at
all. Setting `system_prompt_mode: none` on the claude harness would stop the
container script projecting the prompt, and the host would **still** append
`--system-prompt`. Only removing `command.system_prompt_flag` suppresses it. That
is an internal inconsistency in scion worth reporting on its own.

### F6. Lever passes no system prompt and no template — confirmed

- `$L/internal/scion/lifecycle.go:230-286` `Start()` builds
  `scion [-g project] start <name> <task> --harness claude --harness-auth … [--role …] [--image …] [--workspace …]`.
  No `-t/--type`, no `--config`.
- `$L/internal/apply/run.go:624` `stepStartManager` reads `manager.prompt_file`
  and passes it as `StartOpts.Task` — the boot prompt is the first **user** turn.
- lever has no reference to templates, `system_prompt`, or `scion-agent.yaml`
  anywhere in `internal/` or `cmd/` (grep clean; the only "template" hits are the
  Lima VM template and x509 cert templates).

So the system prompt is 100% scion's doing, as stated.

### F7. Which copy of the template actually feeds a lever agent

There are three on-disk copies in the live guest. Only one is in the path:

| path | live content | refreshed when |
|---|---|---|
| `~/.scion/templates/default/system-prompt.md` | `# Placeholder` (14 B, mtime Aug 18 23:05) | **force-overwritten on every `scion server start`** |
| `~/.scion/storage/local/templates/global/default/system-prompt.md` | `# Placeholder` (14 B, mtime Jul 10) | hub resource bootstrap |
| `~/.scion/project-configs/lever__59ef7fc3/.scion/templates/` | **empty dir** | never (scion does not seed it) |

The force refresh is `$M/cmd/server_foreground.go:113`:

```go
} else if !hostedMode {
    if err := config.UpdateDefaultTemplates(true, harness.EmbedOnlyHarnesses()); err != nil {
```

→ `$M/pkg/config/templates.go:493` → `MaterializeBundledResources(globalDir, MaterializeOptions{Force: true})`
→ `$M/pkg/config/materialize.go:35-42`, which iterates `resources.BuiltinTemplates()` — **only the `default` template**. Any other template directory under `~/.scion/templates/` is untouched.

Which copy is consumed depends on whether a template ID reaches the broker:

- Hub create resolves a template **only when the request names one**:
  `$M/pkg/hub/handlers_agents_core.go:839-840` — `if req.Template != "" { resolvedTemplate, err = s.resolveTemplate(...) }`.
  lever never passes `-t`, so `AppliedConfig.TemplateID`/`TemplateHash` stay empty.
- The broker therefore skips hydration
  (`$M/pkg/runtimebroker/handlers.go:944-948` returns `""` when both are empty;
  `$M/pkg/runtimebroker/start_context.go:456-473` leaves `opts.Template` unset)
  and provisioning falls back to local template resolution.
- Local resolution: `$M/pkg/agent/provision.go:1774-1782` defaults the name from
  effective settings' `default_template` (live value: `default`), then
  `GetTemplateChainInProject` → `FindTemplateInProjectPath`
  (`$M/pkg/config/templates.go:360-393`) searches **`<projectConfigDir>/templates/<name>` first, then `~/.scion/templates/<name>`**.

**Conclusion: today the effective source is `~/.scion/templates/default/` — the copy scion force-rewrites on every hub start.** The hub-storage copy is out of the path for lever's agents (it would come into play only if lever started passing `-t`).

Note the asymmetry between the two bundled copies: `resources/templates/default/agents.md` is **0 bytes** while `pkg/config/embeds/templates/default/agents.md` is **1184 bytes** (the "sciontool status" instructions). The live `~/.scion/templates/default/agents.md` is 0 bytes and the hub-storage copy is 1184 bytes — i.e. the two bundled default templates have already drifted upstream, and the empty `agents.md` sits next to the non-empty `system-prompt.md`. Whoever emptied `agents.md` did not empty its sibling.

### F8. The staged prompt is written ONCE, at provisioning — the live agent's is frozen

Live, read-only, in the assistant's agent home
(`~/.scion/project-configs/lever__59ef7fc3/.scion/agents/assistant/home`):

```
.scion/harness/inputs/
  instructions.md        4189 B  Jul 31 11:58
  system-prompt.md         14 B  Jul 31 11:58   <- "# Placeholder"
  auth-candidates.json    297 B  Aug 18 22:45
  telemetry.json         1309 B  Aug 18 22:45
.claude/CLAUDE.md        4270 B  Aug 18 22:45   <- re-projected each start
.claude/system-prompt.md  ABSENT
```

The Aug 18 restart re-staged auth/telemetry and re-projected `CLAUDE.md`, but did
**not** rewrite the prompt inputs. `ProvisionAgent` only runs when the agent
directory does not exist (`$M/pkg/agent/provision.go:1779`).

Two consequences:

1. **Any template-level fix reaches new agents only.** The live assistant will
   keep launching with `--system-prompt '# Placeholder'` until either its staged
   input file is changed in place, or it is re-provisioned (which destroys the
   conversation — the exact loss recorded on 2026-07-31).
2. The instructions channel (`agent_instructions` → `.claude/CLAUDE.md`) is alive
   and re-projected on every container start, so lever's guidance is **not**
   dependent on the system prompt.

---

## Options

`readSystemPrompt` is evaluated host-side at every start/resume, so every option
below takes effect at the **next container start** of an agent whose staged input
it changes; none of them require re-provisioning except where noted.

| # | Option | Where lever writes | Survives `scion server start`? | Survives a scion pin bump? | What breaks / costs |
|---|---|---|---|---|---|
| **A1** | Empty (`: > file`) the global default template's `system-prompt.md` | `~/.scion/templates/default/system-prompt.md` (guest) | **No** — force-overwritten (F7) | n/a | Needs a lever apply step ordered *after* `KindScionServer`. Any out-of-band hub restart silently restores the placeholder for agents provisioned after it. Self-heals on the next `lever apply`. |
| **A2** | Put a *real* lever system prompt in that same file | same | No (same) | n/a | Same fragility, **plus** it keeps Claude Code's built-in prompt replaced — strictly worse than A1 unless lever genuinely wants to own the whole prompt. Not recommended. |
| **B** | Lever-owned overlay template: `~/.scion/templates/lever/system-prompt.md` (empty), then `scion -g <project> config set default_template lever` | one new dir + one settings key, both via existing lever seams | **Yes** — materialization only touches `default` (F7) | Yes — templates are scion's documented extension point | Chain becomes `[default, lever]` (`templates.go:399-421`), so `agents.md`, `home/`, `skills/` still come from scion's default and stay current. `ResolveContentInChain` walks the chain in reverse, so the overlay's empty file wins. Cost: 2 writes instead of 1; `config set` must target the project scope, because the project settings already pin `default_template: default` and project beats machine. |
| **C1** | Set `system_prompt_mode: none` in the claude harness-config | hub-stored harness-config | — | — | **Does not work.** `readSystemPrompt` ignores the mode (F5). |
| **C2** | Delete `command.system_prompt_flag` from the claude harness-config | the **hub-stored** copy `~/.scion/storage/local/harness-configs/global/claude/config.yaml` (this is the copy the broker hydrates, because lever passes `--harness claude` → `HarnessConfigID` is set → `hydrateHarnessConfig`) | Yes for the storage copy | **No** — lever would be maintaining a fork of scion's claude bundle and re-uploading it after every pin bump | Also permanently forfeits the ability to set a real system prompt. Highest maintenance of all options. |
| **D** | Pass `scion start --config <file>` with `system_prompt: " "` | one staged YAML in the guest; single change site (`lifecycle.go` `Start`) | Yes | Fragile: relies on a whitespace value staying non-empty at the config layer and empty after `TrimSpace`. If upstream ever trims inline config (making `SystemPrompt == ""`), `inlineProvidedSystemPrompt` goes false and provisioning **falls back to the template placeholder** — a silent regression | Verified wiring: `cmd/common.go:452-465` loads it for both local and hub paths, `startAgentViaHub` sends it as `req.Config` (`cmd/common.go:807-808`), `provision.go:991,1051-1056` uses the content directly. Note `scion create` (not `start`) drops `--config` in hub mode — `cmd/create.go:69-71` returns before loading it — so this only works on the `start` verb. Also writes a nonsense value into the hub agent record. |
| **E** | Delete the template's `system-prompt.md` | — | — | — | **Actively harmful** — stages the literal string `system-prompt.md` (F2). |
| **F** | Do nothing in lever; file upstream | — | — | — | Every lever agent keeps running with Claude Code's built-in system prompt replaced until an upstream release lands *and* the pin moves *and* affected agents are re-provisioned. The live assistant would never be fixed without conversation loss (F8). |
| **G** | One-off: truncate the live assistant's staged input | `.../agents/assistant/home/.scion/harness/inputs/system-prompt.md` | Yes (never re-staged, F8) | Yes | The only way to fix the **existing** agent without deleting it. Takes effect at the next container start. Orthogonal to A/B/D — needed in addition to any of them. |

---

## Recommendation

**Do B for new agents, plus G for the live one. Then file upstream.**

Reasoning, plainly:

1. **The right content is nothing, not something.** Claude Code's built-in system
   prompt is the tool's behavioural contract. Lever has two supported channels
   that already work — `agent_instructions` → `.claude/CLAUDE.md` (live and
   re-projected every start, F8) and the boot prompt as the first user turn (F6)
   — so lever needs no system prompt at all. An empty prompt restores the stock
   Claude Code behaviour every other user of the tool gets. A2 (writing lever's
   own prompt) trades one replacement for another and should be rejected.

2. **B is the only option that is both durable and uses the product as designed.**
   A1 is one line of work but sits in the one directory scion force-rewrites on
   every hub start (F7); it would leave a silent failure window after any
   out-of-band restart. C2 means forking and re-uploading scion's claude bundle
   on every pin bump. D depends on a whitespace value surviving upstream
   normalisation, and its failure mode is a *silent* fall-back to the placeholder.
   B writes into a directory scion's materializer provably never touches, layers
   on top of the stock default so `agents.md`/`skills`/`home` keep tracking
   upstream, and both of its writes go through seams lever already has (a guest
   file write, and `scion config set` through the existing `scion.Client`).

3. **Pin it with a test.** The behaviour B relies on is `readSystemPrompt`'s
   `TrimSpace(...) != ""` gate. A lever test that asserts the emitted argv carries
   no `--system-prompt` (or a `lever doctor` check that greps the agent's launch
   argv) turns a future silent regression into a loud one.

4. **G is not optional.** Without it the production assistant keeps the frozen
   `# Placeholder` forever, because provisioning never re-runs for an existing
   agent. It is a single-file truncation in the guest, it survives restarts, and
   it costs no conversation.

Sequencing note: A1 is a reasonable 10-minute stopgap if B is not ready, provided
the write is ordered after `KindScionServer` in `internal/apply/plan.go`. It
should not be the end state.

---

## Upstream ask

Three items, in priority order. The first is the real bug; the other two are
consistency defects found on the way.

1. **Claude's system prompt should append, not replace — or the placeholder must
   not be emitted.** `harnesses/claude/config.yaml:63` declares
   `system_prompt_flag: "--system-prompt"`, which per `claude --help` (2.1.226)
   *replaces* Claude Code's built-in system prompt, whereas
   `--append-system-prompt` adds to it. Combined with the stock `default`
   template's `system-prompt.md` reading `# Placeholder`
   (`resources/templates/default/system-prompt.md`), **every scion claude agent
   started from the stock template runs with Claude Code's entire built-in system
   prompt replaced by the two words `# Placeholder`.** Concretely, one of:
   - blank `resources/templates/default/system-prompt.md` and
     `pkg/config/embeds/templates/default/system-prompt.md` (its sibling
     `agents.md` is already 0 bytes in the `resources` copy — the same treatment,
     applied to the file that actually matters); and/or
   - apply gemini's `_is_meaningful_system_prompt` guard
     (`harnesses/gemini-cli/provision.py:140-147`) in
     `ContainerScriptHarness.readSystemPrompt`; and/or
   - add an `append` flag/mode so a harness can declare
     `--append-system-prompt`, which is what an *additive* scion prompt actually
     wants.

   Worth stating in the report to upstream: this is a behaviour change introduced
   by #682 (`changelog/2026-07-12-changelog.md:22`), which first wired
   `system_prompt_flag` into argv. The placeholder was inert before that.

2. **`system_prompt_mode: none` does not suppress the native flag.**
   `pkg/harness/container_script_harness.go:186-213` never consults
   `SystemPromptMode`. An operator who sets `none` — the documented way to say
   "this harness should not receive a system prompt",
   `harnesses/authoring-guide.md:142` — still gets `--system-prompt <content>` on
   the command line. Either honour the mode in `readSystemPrompt` or document
   that `none` governs only the instructions projection.

3. **Two smaller defects, worth a line each.**
   - `HasSystemPrompt()` (`pkg/api/harness.go:30`) has no production callers in
     the module; for the claude harness it can only ever return false, because
     `harnesses/claude/provision.py` never writes `.claude/system-prompt.md`.
     Either wire it up or drop it from the interface.
   - `scion create --config` is silently ignored in hub mode:
     `cmd/create.go:69-71` returns via `createAgentViaHub` **before** the inline
     config is loaded at `:73-85`. `scion start --config` handles it correctly
     (`cmd/common.go:452-465`). Same flag, same help text, different behaviour.
