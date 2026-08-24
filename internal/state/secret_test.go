package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seededState returns a State whose dir exists and holds one file, written by
// pick (a State path method) with content and mode.
func seededState(t *testing.T, pick func(State) string, content []byte, mode os.FileMode) State {
	t.Helper()
	s := ForConfig(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pick(s), content, mode); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestControllerPATRoundTrip(t *testing.T) {
	s := ForConfig(t.TempDir())
	if err := s.SaveControllerPAT("pat-secret-123"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.ControllerPAT())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("controller.pat perms = %#o, want 0600", perm)
	}
	b, _ := os.ReadFile(s.ControllerPAT())
	if string(b) != "pat-secret-123" {
		t.Fatalf("controller.pat on disk = %q, want the token verbatim", b)
	}
	tok, err := s.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "pat-secret-123" {
		t.Fatalf("LoadControllerPAT() = %q, want %q", tok, "pat-secret-123")
	}
}

func TestLoadControllerPATAbsent(t *testing.T) {
	s := ForConfig(t.TempDir())
	tok, err := s.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		t.Fatalf("LoadControllerPAT() on absent file = %q, want empty", tok)
	}
}

func TestLoadControllerPATWrongPerms(t *testing.T) {
	s := seededState(t, State.ControllerPAT, []byte("pat-secret-123"), 0o644)
	if _, err := s.LoadControllerPAT(); err == nil {
		t.Fatal("LoadControllerPAT() with 0644 perms: want error, got nil")
	}
}

func TestRemotePATRoundTrip(t *testing.T) {
	s := State{Dir: t.TempDir()}
	if tok, err := s.LoadRemotePAT(); err != nil || tok != "" {
		t.Fatalf("absent PAT: got (%q, %v), want (\"\", nil)", tok, err)
	}
	if err := s.SaveRemotePAT("scion_pat_test"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.RemotePAT())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("remote.pat perms: %v %v", fi, err)
	}
	tok, err := s.LoadRemotePAT()
	if err != nil || tok != "scion_pat_test" {
		t.Fatalf("got (%q, %v)", tok, err)
	}
}

func TestLoadRemotePATRejectsLoosePerms(t *testing.T) {
	s := State{Dir: t.TempDir()}
	if err := s.SaveRemotePAT("scion_pat_test"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.RemotePAT(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadRemotePAT(); err == nil {
		t.Fatal("want perm error, got nil")
	}
}

// TestReadSecretTrims: a trailing newline (an operator-written file) is not
// part of the secret.
func TestReadSecretTrims(t *testing.T) {
	s := State{Dir: t.TempDir()}
	if err := WriteSecret(s.RemotePAT(), "remote.pat", "  tok\n"); err != nil {
		t.Fatal(err)
	}
	v, err := ReadSecret(s.RemotePAT(), "remote.pat")
	if err != nil || v != "tok" {
		t.Fatalf("ReadSecret = (%q, %v), want (\"tok\", nil)", v, err)
	}
}

// TestEnsureSessionSecretGeneratesOnce proves the generate-then-adopt cycle:
// first call creates the file 0600 with a 64-hex value; a second call returns
// the SAME value without rewriting the file (never-rotate — a rewrite would
// sign every browser session out). Values are proven by length/shape, never
// printed.
func TestEnsureSessionSecretGeneratesOnce(t *testing.T) {
	s := ForConfig(t.TempDir())
	v1, err := s.EnsureSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(v1) != 64 {
		t.Fatalf("generated secret length = %d, want 64 (32 bytes hex)", len(v1))
	}
	fi, err := os.Stat(s.SessionSecret())
	if err != nil {
		t.Fatalf("session-secret not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("session-secret perms = %#o, want 0600", perm)
	}
	before, err := os.ReadFile(s.SessionSecret())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != v1+"\n" {
		t.Fatalf("session-secret on disk = %q, want the hex value newline-terminated", before)
	}
	v2, err := s.EnsureSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	if v2 != v1 {
		t.Fatal("second EnsureSessionSecret returned a different value — must adopt, not rotate")
	}
	after, err := os.ReadFile(s.SessionSecret())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("second EnsureSessionSecret rewrote the file — must adopt, not rotate")
	}
}

// TestEnsureSessionSecretAdoptsExisting pins the pre-seed path: an operator
// places the live hub's key in the file (possibly with trailing whitespace),
// and lever adopts it trimmed, byte-for-byte untouched on disk.
func TestEnsureSessionSecretAdoptsExisting(t *testing.T) {
	seeded := []byte("operator-seeded-value\n")
	s := seededState(t, State.SessionSecret, seeded, 0o600)
	v, err := s.EnsureSessionSecret()
	if err != nil {
		t.Fatal(err)
	}
	if v != "operator-seeded-value" {
		t.Fatal("adopted value differs from the seeded one (want it whitespace-trimmed, otherwise verbatim)")
	}
	b, err := os.ReadFile(s.SessionSecret())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(seeded) {
		t.Fatal("EnsureSessionSecret rewrote an operator-seeded file")
	}
}

func TestEnsureSessionSecretRejectsLoosePerms(t *testing.T) {
	s := seededState(t, State.SessionSecret, []byte("whatever"), 0o644)
	if _, err := s.EnsureSessionSecret(); err == nil {
		t.Fatal("want perm error, got nil")
	}
}

func TestEnsureSessionSecretRejectsEmptyFile(t *testing.T) {
	s := seededState(t, State.SessionSecret, []byte(" \n"), 0o600)
	if _, err := s.EnsureSessionSecret(); err == nil {
		t.Fatal("empty session-secret must error, not silently regenerate")
	}
}

func TestReadRequiredSecretAbsentIsNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	if _, err := ReadRequiredSecret(path, "thing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadRequiredSecret(absent) = %v, want fs.ErrNotExist", err)
	}
	if v, err := ReadSecret(path, "thing"); err != nil || v != "" {
		t.Fatalf("ReadSecret(absent) = %q, %v; want \"\", nil", v, err)
	}
}
