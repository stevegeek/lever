package broker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/lever-to/lever/internal/broker/registry"
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
// identity (mTLS); normalizing the op to the wildcard when the tool is
// coarse-gated (registry has WildcardOp registered); the request/delegation
// policy (rules.MayObtain, checked against the normalized op — no grant
// widening, the caller must hold the exact {tool, op} grant); the operation
// is registered (registry.HasOperation); and the requested constraint values
// are permitted (registry.ValidateConstraints). The token is bound to
// BoundTo and carries the normalized op. Fails closed at every gate.
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
	// Agents cannot know a tool's gate, so a fine-shaped request (a real MCP
	// op name, e.g. "get_weather") against a coarse tool must be normalized to
	// the wildcard op BEFORE the policy check. A tool exposes WildcardOp in
	// the registry only when it is coarse-gated (an existing reviewed
	// invariant enforced at registration), so this branch never fires for a
	// fine tool. Crucially this does NOT widen the grant: MayObtain below
	// still requires the caller to hold the exact {tool, "*"} grant, and the
	// original op is preserved (requestedOp) purely for the audit trail.
	requestedOp := req.Op
	if b.reg.HasOperation(req.Tool, registry.WildcardOp) {
		req.Op = registry.WildcardOp
	}
	if !b.rules.MayObtain(caller, req.BoundTo, req.Tool, req.Op) {
		detail := fmt.Sprintf("policy: may not obtain/delegate (tool=%s op=%s", req.Tool, requestedOp)
		if requestedOp != req.Op {
			detail += fmt.Sprintf(" coerced_to=%s", req.Op)
		}
		if req.BoundTo != caller {
			detail += fmt.Sprintf(" bound_to=%s", req.BoundTo)
		}
		detail += ")"
		b.audit("request", caller, "deny", detail)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !b.reg.HasOperation(req.Tool, req.Op) {
		detail := fmt.Sprintf("unregistered op (tool=%s op=%s", req.Tool, requestedOp)
		if requestedOp != req.Op {
			detail += fmt.Sprintf(" coerced_to=%s", req.Op)
		}
		if req.BoundTo != caller {
			detail += fmt.Sprintf(" bound_to=%s", req.BoundTo)
		}
		detail += ")"
		b.audit("request", caller, "deny", detail)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err := b.reg.ValidateConstraints(req.Tool, req.Constraints); err != nil {
		detail := fmt.Sprintf("%s (tool=%s op=%s", err.Error(), req.Tool, requestedOp)
		if requestedOp != req.Op {
			detail += fmt.Sprintf(" coerced_to=%s", req.Op)
		}
		if req.BoundTo != caller {
			detail += fmt.Sprintf(" bound_to=%s", req.BoundTo)
		}
		detail += ")"
		b.audit("request", caller, "deny", detail)
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
	detail := req.Tool + "." + req.Op + "->" + req.BoundTo
	if requestedOp != req.Op {
		detail += " (op coerced: " + requestedOp + " -> " + req.Op + ")"
	}
	b.audit("request", caller, "allow", detail)
}
