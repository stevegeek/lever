package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// LLM-auth mode values: how an agent obtains its LLM credential. Deliberately
// not shared with scion's --harness-auth vocabulary, which is scion's contract
// and only coincides.
const (
	LLMAuthAPIKey       = "api-key"
	LLMAuthSubscription = "subscription"
)

// HarnessEnvOverlay returns the env overlay lever writes into the agent's
// claude settings.json. Boot and the renew sidecar must write identical keys,
// so add a key here, never at one call site.
//
// CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN forces Claude Code's classic renderer.
// Every route to a lever agent is a PTY onto the container's tmux, and the
// fullscreen default draws on the alternate screen, which tmux copy-mode and
// a browser terminal's scrollback cannot see. It must be set before the
// process starts: /tui default relaunches, and Claude Code refuses to
// relaunch with the --system-prompt flag scion's harness passes. Claude Code
// 2.1.226 honours it (no ESC[?1049h).
func HarnessEnvOverlay() map[string]string {
	return map[string]string{
		// "1" is the documented truthy form.
		"CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN": "1",
	}
}

var errEmptyLLMToken = errors.New("broker /request: empty token in response")

// RequestLLMToken obtains a capability(llm) token bound to cn from the broker.
// A blank token fails closed rather than silently break LLM auth. The token is
// already base64url-encoded and the proxy decodes it verbatim: do not
// re-encode it.
func RequestLLMToken(ctx context.Context, brokerURL string, client *http.Client, cn string) (string, error) {
	tok, err := Request(ctx, NormalizeBrokerURL(brokerURL), client, "llm", "generate", cn, nil)
	if err != nil {
		return "", err
	}
	if tok == "" {
		return "", errEmptyLLMToken
	}
	return tok, nil
}

// RefreshLLMToken obtains a fresh capability(llm) token for id from the broker
// and merges ANTHROPIC_AUTH_TOKEN + ANTHROPIC_BASE_URL into overlay. Boot calls
// it at first enrol and the renewal sidecar on every cert rotation.
//
// ANTHROPIC_BASE_URL points at the loopback gateway, not the broker: Claude
// caches the leaf for its lifetime, so a direct-broker URL would fail once
// the leaf expires. brokerURL is only the token-acquisition endpoint.
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
	overlay["ANTHROPIC_BASE_URL"] = llmBaseURL(LocalGatewayURL)
	return nil
}

// llmBaseURL's base must be the gateway URL, never the broker (see
// RefreshLLMToken).
func llmBaseURL(base string) string {
	return strings.TrimRight(base, "/") + "/llm"
}
