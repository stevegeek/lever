package brokerctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
)

// State is the host-side broker state directory (keys, revocation, pid, log).
type State struct{ Dir string }

// StateDir returns the .lever-state directory beside the config.
func StateDir(configDir string) State { return State{Dir: filepath.Join(configDir, ".lever-state")} }

func (s State) CACert() string     { return filepath.Join(s.Dir, "ca.crt") }
func (s State) CAKey() string      { return filepath.Join(s.Dir, "ca.key") }
func (s State) BrokerKey() string  { return filepath.Join(s.Dir, "broker.key") }
func (s State) BrokerPub() string  { return filepath.Join(s.Dir, "broker.pub") }
func (s State) Revocation() string { return filepath.Join(s.Dir, "revocation.json") }
func (s State) PID() string        { return filepath.Join(s.Dir, "broker.pid") }
func (s State) Log() string        { return filepath.Join(s.Dir, "broker.log") }
func (s State) OutLog() string     { return filepath.Join(s.Dir, "broker.out.log") }

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

// SaveRevocation persists the revocation state (0600).
func (s State) SaveRevocation(rs broker.RevocationState) error {
	b, err := json.Marshal(rs)
	if err != nil {
		return fmt.Errorf("brokerctl: marshal revocation: %w", err)
	}
	if err := os.WriteFile(s.Revocation(), b, 0o600); err != nil {
		return fmt.Errorf("brokerctl: write revocation: %w", err)
	}
	return nil
}
