package wire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestStageRoundTrips pins that Stage deposits bootstrap.json at 0600 with every
// envelope field intact and decodable against the same tags — the single staging
// path shared by the host (manager enrolment) and the broker (worker dispatch).
func TestStageRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".lever")
	want := Bootstrap{
		Ticket:    "tkt-stage",
		BrokerCA:  "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----",
		BrokerURL: "https://broker.local:8461",
		AgentCN:   "worker.example",
	}
	if err := Stage(dir, want); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	p := filepath.Join(dir, "bootstrap.json")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perms = %v, want 0600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var got Bootstrap
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip: got %+v, want %+v", got, want)
	}
}

// TestStageReChmodsOnOverwrite pins the load-bearing explicit Chmod: os.WriteFile
// applies its perm only when it CREATES the file, so re-staging over a
// wider-mode bootstrap.json must still land at 0600. (The broker re-stages a
// fresh ticket on every resume/re-enrol over the existing file.)
func TestStageReChmodsOnOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".lever")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "bootstrap.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Stage(dir, Bootstrap{Ticket: "t"}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perms after overwrite = %v, want 0600", fi.Mode().Perm())
	}
}
