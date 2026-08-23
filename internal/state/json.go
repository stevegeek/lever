package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if err := WriteFileAtomic(path, b, 0o600); err != nil {
		return fmt.Errorf("state: write %s: %w", what, err)
	}
	return nil
}

// WriteFileAtomic writes data to a temp file in the same directory as path
// then renames it over path — atomic on POSIX, so a crash mid-write leaves
// either the old file or the new one, never a torn partial write.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
