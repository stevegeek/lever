package broker

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// routeHandler returns the gated reverse-proxy handler for a route: authorize
// (caller + biscuit + operation), strip the biscuit, optionally inject the
// configured secret, then forward to the backend with the route prefix removed.
func (b *Broker) routeHandler(rt Route) (http.Handler, error) {
	target, err := url.Parse(rt.Backend)
	if err != nil {
		return nil, fmt.Errorf("broker: route %q bad backend %q: %w", rt.Operation, rt.Backend, err)
	}

	var injectSecret string
	if rt.SecretName != "" {
		s, ok := b.policy.Secret(rt.SecretName)
		if !ok {
			return nil, fmt.Errorf("broker: route %q references unknown secret %q", rt.Operation, rt.SecretName)
		}
		injectSecret = s
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	base := rp.Director
	rp.Director = func(req *http.Request) {
		req.URL.Path = "/" + strings.TrimPrefix(strings.TrimPrefix(req.URL.Path, rt.PathPrefix), "/")
		base(req)
		req.Host = target.Host
		req.Header.Del("Authorization") // never leak the biscuit upstream
		if injectSecret != "" {
			req.Header.Set("Authorization", "Bearer "+injectSecret)
		}
		req.Header.Del("Cookie")
		req.Header.Del("X-Forwarded-For")
		req.Header.Del("X-Forwarded-Host")
		req.Header.Del("X-Forwarded-Proto")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, err := b.authorize(r, rt.Operation)
		if err != nil {
			b.audit(rt.Operation, "", "deny", err.Error())
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		b.audit(rt.Operation, caller, "allow", r.URL.Path)
		rp.ServeHTTP(w, r)
	}), nil
}
