package broker

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// llmOperation is the fixed grant required to use the Anthropic proxy leg.
const llmOperation = "llm"

// llmHandler proxies Anthropic API calls (api-key mode), swapping the agent's
// biscuit bearer for the real Console API key.
func (b *Broker) llmHandler() (http.Handler, error) {
	target, err := url.Parse(b.policy.LLM.Backend)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("broker: invalid LLM backend %q", b.policy.LLM.Backend)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	base := rp.Director
	rp.Director = func(req *http.Request) {
		req.URL.Path = "/" + strings.TrimPrefix(strings.TrimPrefix(req.URL.Path, "/llm/"), "/")
		base(req)
		req.Host = target.Host
		req.Header.Del("Authorization") // drop the biscuit
		req.Header.Set("x-api-key", b.policy.LLM.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Del("Cookie")
		req.Header.Del("X-Forwarded-For")
		req.Header.Del("X-Forwarded-Host")
		req.Header.Del("X-Forwarded-Proto")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := b.authorize(r, llmOperation)
		if err != nil {
			b.audit(llmOperation, "", "deny", err.Error())
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		b.audit(llmOperation, caller, "allow", r.URL.Path)
		rp.ServeHTTP(w, r)
	}), nil
}
