package captool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/mcp"
)

// handleToolsCall verifies the token independently of the broker, then runs
// the backstop.
func (s *Server) handleToolsCall(id any, msg map[string]any, caller string) []byte {
	if caller == "" {
		s.audit("", "", "deny", "missing X-Lever-Caller")
		return mcp.Error(id, mcp.CodeServerError, "forbidden")
	}
	// Release the lock before freshEpoch's network call.
	s.mu.Lock()
	pubKey := s.pubKey
	s.mu.Unlock()

	// Fail closed before Register: with no key nothing can be verified, and
	// token.Verify would panic. This runs before parsing so the audit line
	// carries no detail from the untrusted payload.
	if pubKey == nil {
		s.audit("", caller, "deny", "not registered")
		return mcp.Error(id, mcp.CodeServerError, "forbidden")
	}
	op, args, capB64, ok := mcp.ToolsCall(msg)
	if !ok || capB64 == "" {
		s.audit(op, caller, "deny", "missing capability or bad shape")
		return mcp.Error(id, mcp.CodeServerError, "forbidden")
	}
	o, known := s.ops[op]
	if !known {
		s.audit(op, caller, "deny", "unknown operation")
		return mcp.Error(id, mcp.CodeMethodNotFound, "method not found")
	}
	rawTok, err := base64.RawURLEncoding.DecodeString(capB64)
	if err != nil {
		s.audit(op, caller, "deny", "bad capability encoding")
		return mcp.Error(id, mcp.CodeServerError, "forbidden")
	}
	params := mcp.MapConstraintParams(o.CaveatParam, args)
	if err := token.Verify(pubKey, rawTok, token.Request{
		Caller: caller, Capability: token.Capability{Tool: s.name, Operation: op},
		Params: params, Now: time.Now(), MinEpoch: s.freshEpoch(context.Background()),
	}); err != nil {
		s.audit(op, caller, "deny", "verify: "+err.Error())
		return mcp.Error(id, mcp.CodeServerError, "forbidden")
	}
	vc := ValidatedContext{Caller: caller, Tool: s.name, Operation: op, Constraints: params}
	if o.Backstop != nil {
		// The backstop guards the args the Handler executes, not the
		// constraint-mapped params.
		if err := o.Backstop(vc, args); err != nil {
			s.audit(op, caller, "deny", "backstop: "+err.Error())
			return mcp.Error(id, mcp.CodeServerError, "forbidden")
		}
	}
	result, err := o.Handler(vc, args)
	if err != nil {
		s.audit(op, caller, "error", err.Error())
		return mcp.Error(id, mcp.CodeInternalError, "tool error")
	}
	s.audit(op, caller, "allow", "")
	payload, _ := json.Marshal(result)
	return mcp.TextResult(id, string(payload))
}
