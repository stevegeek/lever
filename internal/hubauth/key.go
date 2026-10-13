package hubauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// maxKeyFile bounds the read: a P-256 PKCS#8 PEM is about 240 bytes.
const maxKeyFile = 16 << 10

// Key is the assertion signing key. It is admin-equivalent for the
// instance's hub.
type Key struct {
	priv *ecdsa.PrivateKey
}

// EnsureKey loads the key at path, or makes one when the file is absent.
// Every other problem with an existing file is an error: regenerating or
// re-moding a key someone else may have read would hide that.
func EnsureKey(path string) (k *Key, created bool, err error) {
	k, err = LoadKey(path)
	if err == nil {
		return k, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, fmt.Errorf("hubauth: key dir: %w", err)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("hubauth: generate key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, false, fmt.Errorf("hubauth: encode key: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".hub-auth-key-*")
	if err != nil {
		return nil, false, fmt.Errorf("hubauth: write key: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	werr := tmp.Chmod(0o600)
	if werr == nil {
		werr = pem.Encode(tmp, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return nil, false, fmt.Errorf("hubauth: write key: %w", werr)
	}
	// A hard link publishes without replacing: two first uses agree on the
	// winner's key instead of one silently overwriting a key already in use.
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			k, err := LoadKey(path)
			return k, false, err
		}
		return nil, false, fmt.Errorf("hubauth: write key: %w", err)
	}
	syncDir(dir)
	return &Key{priv: priv}, true, nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// LoadKey reads the key at path and never creates it. An absent file is an
// error wrapping fs.ErrNotExist.
func LoadKey(path string) (*Key, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("hubauth: key %s is a symlink", path)
		}
		return nil, fmt.Errorf("hubauth: key: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("hubauth: key: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("hubauth: key %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("hubauth: key %s must not be readable by group or others (mode %#o); "+
			"if it may have been read, delete it so lever makes a new one", path, perm)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, fmt.Errorf("hubauth: read key: %w", err)
	}
	if len(b) > maxKeyFile {
		return nil, fmt.Errorf("hubauth: key %s is too large", path)
	}
	block, rest := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("hubauth: key %s is not one PEM PRIVATE KEY block", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("hubauth: key %s: %w", path, err)
	}
	priv, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("hubauth: key %s is not a P-256 ECDSA key", path)
	}
	return &Key{priv: priv}, nil
}

func (k *Key) publicDER() []byte {
	der, err := x509.MarshalPKIXPublicKey(&k.priv.PublicKey)
	if err != nil {
		// A P-256 public key always marshals; LoadKey and EnsureKey admit
		// nothing else.
		panic("hubauth: marshal public key: " + err.Error())
	}
	return der
}

// PublicPEM is what the hub is configured with.
func (k *Key) PublicPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: k.publicDER()})
}

func (k *Key) Fingerprint() string {
	sum := sha256.Sum256(k.publicDER())
	return "sha256:" + hex.EncodeToString(sum[:])
}
