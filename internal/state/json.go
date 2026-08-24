package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/stevegeek/lever/internal/fsutil"
)

// LoadJSON reads a JSON-encoded state value from path; an absent file is the
// zero value. `what` names the state in error messages (e.g. "revocation").
func LoadJSON[T any](path, what string) (T, error) {
	var v T
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return v, fmt.Errorf("state: read %s: %w", what, err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("state: parse %s: %w", what, err)
	}
	return v, nil
}

// SaveJSON persists a JSON-encoded state value (0600), atomically. `what`
// names the state in error messages.
func SaveJSON[T any](path, what string, v T) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("state: marshal %s: %w", what, err)
	}
	if err := fsutil.WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("state: write %s: %w", what, err)
	}
	return nil
}
