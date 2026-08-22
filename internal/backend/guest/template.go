package guest

import (
	"context"
	"fmt"
	"strings"
)

// The guest half of lever's agent-template overlay.
//
// # Why lever owns a template at all
//
// Scion's stock `default` template ships a `system-prompt.md` whose entire
// content is the literal text `# Placeholder`, and the claude harness declares
// `system_prompt_flag: "--system-prompt"`. Per `claude --help`, that flag
// REPLACES Claude Code's built-in system prompt (the additive form is
// `--append-system-prompt`). So every agent provisioned from the stock template
// launches as:
//
//	claude … --system-prompt '# Placeholder'
//
// — with the tool's entire behavioural contract replaced by two words. Tool
// definitions arrive over the API and are unaffected, which is why this is
// invisible until you look at the argv.
//
// It is upstream scaffolding that went live by accident: the placeholder file
// predates scion #682 (2026-07-12), which first wired `system_prompt_flag` into
// the command line. Scion's own gemini provisioner carries a
// `_is_meaningful_system_prompt` guard that special-cases this exact string;
// the Go path that builds claude's argv has no equivalent.
//
// # Why the fix is an EMPTY prompt, not a lever-authored one
//
// Claude Code's built-in system prompt is the tool's behavioural contract, and
// lever already has two working channels for its own guidance: the
// `agent_instructions` projection (→ the agent's CLAUDE.md, re-projected on
// every container start) and the boot prompt, which lever passes as the agent's
// TASK — the first user turn. Lever needs no system prompt, so it asks for
// none, and every agent gets the same stock behaviour any other Claude Code
// user gets. Writing lever's own prompt here would trade one replacement for
// another.
//
// # Why an OVERLAY template, and not the obvious alternatives
//
// The emit condition is `strings.TrimSpace(content) != ""`
// (pkg/harness/container_script_harness.go readSystemPrompt), so an empty file
// suppresses the flag entirely. Getting an empty file in front of scion's is
// the whole job. The alternatives all fail:
//
//   - Emptying `~/.scion/templates/default/system-prompt.md` in place does work,
//     but scion force-overwrites that directory on every `scion server start`
//     (cmd/server_foreground.go → config.UpdateDefaultTemplates(true, …) →
//     MaterializeBundledResources{Force:true}). Any out-of-band hub restart
//     silently restores the placeholder for agents provisioned after it.
//   - DELETING the file is actively harmful. The template config names it
//     (`system_prompt: system-prompt.md`), and when no template in the chain
//     supplies the file, config.ResolveContentInChain falls through to "treat
//     the field as inline content" — launching the agent with
//     `--system-prompt 'system-prompt.md'`, the literal filename.
//   - `system_prompt_mode: none` does nothing: readSystemPrompt never consults
//     the mode. (Filed upstream; see the engineering note.)
//   - Editing the harness config means maintaining a fork of scion's claude
//     bundle across every pin bump.
//
// The overlay avoids all of it. MaterializeBundledResources iterates only
// `resources.BuiltinTemplates()` — the `default` template alone — so a template
// directory under any other name is never touched. And because the name is not
// "default", config.GetTemplateChainInProject PREPENDS the stock default as a
// base layer, so `agents.md`, `home/` and `skills/` keep tracking upstream;
// ResolveContentInChain then walks the chain in REVERSE (most specific first),
// so lever's empty file wins for this one field and nothing else changes.
const (
	// LeverTemplateName is the overlay template's directory name under the
	// guest's global templates dir. Deliberately NOT "default": that name would
	// suppress the automatic base-layer prepend in GetTemplateChainInProject and
	// leave agents with ONLY lever's template — losing scion's agents.md, home/
	// and skills/ entirely.
	LeverTemplateName = "lever"

	// leverTemplateRel is the overlay directory, relative to the run user's home.
	leverTemplateRel = ".scion/templates/" + LeverTemplateName
)

// EnsureLeverTemplate creates the overlay template in the guest and reports
// whether it had to write anything.
//
// The directory holds exactly one file: an empty `system-prompt.md`. Empty is
// the entire point (see this file's doc) — do not "fix" it by adding content.
//
// Idempotent, and deliberately convergent rather than assertive: it rewrites
// the file only when it is absent or non-empty, so a re-apply neither churns
// the guest nor reports a spurious change. It does NOT create a
// `scion-agent.yaml`: with none, the chain inherits the stock default's config,
// which is what makes this an overlay rather than a replacement.
func (g Guest) EnsureLeverTemplate(ctx context.Context) (bool, error) {
	// -s is "size > 0", so this writes when the file is missing OR non-empty,
	// and stays silent otherwise. `: >` truncates in place rather than
	// unlinking, so no reader ever observes the path missing.
	script := fmt.Sprintf(
		`set -e; d="$HOME/%s"; f="$d/system-prompt.md"; `+
			`if [ -s "$f" ] || [ ! -f "$f" ]; then mkdir -p "$d" && : > "$f" && echo wrote; fi`,
		leverTemplateRel)
	res, err := g.userRun(ctx, "bash", "-c", script)
	if err != nil {
		return false, fmt.Errorf("guest: create the %s agent template: %w", LeverTemplateName, err)
	}
	return strings.Contains(res.Stdout, "wrote"), nil
}
