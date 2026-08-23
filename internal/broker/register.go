package broker

import (
	"encoding/json"
	"maps"
	"net/http"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/wire"
)

// handleRegister merges a first-party tool's registration against the
// CONFIG-AUTHORITATIVE envelope pre-loaded at boot (D4): the host config owns
// backend/allowed_values/FirstParty/permitted-ops; the tool supplies only
// caveat_param (the stored value, preserving single-source projection-agreement).
// A tool can never widen its own envelope, and an unconfigured tool is rejected
// before any registry write. Served only on the host-loopback admin listener.
func (b *Broker) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req wire.RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
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
		// External tools are registered from config at boot and are fronted,
		// not spawned — nothing legitimate self-registers under their name.
		b.audit("register", req.Name, "deny", "external tool does not self-register")
		http.Error(w, "external tool does not self-register", http.StatusForbidden)
		return
	}
	// Rebuild ops from the CONFIG op set; attach the body's caveat_param.
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
		cp := bodyCP[name]           // may be nil if the body didn't include this op
		if len(op.CaveatParam) > 0 { // config declared a guard — body must match
			if !maps.Equal(op.CaveatParam, cp) {
				b.audit("register", req.Name, "deny", "caveat_param mismatch for "+name)
				http.Error(w, "caveat_param mismatch", http.StatusForbidden)
				return
			}
			cp = op.CaveatParam
		}
		// Params come from CONFIG only (the declared guard) — a self-
		// registering tool cannot widen or drop the operator's declared set.
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
