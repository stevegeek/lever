package state

import (
	"encoding/json"

	"github.com/stevegeek/lever/internal/skills"
)

// HashJSON digests the JSON encoding of v (sha256, hex, as skills.Hash). A
// marshal failure yields "": callers compare against a recorded hash, so ""
// is a guaranteed mismatch that fails toward a restart.
func HashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return skills.Hash(b)
}
