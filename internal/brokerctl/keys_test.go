package brokerctl

import (
	"os"
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
