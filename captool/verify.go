package captool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lever-to/lever/internal/cap/token"
)

// ErrBackstop is the conventional error an Operation.Backstop returns to deny.
var ErrBackstop = errors.New("captool: backstop denied")

// handleToolsCall is the gated path: independent token verification + backstop.
func (s *Server) handleToolsCall(w http.ResponseWriter, id any, msg map[string]any, caller string) {
	if caller == "" {
		s.audit("", "", "deny", "missing X-Lever-Caller")
		writeRPCError(w, id, -32000, "forbidden")
		return
	}
	op, args, capB64, ok := toolsCallArgs(msg)
	if !ok || capB64 == "" {
		s.audit(op, caller, "deny", "missing capability or bad shape")
		writeRPCError(w, id, -32000, "forbidden")
		return
	}
	o, known := s.ops[op]
	if !known {
		s.audit(op, caller, "deny", "unknown operation")
		writeRPCError(w, id, -32601, "method not found")
		return
	}
	rawTok, err := base64.RawURLEncoding.DecodeString(capB64)
	if err != nil {
		s.audit(op, caller, "deny", "bad capability encoding")
		writeRPCError(w, id, -32000, "forbidden")
		return
	}
	params := mapConstraintParams(o.CaveatParam, args)
	if err := token.Verify(s.pubKey, rawTok, token.Request{
		Caller: caller, Capability: token.Capability{Tool: s.name, Operation: op},
		Params: params, Now: time.Now(), MinEpoch: s.freshEpoch(context.Background()),
	}); err != nil {
		s.audit(op, caller, "deny", "verify: "+err.Error())
		writeRPCError(w, id, -32000, "forbidden")
		return
	}
	vc := ValidatedContext{Caller: caller, Tool: s.name, Operation: op, Constraints: params}
	if o.Backstop != nil {
		if err := o.Backstop(vc, args); err != nil {
			s.audit(op, caller, "deny", "backstop: "+err.Error())
			writeRPCError(w, id, -32000, "forbidden")
			return
		}
	}
	result, err := o.Handler(vc, args)
	if err != nil {
		s.audit(op, caller, "error", err.Error())
		writeRPCError(w, id, -32603, "tool error")
		return
	}
	s.audit(op, caller, "allow", "")
	payload, _ := json.Marshal(result)
	writeRPCResult(w, id, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(payload)}},
	})
}

// toolsCallArgs extracts the operation name, canonical string arguments
// (non-string values json-encoded; _capability excluded), and the _capability.
// This MUST mirror the broker gateway's projection so verification agrees.
func toolsCallArgs(msg map[string]any) (string, map[string]string, string, bool) {
	params, ok := msg["params"].(map[string]any)
	if !ok {
		return "", nil, "", false
	}
	name, _ := params["name"].(string)
	rawArgs, _ := params["arguments"].(map[string]any)
	args := map[string]string{}
	capability := ""
	for k, v := range rawArgs {
		if k == "_capability" {
			capability, _ = v.(string)
			continue
		}
		if str, isStr := v.(string); isStr {
			args[k] = str
		} else {
			b, err := json.Marshal(v)
			if err != nil {
				return "", nil, "", false
			}
			args[k] = string(b)
		}
	}
	if name == "" {
		return "", nil, "", false
	}
	return name, args, capability, true
}

// mapConstraintParams turns request args into constraint-keyed params per the
// CaveatParam mapping (identity for keys not renamed). Mirrors registry.MapParams.
func mapConstraintParams(caveatParam map[string]string, args map[string]string) map[string]string {
	out := make(map[string]string, len(args))
	for k, v := range args {
		out[k] = v // identity mapping (constraint key == arg name)
	}
	for ck, argName := range caveatParam {
		if v, ok := args[argName]; ok {
			out[ck] = v
		}
	}
	return out
}
