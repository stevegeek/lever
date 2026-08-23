package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
// Today it carries one static key, CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN, which
// forces Claude Code's classic renderer; Boot and RefreshLLMToken add the
// dynamic ANTHROPIC_* keys in api-key mode. (The identity file paths used to be
// written here too, as CLAUDE_CODE_CLIENT_CERT/_KEY + NODE_EXTRA_CA_CERTS, but
// Claude reaches the broker through the plaintext loopback gateway and never
// presents the leaf itself; the image's Dockerfile still exports the same paths
// as container env for any other in-container tooling.)
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
func HarnessEnvOverlay() map[string]string {
	return map[string]string{
		// Classic renderer: see the doc above. "1" is the documented truthy form.
		"CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN": "1",
	}
}

// RequestLLMToken obtains a capability(llm) token bound to cn from the broker
// /request endpoint over the enrolled mTLS client. It is Request with two things
// added: brokerURL normalization (NormalizeBrokerURL) and the empty-token guard —
// a blank ANTHROPIC_AUTH_TOKEN must fail closed rather than silently break LLM
// auth. The broker returns the token already base64url-encoded, and the proxy's
// bearerToken decodes the value after "Bearer " verbatim, so it is returned as
// is: do NOT re-encode it.
func RequestLLMToken(ctx context.Context, brokerURL string, client *http.Client, cn string) (string, error) {
	tok, err := Request(ctx, NormalizeBrokerURL(brokerURL), client, "llm", "generate", cn, nil)
	if err != nil {
		return "", err
	}
	if tok == "" {
		return "", errors.New("broker /request: empty token in response")
	}
	return tok, nil
}

// RefreshLLMToken obtains a fresh capability(llm) token for id from the broker
// and merges ANTHROPIC_AUTH_TOKEN + ANTHROPIC_BASE_URL into overlay. Boot calls
// it at first enrol and the renewal sidecar on every cert rotation in api-key
// mode, so both write the same two keys.
//
// ANTHROPIC_BASE_URL points at the in-container loopback gateway, NOT the
// broker: the gateway presents the rotating agent leaf on Claude's behalf, so a
// direct-broker URL would resurrect the 24h cached-leaf outage. brokerURL is
// only the token-acquisition endpoint.
//
// Fail closed: overlay is only mutated after a successful token acquisition.
func RefreshLLMToken(ctx context.Context, brokerURL string, id Identity, overlay map[string]string) error {
	cn, err := id.CN()
	if err != nil {
		return fmt.Errorf("agent: refresh llm token: %w", err)
	}
	client, err := id.Client()
	if err != nil {
		return fmt.Errorf("agent: refresh llm token: build mTLS client: %w", err)
	}
	tok, err := RequestLLMToken(ctx, brokerURL, client, cn)
	if err != nil {
		return fmt.Errorf("agent: refresh llm token: obtain: %w", err)
	}
	overlay["ANTHROPIC_AUTH_TOKEN"] = tok
	// Claude posts to the loopback gateway, which proxies /llm to the broker.
	overlay["ANTHROPIC_BASE_URL"] = llmBaseURL(LocalGatewayURL)
	return nil
}

// llmBaseURL is the ANTHROPIC_BASE_URL Claude posts to: the loopback gateway's
// /llm path (the gateway presents the rotating leaf on Claude's behalf). base is
// the gateway URL — never the broker (see RefreshLLMToken for why).
func llmBaseURL(base string) string {
	return strings.TrimRight(base, "/") + "/llm"
}
