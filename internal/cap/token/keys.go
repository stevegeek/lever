// Package token mints and verifies per-agent biscuit capability tokens.
package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

// osStat is a package indirection so tests can assert file permissions.
var osStat = os.Stat

// KeyPair is the broker's biscuit root keypair (Ed25519). The private key is
// the forge; the public key is published for verification.
type KeyPair struct {
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

// Generate creates a fresh root keypair.
func Generate() (KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("token: generate keypair: %w", err)
	}
	return KeyPair{Public: pub, Private: priv}, nil
}

// SavePrivate writes the private key as hex, 0600 (it is the forge secret).
func (k KeyPair) SavePrivate(path string) error {
	enc := hex.EncodeToString(k.Private)
	if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
		return fmt.Errorf("token: save private: %w", err)
	}
	return nil
}

// SavePublic writes the public key as hex, 0644 (safe to publish).
func (k KeyPair) SavePublic(path string) error {
	enc := hex.EncodeToString(k.Public)
	if err := os.WriteFile(path, []byte(enc), 0o644); err != nil {
		return fmt.Errorf("token: save public: %w", err)
	}
	return nil
}

// LoadPrivate reads a hex private key and derives the public key from it.
func LoadPrivate(path string) (KeyPair, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return KeyPair{}, fmt.Errorf("token: read private: %w", err)
	}
	b, err := hex.DecodeString(string(raw))
	if err != nil {
		return KeyPair{}, fmt.Errorf("token: decode private: %w", err)
	}
	if len(b) != ed25519.PrivateKeySize {
		return KeyPair{}, fmt.Errorf("token: private key is %d bytes, want %d", len(b), ed25519.PrivateKeySize)
	}
	priv := ed25519.PrivateKey(b)
	return KeyPair{Private: priv, Public: priv.Public().(ed25519.PublicKey)}, nil
}

// EncodePublicKey renders an Ed25519 public key as hex (same wire form as
// SavePublic), for publishing over the broker's admin API.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return hex.EncodeToString(pub)
}

// DecodePublicKey parses a hex-encoded Ed25519 public key.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("token: decode public: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("token: public key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// LoadPublicKey reads a hex public key.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("token: read public: %w", err)
	}
	b, err := hex.DecodeString(string(raw))
	if err != nil {
		return nil, fmt.Errorf("token: decode public: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("token: public key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}
