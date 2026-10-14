package broker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/stevegeek/lever/internal/broker/registry"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/wire"
)

// denyDetail is both the audit detail and the 403 body. The op shown is the
// ORIGINAL (pre-coercion) op.
func denyDetail(prefix string, req wire.CapRequest, requestedOp, caller string) string {
	detail := fmt.Sprintf("%s (tool=%s op=%s", prefix, req.Tool, requestedOp)
	if requestedOp != req.Op {
		detail += fmt.Sprintf(" coerced_to=%s", req.Op)
	}
	if req.BoundTo != caller {
		detail += fmt.Sprintf(" bound_to=%s", req.BoundTo)
	}
	return detail + ")"
}

// handleRequest mints a capability token bound to BoundTo. The caller must
// hold the exact {tool, op} grant for the normalized op, the op must be
// registered, and the constraint values permitted. Fails closed at every
// gate.
func (b *Broker) handleRequest(w http.ResponseWriter, r *http.Request) {
	// A revoked agent cannot mint OR delegate: a delegation to a non-revoked
	// agent would otherwise survive revocation.
	caller, ok := b.requireLiveAgent(w, r, "request", "")
	if !ok {
		return
	}
	var req wire.CapRequest
	if err := decodeBody(w, r, jailBodyLimit, &req); err != nil {
		b.audit("request", caller, "deny", "bad body")
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.BoundTo == "" {
		req.BoundTo = caller
	}
	// An agent cannot know a tool's gate, so a real op name against a coarse
	// tool is normalized to the wildcard BEFORE the policy check. Only a
	// coarse tool registers WildcardOp, and this does NOT widen the grant:
	// the caller must still hold the exact {tool, "*"} grant.
	requestedOp := req.Op
	if b.reg.HasOperation(req.Tool, registry.WildcardOp) {
		req.Op = registry.WildcardOp
	}
	rule, allowed := b.rules.MayObtainRule(caller, req.BoundTo, req.Tool, req.Op)
	if !allowed {
		detail := denyDetail("policy: may not obtain/delegate", req, requestedOp, caller)
		b.audit("request", caller, "deny", detail)
		http.Error(w, detail, http.StatusForbidden)
		return
	}
	if !b.reg.HasOperation(req.Tool, req.Op) {
		detail := denyDetail("unregistered op", req, requestedOp, caller)
		b.audit("request", caller, "deny", detail)
		http.Error(w, detail, http.StatusForbidden)
		return
	}
	if err := b.reg.ValidateConstraints(req.Tool, req.Op, req.Constraints); err != nil {
		detail := denyDetail(err.Error(), req, requestedOp, caller)
		b.audit("request", caller, "deny", detail)
		http.Error(w, detail, http.StatusForbidden)
		return
	}
	cons := make([]token.Constraint, 0, len(req.Constraints))
	for k, v := range req.Constraints {
		cons = append(cons, token.Constraint{Key: k, Value: v})
	}
	expiry := time.Now().Add(b.grantTTL)
	epoch := b.MinEpoch()
	tok, err := token.Mint(b.keys.Private, token.Grant{
		Agent:       req.BoundTo,
		Capability:  token.Capability{Tool: req.Tool, Operation: req.Op},
		Constraints: cons,
		Expiry:      expiry,
		Epoch:       epoch,
	})
	if err != nil {
		b.audit("request", caller, "error", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	detail := req.Tool + "." + req.Op + "->" + req.BoundTo
	if requestedOp != req.Op {
		detail += " (op coerced: " + requestedOp + " -> " + req.Op + ")"
	}
	// The allow line is the mint ledger, written BEFORE the token leaves the
	// broker so no handed-out token lacks a mint line. The token itself is
	// never logged. Constraints are JSON so any value stays unambiguous.
	kvs := []any{
		"id", token.ID(tok),
		"rule", rule,
		"exp", expiry.UTC().Format(time.RFC3339),
		"epoch", epoch,
	}
	if len(req.Constraints) > 0 {
		cj, err := json.Marshal(req.Constraints)
		if err != nil {
			b.audit("request", caller, "error", "marshal constraints: "+err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		kvs = append(kvs, "constraints", string(cj))
	}
	b.audit("request", caller, "allow", detail, kvs...)
	writeJSON(w, wire.CapResponse{Token: base64.RawURLEncoding.EncodeToString(tok)})
}
