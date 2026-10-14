package broker

import (
	"encoding/json"
	"net/http"
)

// Request body caps. Every JSON handler decodes through decodeBody, so a
// route is never unbounded by omission.
const (
	jailBodyLimit    = 64 << 10
	adminBodyLimit   = 64 << 10
	smallBodyLimit   = 4 << 10
	signedBodyLimit  = 256 << 10
	gatewayBodyLimit = 4 << 20
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decodeBody reads at most limit bytes. The caller audits and writes the
// response on error.
func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v)
}
