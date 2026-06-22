// Package broker is the host-side capability broker: it mints per-agent biscuit
// capabilities from policy and authorizes/proxies agent calls so real secrets
// never enter a container.
package broker

import (
	"strings"
	"time"
)

// LLMMode selects how the Anthropic credential is handled.
type LLMMode string

const (
	LLMSubscription LLMMode = "subscription" // token stays in-container (carve-out); broker LLM proxy disabled
	LLMAPIKey       LLMMode = "api-key"      // broker proxies Anthropic, injecting the real API key
)

// Route maps a request path prefix to a backend, gated by an operation string.
type Route struct {
	Operation  string // the grant checked against the biscuit (e.g. "qmd", "github.api")
	PathPrefix string // e.g. "/mcp/qmd/"
	Backend    string // upstream base URL, e.g. "http://127.0.0.1:3101"
	SecretName string // optional: secret to inject as "Authorization: Bearer"; "" = none
}

// LLMConfig configures the optional Anthropic proxy leg.
type LLMConfig struct {
	Mode    LLMMode
	APIKey  string // api-key mode only
	Backend string // e.g. "https://api.anthropic.com"
}

// Policy is the broker's authoritative configuration (built host-side from
// lever.yaml). It is the single source of what each grove may do.
type Policy struct {
	ManagerIdentity string              // mTLS CN allowed to call /provision
	Grants          map[string][]string // grove -> allowed operations
	Routes          []Route
	Secrets         map[string]string // secret name -> value
	LLM             LLMConfig
	GrantTTL        time.Duration // biscuit lifetime; defaults applied in New()
}

// GrantsFor returns the operations a grove may perform.
func (p Policy) GrantsFor(grove string) ([]string, bool) {
	g, ok := p.Grants[grove]
	return g, ok
}

// RouteForPath returns the route whose PathPrefix is the longest match for path.
func (p Policy) RouteForPath(path string) (Route, bool) {
	var best Route
	found := false
	for _, r := range p.Routes {
		if strings.HasPrefix(path, r.PathPrefix) && len(r.PathPrefix) > len(best.PathPrefix) {
			best, found = r, true
		}
	}
	return best, found
}

// Secret returns a configured secret value by name.
func (p Policy) Secret(name string) (string, bool) {
	v, ok := p.Secrets[name]
	return v, ok
}
