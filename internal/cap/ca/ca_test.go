package ca

import (
	"path/filepath"
	"testing"
)

func TestCASaveLoadRoundTrip(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if err := c.SaveCert(certPath); err != nil {
		t.Fatalf("save cert: %v", err)
	}
	if err := c.SaveKey(keyPath); err != nil {
		t.Fatalf("save key: %v", err)
	}

	loaded, err := Load(certPath, keyPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	certPEM, _, err := loaded.IssueAgentCert("scratch")
	if err != nil {
		t.Fatalf("issue after load: %v", err)
	}
	if len(certPEM) == 0 {
		t.Fatal("empty issued cert")
	}
	if loaded == nil {
		t.Fatal("nil CA after load")
	}
}

func TestSaveKeyIsNotWorldReadable(t *testing.T) {
	c, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.key")
	if err := c.SaveKey(p); err != nil {
		t.Fatal(err)
	}
	info, err := osStat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("CA key mode = %o, want no group/other bits", info.Mode().Perm())
	}
}
