package broker

import (
	"encoding/json"
	"net/http"
)

// writeJSON encodes v as JSON to w with Content-Type set.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
