package broker

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/lever-to/lever/internal/cap/ca"
	"github.com/lever-to/lever/internal/cap/token"
)

// CapRequest is the body of POST /request: an agent asking to mint a capability
// for itself (BoundTo == caller) or to delegate one (BoundTo == another agent).
type CapRequest struct {
	Tool        string            `json:"tool"`
	Op          string            `json:"op"`
	BoundTo     string            `json:"bound_to"`
	Constraints map[string]string `json:"constraints,omitempty"`
}

// CapResponse carries the minted capability token (base64url signed token).
type CapResponse struct {
	Token string `json:"token"`
}

// handleRequest mints a capability token after checking, in order: the caller's
// identity (mTLS), the request/delegation policy (rules.MayObtain), the
// operation is registered (registry.HasOperation), and the requested constraint
// values are permitted (registry.ValidateConstraints). The token is bound to
// BoundTo. Fails closed at every gate.
func (b *Broker) handleRequest(w http.ResponseWriter, r *http.Request) {
	caller, err := ca.RequireAgent(r)
	if err != nil {
		b.audit("request", "", "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req CapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		b.audit("request", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.BoundTo == "" {
		req.BoundTo = caller // default: self-obtain
	}
	if !b.rules.MayObtain(caller, req.BoundTo, req.Tool, req.Op) {
		b.audit("request", caller, "deny", "policy: may not obtain/delegate")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !b.reg.HasOperation(req.Tool, req.Op) {
		b.audit("request", caller, "deny", "unregistered op")
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := b.reg.ValidateConstraints(req.Tool, req.Constraints); err != nil {
		b.audit("request", caller, "deny", err.Error())
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	cons := make([]token.Constraint, 0, len(req.Constraints))
	for k, v := range req.Constraints {
		cons = append(cons, token.Constraint{Key: k, Value: v})
	}
	tok, err := token.Mint(b.keys.Private, token.Grant{
		Agent:       req.BoundTo,
		Capability:  token.Capability{Tool: req.Tool, Operation: req.Op},
		Constraints: cons,
		Expiry:      time.Now().Add(b.grantTTL),
		Epoch:       b.MinEpoch(),
	})
	if err != nil {
		b.audit("request", caller, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CapResponse{Token: base64.RawURLEncoding.EncodeToString(tok)})
	b.audit("request", caller, "allow", req.Tool+"."+req.Op+"->"+req.BoundTo)
}
