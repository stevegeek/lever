package servicehost

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/stevegeek/lever/internal/fsutil"
)

// EnsureKeyPair returns the fingerprint of the key pair at certPath and
// keyPath, making one first when neither exists. created reports a new
// pair. With rotate it replaces an existing pair. One without the other is
// an error: it is not this function's to guess which half to trust.
func EnsureKeyPair(certPath, keyPath, cn string, eku x509.ExtKeyUsage, rotate bool) (fp Fingerprint, created bool, err error) {
	_, cerr := os.Stat(certPath)
	_, kerr := os.Stat(keyPath)
	certThere, keyThere := cerr == nil, kerr == nil
	for _, e := range []error{cerr, kerr} {
		if e != nil && !errors.Is(e, fs.ErrNotExist) {
			return "", false, e
		}
	}
	if certThere && keyThere && !rotate {
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			return "", false, err
		}
		keyPEM, err := readPrivate(keyPath)
		if err != nil {
			return "", false, err
		}
		_, fp, err := LoadKeyPair(certPEM, keyPEM)
		return fp, false, err
	}
	if certThere != keyThere && !rotate {
		return "", false, fmt.Errorf("only one of %s and %s exists; remove it to make a new pair", certPath, keyPath)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return "", false, err
	}
	certPEM, keyPEM, err := GenerateSelfSigned(cn, eku)
	if err != nil {
		return "", false, err
	}
	if err := fsutil.WriteFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return "", false, err
	}
	if err := fsutil.WriteFileAtomic(certPath, certPEM, 0o644); err != nil {
		return "", false, err
	}
	_, fp, err = LoadKeyPair(certPEM, keyPEM)
	return fp, true, err
}

// LoadClientKeyPair reads the broker's client pair for a service host,
// refusing a key others can read.
func LoadClientKeyPair(certPath, keyPath string) (cert tls.Certificate, fp Fingerprint, err error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return cert, "", err
	}
	keyPEM, err := readPrivate(keyPath)
	if err != nil {
		return cert, "", err
	}
	return LoadKeyPair(certPEM, keyPEM)
}
