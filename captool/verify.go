package captool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/mcp"
)

// handleToolsCall is the gated path: independent token verification + backstop.
// It returns the framed JSON-RPC reply.
func (s *Server) handleToolsCall(id any, msg map[string]any, caller string) []byte {
	if caller == "" {
		s.audit("", "", "deny", "missing X-Lever-Caller")
		return mcp.Error(id, -32000, "forbidden")
	}
	// Snapshot pubKey under the mutex so we see Register's write atomically and
	// avoid a data race (Register writes under s.mu; reads must also hold s.mu).
	// We release before any I/O (freshEpoch, token.Verify) so we never hold the
	// lock across network calls.
	s.mu.Lock()
	pubKey := s.pubKey
	s.mu.Unlock()

	// Fail closed: if Register has not yet run the server has no public key and
	// cannot verify anything — deny early with an audit trace rather than letting
	// token.Verify panic on a nil key.  Placement here (after caller, before op
	// extraction) keeps the audit message generic but avoids a partial parse that
	// could log misleading op/arg detail derived from an untrusted payload.
	if pubKey == nil {
		s.audit("", caller, "deny", "not registered")
		return mcp.Error(id, -32000, "forbidden")
	}
	op, args, capB64, ok := mcp.ToolsCall(msg)
	if !ok || capB64 == "" {
		s.audit(op, caller, "deny", "missing capability or bad shape")
		return mcp.Error(id, -32000, "forbidden")
	}
	o, known := s.ops[op]
	if !known {
		s.audit(op, caller, "deny", "unknown operation")
		return mcp.Error(id, -32601, "method not found")
	}
	rawTok, err := base64.RawURLEncoding.DecodeString(capB64)
	if err != nil {
		s.audit(op, caller, "deny", "bad capability encoding")
		return mcp.Error(id, -32000, "forbidden")
	}
	params := mcp.MapConstraintParams(o.CaveatParam, args)
	if err := token.Verify(pubKey, rawTok, token.Request{
		Caller: caller, Capability: token.Capability{Tool: s.name, Operation: op},
		Params: params, Now: time.Now(), MinEpoch: s.freshEpoch(context.Background()),
	}); err != nil {
		s.audit(op, caller, "deny", "verify: "+err.Error())
		return mcp.Error(id, -32000, "forbidden")
	}
	vc := ValidatedContext{Caller: caller, Tool: s.name, Operation: op, Constraints: params}
	if o.Backstop != nil {
		// Pass args (raw execution args), not params: the backstop guards what the
		// Handler actually executes, independent of token constraint mapping.
		if err := o.Backstop(vc, args); err != nil {
			s.audit(op, caller, "deny", "backstop: "+err.Error())
			return mcp.Error(id, -32000, "forbidden")
		}
	}
	result, err := o.Handler(vc, args)
	if err != nil {
		s.audit(op, caller, "error", err.Error())
		return mcp.Error(id, -32603, "tool error")
	}
	s.audit(op, caller, "allow", "")
	payload, _ := json.Marshal(result)
	return mcp.TextResult(id, string(payload))
}
