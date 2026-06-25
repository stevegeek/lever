package broker

import (
	"encoding/json"
	"maps"
	"net/http"

	"github.com/lever-to/lever/internal/broker/registry"
	"github.com/lever-to/lever/internal/cap/token"
)

// OperationSpec is one operation in a registration request.
type OperationSpec struct {
	Name        string            `json:"name"`
	CaveatParam map[string]string `json:"caveat_param,omitempty"`
}

// RegisterRequest is the body of POST /register (admin listener only).
type RegisterRequest struct {
	Name          string              `json:"name"`
	Backend       string              `json:"backend"`
	Operations    []OperationSpec     `json:"operations"`
	AllowedValues map[string][]string `json:"allowed_values,omitempty"`
	FirstParty    bool                `json:"first_party,omitempty"`
}

// RegisterResponse gives the registering tool the broker's verification key and
// current epoch, so captool can verify tokens independently + check freshness.
type RegisterResponse struct {
	PublicKey string `json:"public_key"`
	Epoch     int    `json:"epoch"`
}

// copyAllowedValues deep-copies a map[string][]string so the registry entry
// does not alias the caller's map (registry contract: caller must not mutate
// maps it hands to Register).
func copyAllowedValues(m map[string][]string) map[string][]string {
	if m == nil {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

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
	var req RegisterRequest
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
		merged[name] = registry.Operation{Name: name, CaveatParam: cp}
	}
	t := registry.Tool{
		Name: cfg.Name, Backend: cfg.Backend, AllowedValues: copyAllowedValues(cfg.AllowedValues),
		FirstParty: cfg.FirstParty, Operations: merged,
	}
	if err := b.reg.Register(t); err != nil {
		b.audit("register", req.Name, "deny", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.audit("register", req.Name, "allow", cfg.Backend)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(RegisterResponse{
		PublicKey: token.EncodePublicKey(b.keys.Public),
		Epoch:     b.MinEpoch(),
	})
}
