package state

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadSecret reads a 0600 secret file, whitespace-trimmed. An absent file
// returns ("", nil) so callers can branch on "" meaning "need to mint"; use
// ReadRequiredSecret when absence is an error. A file present with
// permissions other than 0600 is treated as tampered or misconfigured and
// returns an error (mirrors the api_key_file defense-in-depth check in
// brokerctl). `what` names the file in errors.
func ReadSecret(path, what string) (string, error) {
	v, err := ReadRequiredSecret(path, what)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return v, err
}

// ReadRequiredSecret is ReadSecret for operator-supplied files that must
// exist: an absent file is an error (wrapping fs.ErrNotExist) rather than
// "". Callers that need to distinguish absent from empty use this.
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

// WriteSecret persists v at path (0600, atomically), creating the parent
// directory (0700) if needed. `what` names the file in errors.
func WriteSecret(path, what, v string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("state: state dir: %w", err)
	}
	if err := WriteFileAtomic(path, []byte(v), 0o600); err != nil {
		return fmt.Errorf("state: write %s: %w", what, err)
	}
	return nil
}

// SaveControllerPAT persists the scion controller personal access token
// (0600) under the state dir, creating the dir if needed.
func (s State) SaveControllerPAT(tok string) error {
	return WriteSecret(s.ControllerPAT(), "controller.pat", tok)
}

// LoadControllerPAT reads the persisted controller PAT; see ReadSecret for
// the absent/permission contract.
func (s State) LoadControllerPAT() (string, error) {
	return ReadSecret(s.ControllerPAT(), "controller.pat")
}

// SaveRemotePAT persists the proxy remote personal access token (0600)
// under the state dir, creating the dir if needed.
func (s State) SaveRemotePAT(tok string) error {
	return WriteSecret(s.RemotePAT(), "remote.pat", tok)
}

// LoadRemotePAT reads the persisted remote PAT; see ReadSecret for the
// absent/permission contract.
func (s State) LoadRemotePAT() (string, error) {
	return ReadSecret(s.RemotePAT(), "remote.pat")
}

// EnsureSessionSecret returns the hub's session-cookie signing key, generating
// 32 random bytes hex-encoded and persisting them (0600) on first use. An
// existing file is adopted verbatim (whitespace-trimmed) and NEVER rewritten —
// rotating the key signs every browser session out, so rotation is the
// operator's move: delete the file and the next apply generates a fresh one.
// The 0600 gate is ReadSecret's; an empty file is an error rather than a
// silent regeneration, because overwriting a file the operator placed would
// violate the never-rewrite contract.
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
