package broker

import (
	"encoding/json"
	"net/http"
)

// Request body caps for the JSON routes. Every JSON handler decodes through
// decodeBody, so a route is never unbounded by omission.
const (
	// jailBodyLimit bounds the ordinary jail JSON routes (enrol/renew carry a
	// ~1 KiB PEM CSR; the rest are a few short strings).
	jailBodyLimit = 64 << 10
	// adminBodyLimit bounds /register (the largest admin body: a tool's op list).
	adminBodyLimit = 64 << 10
	// smallBodyLimit bounds the single-field routes (/revoke, /directive/*).
	smallBodyLimit = 4 << 10
	// signedBodyLimit bounds the directive admin channel's signed envelopes
	// (base64 statement + armored SSH signature).
	signedBodyLimit = 256 << 10
	// gatewayBodyLimit bounds one MCP JSON-RPC request on a tool route.
	gatewayBodyLimit = 4 << 20
)

// writeJSON encodes v as JSON to w with Content-Type set.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decodeBody decodes r's JSON body into v, reading at most limit bytes
// (http.MaxBytesReader, which also closes the connection on overflow). The
// caller audits and writes the response on error.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v)
}
