package state

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/fsutil"
)

// ReadSecret reads a 0600 secret file, whitespace-trimmed. An absent file
// returns ("", nil), meaning "need to mint"; use ReadRequiredSecret when
// absence is an error. Any mode other than 0600 is treated as tampered or
// misconfigured and is an error.
func ReadSecret(path, what string) (string, error) {
	v, err := ReadRequiredSecret(path, what)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return v, err
}

// ReadRequiredSecret is ReadSecret for operator-supplied files that must
// exist: absence is an error wrapping fs.ErrNotExist.
func ReadRequiredSecret(path, what string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("state: %s: %w", what, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		return "", fmt.Errorf("state: %s must be 0600, got %#o", what, perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("state: read %s: %w", what, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// WriteSecret persists v at path atomically, 0600, creating the parent
// directory 0700 if needed.
func WriteSecret(path, what, v string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("state: state dir: %w", err)
	}
	if err := fsutil.WriteFileAtomic(path, []byte(v), 0o600); err != nil {
		return fmt.Errorf("state: write %s: %w", what, err)
	}
	return nil
}

func (s State) SaveControllerPAT(tok string) error {
	return WriteSecret(s.ControllerPAT(), "controller.pat", tok)
}

// LoadControllerPAT: see ReadSecret for the absent/permission contract.
func (s State) LoadControllerPAT() (string, error) {
	return ReadSecret(s.ControllerPAT(), "controller.pat")
}

func (s State) SaveRemotePAT(tok string) error {
	return WriteSecret(s.RemotePAT(), "remote.pat", tok)
}

// LoadRemotePAT: see ReadSecret for the absent/permission contract.
func (s State) LoadRemotePAT() (string, error) {
	return ReadSecret(s.RemotePAT(), "remote.pat")
}

// EnsureSessionSecret returns the hub's session-cookie signing key,
// generating 32 random bytes (hex, 0600) on first use. An existing file is
// adopted and never rewritten: rotating the key signs every browser session
// out, so rotation is the operator's move (delete the file). An empty file
// is an error rather than a regeneration, for the same reason.
func (s State) EnsureSessionSecret() (string, error) {
	v, err := ReadSecret(s.SessionSecret(), "session-secret")
	if err != nil {
		return "", err
	}
	if v != "" {
		return v, nil
	}
	if _, err := os.Stat(s.SessionSecret()); err == nil {
		return "", errors.New("state: session-secret is empty — delete the file to generate a new key")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("state: session-secret: %w", err)
	}
	v = hex.EncodeToString(raw[:])
	if err := WriteSecret(s.SessionSecret(), "session-secret", v+"\n"); err != nil {
		return "", err
	}
	return v, nil
}
