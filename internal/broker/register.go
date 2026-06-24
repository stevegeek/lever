package broker

import (
	"encoding/json"
	"net/http"

	"github.com/lever-to/lever/internal/broker/registry"
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
}

// handleRegister records a first-party tool's backend, operations, caveat→param
// mapping, and allowed values. Served only on the host-loopback admin listener.
func (b *Broker) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ops := make(map[string]registry.Operation, len(req.Operations))
	for _, o := range req.Operations {
		ops[o.Name] = registry.Operation{Name: o.Name, CaveatParam: o.CaveatParam}
	}
	t := registry.Tool{Name: req.Name, Backend: req.Backend, Operations: ops, AllowedValues: req.AllowedValues}
	if err := b.reg.Register(t); err != nil {
		b.audit("register", req.Name, "deny", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.audit("register", req.Name, "allow", req.Backend)
	w.WriteHeader(http.StatusOK)
}
