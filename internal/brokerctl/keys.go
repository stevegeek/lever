package brokerctl

import (
	"errors"
	"fmt"
	"os"

	"github.com/stevegeek/lever/internal/cap/ca"
	"github.com/stevegeek/lever/internal/cap/token"
	"github.com/stevegeek/lever/internal/state"
)

// EnsureKeys loads the CA + capability-signing root keypair from the state dir,
// generating and persisting them (0600 secrets) on first use. Reused across
// restarts so issued agent certs survive a broker restart.
func EnsureKeys(st state.State) (token.KeyPair, *ca.CA, error) {
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return token.KeyPair{}, nil, fmt.Errorf("brokerctl: state dir: %w", err)
	}
	kp, err := loadOrGenerate(st.BrokerKey(),
		func() (token.KeyPair, error) { return token.LoadPrivate(st.BrokerKey()) },
		func() (token.KeyPair, error) {
			gen, err := token.Generate()
			if err != nil {
				return token.KeyPair{}, err
			}
			if err := gen.SavePrivate(st.BrokerKey()); err != nil {
				return token.KeyPair{}, err
			}
			return gen, gen.SavePublic(st.BrokerPub())
		})
	if err != nil {
		return token.KeyPair{}, nil, err
	}
	caInst, err := loadOrGenerate(st.CAKey(),
		func() (*ca.CA, error) { return ca.Load(st.CACert(), st.CAKey()) },
		func() (*ca.CA, error) {
			gen, err := ca.Generate()
			if err != nil {
				return nil, err
			}
			if err := gen.SaveCert(st.CACert()); err != nil {
				return nil, err
			}
			return gen, gen.SaveKey(st.CAKey())
		})
	if err != nil {
		return token.KeyPair{}, nil, err
	}
	return kp, caInst, nil
}

// loadOrGenerate returns load() when keyPath exists, else generate() — which
// is expected to persist what it makes so the next call loads it.
func loadOrGenerate[T any](keyPath string, load, generate func() (T, error)) (T, error) {
	if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
		return generate()
	}
	return load()
}
