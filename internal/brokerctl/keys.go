package brokerctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
)

// State is the host-side broker state directory (keys, revocation, pid, log).
type State struct{ Dir string }

// StateDir returns the .lever-state directory beside the config.
func StateDir(configDir string) State { return State{Dir: filepath.Join(configDir, ".lever-state")} }

func (s State) CACert() string        { return filepath.Join(s.Dir, "ca.crt") }
func (s State) CAKey() string         { return filepath.Join(s.Dir, "ca.key") }
func (s State) BrokerKey() string     { return filepath.Join(s.Dir, "broker.key") }
func (s State) BrokerPub() string     { return filepath.Join(s.Dir, "broker.pub") }
func (s State) Revocation() string    { return filepath.Join(s.Dir, "revocation.json") }
func (s State) Directives() string    { return filepath.Join(s.Dir, "directives.json") }
func (s State) DirectiveSock() string { return filepath.Join(s.Dir, "directive.sock") }
func (s State) PID() string           { return filepath.Join(s.Dir, "broker.pid") }
func (s State) Log() string           { return filepath.Join(s.Dir, "broker.log") }
func (s State) OutLog() string        { return filepath.Join(s.Dir, "broker.out.log") }
func (s State) ControllerPAT() string { return filepath.Join(s.Dir, "controller.pat") }

// EnsureKeys loads the CA + capability-signing root keypair from the state dir, generating
// and persisting them (0600 secrets) on first use. Reused across restarts so
// issued agent certs survive a broker restart.
func (s State) EnsureKeys() (token.KeyPair, *ca.CA, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return token.KeyPair{}, nil, fmt.Errorf("brokerctl: state dir: %w", err)
	}
	// Biscuit keypair.
	var kp token.KeyPair
	if _, err := os.Stat(s.BrokerKey()); errors.Is(err, os.ErrNotExist) {
		gen, gerr := token.Generate()
		if gerr != nil {
			return token.KeyPair{}, nil, gerr
		}
		if werr := gen.SavePrivate(s.BrokerKey()); werr != nil {
			return token.KeyPair{}, nil, werr
		}
		if werr := gen.SavePublic(s.BrokerPub()); werr != nil {
			return token.KeyPair{}, nil, werr
		}
		kp = gen
	} else {
		loaded, lerr := token.LoadPrivate(s.BrokerKey())
		if lerr != nil {
			return token.KeyPair{}, nil, lerr
		}
		kp = loaded
	}
	// CA.
	var caInst *ca.CA
	if _, err := os.Stat(s.CAKey()); errors.Is(err, os.ErrNotExist) {
		gen, gerr := ca.Generate()
		if gerr != nil {
			return token.KeyPair{}, nil, gerr
		}
		if werr := gen.SaveCert(s.CACert()); werr != nil {
			return token.KeyPair{}, nil, werr
		}
		if werr := gen.SaveKey(s.CAKey()); werr != nil {
			return token.KeyPair{}, nil, werr
		}
		caInst = gen
	} else {
		loaded, lerr := ca.Load(s.CACert(), s.CAKey())
		if lerr != nil {
			return token.KeyPair{}, nil, lerr
		}
		caInst = loaded
	}
	return kp, caInst, nil
}

// LoadRevocation reads the persisted revocation state; an absent file is the
// zero value (epoch 0, no revocations).
func (s State) LoadRevocation() (broker.RevocationState, error) {
	b, err := os.ReadFile(s.Revocation())
	if errors.Is(err, os.ErrNotExist) {
		return broker.RevocationState{}, nil
	}
	if err != nil {
		return broker.RevocationState{}, fmt.Errorf("brokerctl: read revocation: %w", err)
	}
	var rs broker.RevocationState
	if err := json.Unmarshal(b, &rs); err != nil {
		return broker.RevocationState{}, fmt.Errorf("brokerctl: parse revocation: %w", err)
	}
	return rs, nil
}

// SaveRevocation persists the revocation state (0600), atomically.
func (s State) SaveRevocation(rs broker.RevocationState) error {
	b, err := json.Marshal(rs)
	if err != nil {
		return fmt.Errorf("brokerctl: marshal revocation: %w", err)
	}
	if err := writeFileAtomic(s.Revocation(), b, 0o600); err != nil {
		return fmt.Errorf("brokerctl: write revocation: %w", err)
	}
	return nil
}

// LoadDirectives reads persisted directive state; absent file = zero value.
func (s State) LoadDirectives() (broker.DirectiveState, error) {
	b, err := os.ReadFile(s.Directives())
	if errors.Is(err, os.ErrNotExist) {
		return broker.DirectiveState{}, nil
	}
	if err != nil {
		return broker.DirectiveState{}, fmt.Errorf("brokerctl: read directives: %w", err)
	}
	var ds broker.DirectiveState
	if err := json.Unmarshal(b, &ds); err != nil {
		return broker.DirectiveState{}, fmt.Errorf("brokerctl: parse directives: %w", err)
	}
	return ds, nil
}

// SaveDirectives persists directive state (0600), atomically: a crash
// mid-write must never torn-write directives.json, since it holds the
// replay tombstone set the broker needs on restart.
func (s State) SaveDirectives(ds broker.DirectiveState) error {
	b, err := json.Marshal(ds)
	if err != nil {
		return fmt.Errorf("brokerctl: marshal directives: %w", err)
	}
	if err := writeFileAtomic(s.Directives(), b, 0o600); err != nil {
		return fmt.Errorf("brokerctl: write directives: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to a temp file in the same directory as path
// then renames it over path — atomic on POSIX, so a crash mid-write leaves
// either the old file or the new one, never a torn partial write.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
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

// SaveControllerPAT persists the scion controller personal access token
// (0600) under the state dir, creating the dir if needed.
func (s State) SaveControllerPAT(tok string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("brokerctl: state dir: %w", err)
	}
	if err := os.WriteFile(s.ControllerPAT(), []byte(tok), 0o600); err != nil {
		return fmt.Errorf("brokerctl: write controller.pat: %w", err)
	}
	return nil
}

// LoadControllerPAT reads the persisted controller PAT. An absent file
// returns ("", nil) so callers can branch on "" meaning "need to mint".
// A file present with permissions other than 0600 is treated as tampered
// or misconfigured and returns an error (mirrors the api_key_file
// defense-in-depth check in build.go).
func (s State) LoadControllerPAT() (string, error) {
	fi, err := os.Stat(s.ControllerPAT())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("brokerctl: controller.pat: %w", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		return "", fmt.Errorf("brokerctl: controller.pat must be 0600, got %#o", perm)
	}
	b, err := os.ReadFile(s.ControllerPAT())
	if err != nil {
		return "", fmt.Errorf("brokerctl: read controller.pat: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
