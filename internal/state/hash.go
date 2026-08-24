package state

import (
	"encoding/json"

	"github.com/stevegeek/lever/internal/skills"
)

// HashJSON digests the JSON encoding of v (sha256, hex — the same byte digest
// skills.Hash uses). A marshal failure yields "": every caller compares the
// result against a recorded hash, so "" is a guaranteed mismatch, which fails
// toward a restart.
func HashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return skills.Hash(b)
}
