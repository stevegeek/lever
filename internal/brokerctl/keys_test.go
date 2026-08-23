package brokerctl

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker"
)

func TestEnsureKeysGeneratesThenReloads(t *testing.T) {
	s := StateDir(t.TempDir())
	kp1, ca1, err := s.EnsureKeys()
	if err != nil {
		t.Fatal(err)
	}
	kp2, ca2, err := s.EnsureKeys() // second call must reload, not regenerate
	if err != nil {
		t.Fatal(err)
	}
	if string(kp1.Public) != string(kp2.Public) {
		t.Fatal("capability-signing key changed across EnsureKeys calls — must persist + reload")
	}
	if string(ca1.CertPEM()) != string(ca2.CertPEM()) {
		t.Fatal("CA cert changed across EnsureKeys calls — must persist + reload")
	}
}

func TestRevocationRoundTrip(t *testing.T) {
	s := StateDir(t.TempDir())
	if _, _, err := s.EnsureKeys(); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.LoadRevocation(); rs.MinEpoch != 0 || len(rs.Revoked) != 0 {
		t.Fatalf("absent revocation.json must be zero value, got %+v", rs)
	}
	if err := s.SaveRevocation(broker.RevocationState{MinEpoch: 3, Revoked: []string{"worker"}}); err != nil {
		t.Fatal(err)
	}
	rs, err := s.LoadRevocation()
	if err != nil {
		t.Fatal(err)
	}
	if rs.MinEpoch != 3 || len(rs.Revoked) != 1 || rs.Revoked[0] != "worker" {
		t.Fatalf("revocation did not round-trip: %+v", rs)
	}
}

func TestControllerPATRoundTrip(t *testing.T) {
	s := StateDir(t.TempDir())
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
	tok, err := s.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "pat-secret-123" {
		t.Fatalf("LoadControllerPAT() = %q, want %q", tok, "pat-secret-123")
	}
}

func TestLoadControllerPATAbsent(t *testing.T) {
	s := StateDir(t.TempDir())
	tok, err := s.LoadControllerPAT()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		t.Fatalf("LoadControllerPAT() on absent file = %q, want empty", tok)
	}
}

func TestLoadControllerPATWrongPerms(t *testing.T) {
	s := StateDir(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ControllerPAT(), []byte("pat-secret-123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadControllerPAT(); err == nil {
		t.Fatal("LoadControllerPAT() with 0644 perms: want error, got nil")
	}
}

// TestEnsureSessionSecretGeneratesOnce proves the generate-then-adopt cycle:
// first call creates the file 0600 with a 64-hex value; a second call returns
// the SAME value without rewriting the file (never-rotate — a rewrite would
// sign every browser session out). Values are proven by length/shape, never
// printed.
func TestEnsureSessionSecretGeneratesOnce(t *testing.T) {
	s := StateDir(t.TempDir())
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
	s := StateDir(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	seeded := []byte("operator-seeded-value\n")
	if err := os.WriteFile(s.SessionSecret(), seeded, 0o600); err != nil {
		t.Fatal(err)
	}
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
	s := StateDir(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.SessionSecret(), []byte("whatever"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureSessionSecret(); err == nil {
		t.Fatal("want perm error, got nil")
	}
}

func TestEnsureSessionSecretRejectsEmptyFile(t *testing.T) {
	s := StateDir(t.TempDir())
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.SessionSecret(), []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureSessionSecret(); err == nil {
		t.Fatal("empty session-secret must error, not silently regenerate")
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

func TestDirectivesRoundTripAndAbsentIsZero(t *testing.T) {
	st := StateDir(t.TempDir())
	ds, err := st.LoadDirectives()
	if err != nil || len(ds.Directives) != 0 {
		t.Fatalf("absent file: %v %v", ds, err)
	}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := broker.DirectiveState{
		Generations: map[string]int{"mgr": 2},
		Directives:  []*broker.DirectiveRecord{{ID: "d1", State: "consumed", TargetCN: "mgr", TargetGen: 1, Kind: "instruction", ExpiresAt: time.Now()}},
	}
	if err := st.SaveDirectives(want); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadDirectives()
	if err != nil || got.Generations["mgr"] != 2 || len(got.Directives) != 1 || got.Directives[0].State != "consumed" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	fi, _ := os.Stat(st.Directives())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("directives.json must be 0600, got %v", fi.Mode().Perm())
	}
}

// TestSaveDirectivesIsAtomicAndReplaces proves SaveDirectives writes via a
// temp-file-then-rename (not a plain in-place write, which would torn-write
// directives.json — the replay tombstone set — on a mid-write crash): saving
// a second, different state over a first must fully replace it on reload,
// and no .tmp-* scratch file may survive a successful save.
func TestSaveDirectivesIsAtomicAndReplaces(t *testing.T) {
	st := StateDir(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first := broker.DirectiveState{
		Generations: map[string]int{"mgr": 1},
		Directives:  []*broker.DirectiveRecord{{ID: "d1", State: "active", TargetCN: "mgr", TargetGen: 1, Kind: "instruction", ExpiresAt: time.Now()}},
	}
	if err := st.SaveDirectives(first); err != nil {
		t.Fatal(err)
	}
	second := broker.DirectiveState{
		Generations: map[string]int{"mgr": 2},
		Directives:  []*broker.DirectiveRecord{{ID: "d2", State: "consumed", TargetCN: "mgr", TargetGen: 2, Kind: "tool_call", ExpiresAt: time.Now()}},
	}
	if err := st.SaveDirectives(second); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadDirectives()
	if err != nil {
		t.Fatal(err)
	}
	if got.Generations["mgr"] != 2 || len(got.Directives) != 1 || got.Directives[0].ID != "d2" {
		t.Fatalf("second save did not fully replace the first: %+v", got)
	}
	fi, err := os.Stat(st.Directives())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("directives.json must be 0600, got %v", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(st.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if matched, _ := filepath.Match(".tmp-*", e.Name()); matched {
			t.Fatalf("leftover temp file after successful save: %s", e.Name())
		}
	}
}
