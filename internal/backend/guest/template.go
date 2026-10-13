package guest

import (
	"context"
	"fmt"
	"strings"

	"github.com/stevegeek/lever/internal/scion/layout"
)

// The guest half of lever's agent-template overlay.
//
// Scion's stock `default` template ships a `system-prompt.md` containing only
// `# Placeholder`, and the claude harness passes it with `--system-prompt`,
// which REPLACES Claude Code's built-in system prompt. An empty file
// suppresses the flag entirely, so every agent keeps Claude Code's stock
// behaviour; lever's own guidance goes through CLAUDE.md and the boot prompt.
//
// It must be an overlay template:
//
//   - scion force-overwrites `~/.scion/templates/default` on every `scion
//     server start`, so emptying the file there does not last.
//   - DELETING the file makes scion treat the field as inline content and
//     launch with `--system-prompt 'system-prompt.md'`.
//   - `system_prompt_mode: none` is never consulted.
//
// scion rewrites only the `default` template, and for any other name it
// prepends the stock default as a base layer and resolves most-specific
// first, so lever's empty file wins for this one field and nothing else
// changes.
const (
	// LeverTemplateName is deliberately NOT "default": that name would
	// suppress scion's base-layer prepend and lose its agents.md, home/ and
	// skills/ entirely.
	LeverTemplateName = "lever"

	leverTemplateRel = layout.TemplatesRel + "/" + LeverTemplateName
)

// EnsureLeverTemplate creates the overlay template in the guest and reports
// whether it had to write anything.
//
// The directory holds exactly one file: an empty `system-prompt.md`. Empty is
// the entire point; do not "fix" it by adding content. It does NOT create a
// `scion-agent.yaml`: with none, the chain inherits the stock default's
// config.
func (g Guest) EnsureLeverTemplate(ctx context.Context) (bool, error) {
	// `: >` truncates in place rather than unlinking, so no reader ever
	// observes the path missing.
	script := fmt.Sprintf(
		`set -e; d="$HOME/%s"; f="$d/system-prompt.md"; `+
			`if [ -s "$f" ] || [ ! -f "$f" ]; then mkdir -p "$d" && : > "$f" && echo wrote; fi`,
		leverTemplateRel)
	res, err := g.UserRun(ctx, "bash", "-c", script)
	if err != nil {
		return false, fmt.Errorf("guest: create the %s agent template: %w", LeverTemplateName, err)
	}
	return strings.Contains(res.Stdout, "wrote"), nil
}
