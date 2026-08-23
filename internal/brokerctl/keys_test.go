package brokerctl

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/broker"
	"github.com/stevegeek/lever/internal/state"
)

func TestEnsureKeysGeneratesThenReloads(t *testing.T) {
	s := state.ForConfig(t.TempDir())
	kp1, ca1, err := EnsureKeys(s)
	if err != nil {
		t.Fatal(err)
	}
	kp2, ca2, err := EnsureKeys(s) // second call must reload, not regenerate
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
	s := state.ForConfig(t.TempDir())
	if _, _, err := EnsureKeys(s); err != nil {
		t.Fatal(err)
	}
	if rs, _ := LoadRevocation(s); rs.MinEpoch != 0 || len(rs.Revoked) != 0 {
		t.Fatalf("absent revocation.json must be zero value, got %+v", rs)
	}
	if err := SaveRevocation(s, broker.RevocationState{MinEpoch: 3, Revoked: []string{"worker"}}); err != nil {
		t.Fatal(err)
	}
	rs, err := LoadRevocation(s)
	if err != nil {
		t.Fatal(err)
	}
	if rs.MinEpoch != 3 || len(rs.Revoked) != 1 || rs.Revoked[0] != "worker" {
		t.Fatalf("revocation did not round-trip: %+v", rs)
	}
}

func TestDirectivesRoundTripAndAbsentIsZero(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	ds, err := LoadDirectives(st)
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
	if err := SaveDirectives(st, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDirectives(st)
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
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	first := broker.DirectiveState{
		Generations: map[string]int{"mgr": 1},
		Directives:  []*broker.DirectiveRecord{{ID: "d1", State: "active", TargetCN: "mgr", TargetGen: 1, Kind: "instruction", ExpiresAt: time.Now()}},
	}
	if err := SaveDirectives(st, first); err != nil {
		t.Fatal(err)
	}
	second := broker.DirectiveState{
		Generations: map[string]int{"mgr": 2},
		Directives:  []*broker.DirectiveRecord{{ID: "d2", State: "consumed", TargetCN: "mgr", TargetGen: 2, Kind: "tool_call", ExpiresAt: time.Now()}},
	}
	if err := SaveDirectives(st, second); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDirectives(st)
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
