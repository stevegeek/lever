package broker

import (
	"maps"
	"net/http"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/wire"
)

// handleRegister merges a first-party tool's registration into the
// CONFIG-AUTHORITATIVE envelope loaded at boot: config owns everything but
// caveat_param, so a tool can never widen its own envelope, and an
// unconfigured tool is rejected before any registry write. Admin listener
// only.
func (b *Broker) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req wire.RegisterRequest
	if err := decodeBody(w, r, adminBodyLimit, &req); err != nil {
		b.audit("register", "", "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cfg, ok := b.reg.Lookup(req.Name)
	if !ok {
		b.audit("register", req.Name, "deny", "tool not configured")
		http.Error(w, "tool not configured", http.StatusForbidden)
		return
	}
	if cfg.External {
		// Nothing legitimate self-registers under an external tool's name.
		b.audit("register", req.Name, "deny", "external tool does not self-register")
		http.Error(w, "external tool does not self-register", http.StatusForbidden)
		return
	}
	bodyCP := make(map[string]map[string]string, len(req.Operations))
	for _, o := range req.Operations {
		if _, known := cfg.Operations[o.Name]; !known {
			b.audit("register", req.Name, "deny", "operation not configured: "+o.Name)
			http.Error(w, "operation not configured", http.StatusForbidden)
			return
		}
		bodyCP[o.Name] = o.CaveatParam
	}
	merged := make(map[string]registry.Operation, len(cfg.Operations))
	for name, op := range cfg.Operations {
		cp := bodyCP[name]
		if len(op.CaveatParam) > 0 { // a config guard: the body must match
			if !maps.Equal(op.CaveatParam, cp) {
				b.audit("register", req.Name, "deny", "caveat_param mismatch for "+name)
				http.Error(w, "caveat_param mismatch", http.StatusForbidden)
				return
			}
			cp = op.CaveatParam
		}
		// Params come from config only: a tool cannot widen or drop them.
		merged[name] = registry.Operation{Name: name, CaveatParam: cp, Params: op.Params}
	}
	t := registry.Tool{
		Name: cfg.Name, Backend: cfg.Backend, AllowedValues: registry.CloneAllowedValues(cfg.AllowedValues),
		FirstParty: cfg.FirstParty, Operations: merged,
	}
	if err := b.reg.Register(t); err != nil {
		b.audit("register", req.Name, "deny", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.audit("register", req.Name, "allow", cfg.Backend)
	writeJSON(w, wire.RegisterResponse{
		PublicKey: token.EncodePublicKey(b.keys.Public),
		Epoch:     b.MinEpoch(),
	})
}
