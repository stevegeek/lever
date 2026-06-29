package token

import (
	"path/filepath"
	"testing"
)

func TestKeyPairSaveLoadRoundTrip(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	priv := filepath.Join(dir, "cap.key")
	pub := filepath.Join(dir, "cap.pub")

	if err := kp.SavePrivate(priv); err != nil {
		t.Fatalf("save private: %v", err)
	}
	if err := kp.SavePublic(pub); err != nil {
		t.Fatalf("save public: %v", err)
	}

	loaded, err := LoadPrivate(priv)
	if err != nil {
		t.Fatalf("load private: %v", err)
	}
	if !loaded.Private.Equal(kp.Private) {
		t.Error("loaded private key differs")
	}
	if !loaded.Public.Equal(kp.Public) {
		t.Error("derived public key differs from original")
	}

	pubOnly, err := LoadPublicKey(pub)
	if err != nil {
		t.Fatalf("load public: %v", err)
	}
	if !pubOnly.Equal(kp.Public) {
		t.Error("loaded public key differs")
	}
}

func TestEncodeDecodePublicKeyRoundTrip(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	s := EncodePublicKey(kp.Public)
	got, err := DecodePublicKey(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(kp.Public) {
		t.Fatal("public key did not round-trip")
	}
}

func TestDecodePublicKeyRejectsWrongSize(t *testing.T) {
	if _, err := DecodePublicKey("abcd"); err == nil {
		t.Fatal("short hex must be rejected")
	}
}

func TestSavePrivateIsNotWorldReadable(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "cap.key")
	if err := kp.SavePrivate(p); err != nil {
		t.Fatal(err)
	}
	info, err := osStat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("private key mode = %o, want no group/other bits", info.Mode().Perm())
	}
}
