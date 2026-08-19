package agent

import (
	"path/filepath"
	"strings"
)

// LLM-auth mode values, lever-owned. These name how an agent obtains its LLM
// credential and are compared against the -llm-auth flag / BootConfig.LLMAuth /
// renewOpts.llmAuth throughout the agent binary.
//
// Deliberately NOT shared with scion's `--harness-auth api-key` CLI vocabulary
// (internal/scion/lifecycle.go) — that string is scion's wire contract; coupling
// the two constants would be false coupling. They happen to coincide today.
const (
	LLMAuthAPIKey       = "api-key"
	LLMAuthSubscription = "subscription"
)

// HarnessEnvOverlay returns the env overlay lever writes into the agent's
// claude settings.json. Boot writes it at first enrol and the renew sidecar
// rewrites it on every cert rotation; both must produce the IDENTICAL keys, so
// they share this one builder. Add a key here, never at one call site.
//
// Two kinds of key live here:
//
//   - the agent's identity files, by PATH (never the key bytes);
//   - CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN, which forces Claude Code's classic
//     renderer.
//
// The renderer flag is not a preference, it is a property of how a lever agent
// is reached. Claude Code renders fullscreen by default now, drawing on the
// terminal's ALTERNATE SCREEN, which by definition has no scrollback. Every
// route to a lever agent is a PTY onto the container's tmux — `lever attach`,
// or scion's web terminal over `lever remote` — and in both, fullscreen
// rendering destroys the scrollback the operator actually scrolls: tmux
// copy-mode cannot see alternate-screen content, and a browser terminal scrolls
// its own buffer, which holds the shell output sitting BEHIND the alternate
// screen rather than the conversation.
//
// The operator cannot fix this from inside the session either. `/tui default`
// relaunches the process, and Claude Code refuses to relaunch when the session
// carries flags it cannot pass on — a `--system-prompt` replacement among them,
// which is exactly what scion's harness passes (its stock `default` template
// ships a system-prompt.md reading `# Placeholder`). So the flag has to be set
// before the process starts, which means here.
//
// Verified live 2026-08-19 on claude 2.1.226 in the assistant's own jail:
// without it the agent emits ESC[?1049h (alternate screen), with it the agent
// does not. An earlier report that the variable does nothing predates 2.1.226.
func HarnessEnvOverlay(idDir string) map[string]string {
	return map[string]string{
		"CLAUDE_CODE_CLIENT_CERT": filepath.Join(idDir, "agent.crt"),
		"CLAUDE_CODE_CLIENT_KEY":  filepath.Join(idDir, "agent.key"),
		"NODE_EXTRA_CA_CERTS":     filepath.Join(idDir, "ca.crt"),
		// Classic renderer: see the doc above. "1" is the documented truthy form.
		"CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN": "1",
	}
}

// llmBaseURL is the ANTHROPIC_BASE_URL Claude posts to: the loopback gateway's
// /llm path (the gateway presents the rotating leaf on Claude's behalf). base is
// the gateway URL — never the broker (see RefreshLLMToken / Boot for why).
func llmBaseURL(base string) string {
	return strings.TrimRight(base, "/") + "/llm"
}
