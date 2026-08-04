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

// IdentityEnvOverlay returns the harness env overlay that points Claude at the
// agent's identity files by PATH (never the key bytes). Boot writes it at first
// enrol and the renew sidecar rewrites it on every cert rotation; both must
// produce the identical three keys, so they share this one builder.
func IdentityEnvOverlay(idDir string) map[string]string {
	return map[string]string{
		"CLAUDE_CODE_CLIENT_CERT": filepath.Join(idDir, "agent.crt"),
		"CLAUDE_CODE_CLIENT_KEY":  filepath.Join(idDir, "agent.key"),
		"NODE_EXTRA_CA_CERTS":     filepath.Join(idDir, "ca.crt"),
	}
}

// llmBaseURL is the ANTHROPIC_BASE_URL Claude posts to: the loopback gateway's
// /llm path (the gateway presents the rotating leaf on Claude's behalf). base is
// the gateway URL — never the broker (see RefreshLLMToken / Boot for why).
func llmBaseURL(base string) string {
	return strings.TrimRight(base, "/") + "/llm"
}
