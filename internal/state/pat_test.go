package state

import (
	"os"
	"testing"
	"time"
)

func TestPATRecordRoundTrips(t *testing.T) {
	st := ForConfig(t.TempDir())
	exp := time.Date(2027, 9, 1, 12, 0, 0, 0, time.UTC)
	in := PATRecord{
		ID:        "a8bf56c4",
		Requested: []string{"agent:manage", "agent:attach"},
		Granted:   []string{"agent:attach", "agent:create"},
		MintedAt:  exp.AddDate(-1, 0, 0),
		ExpiresAt: exp,
	}
	if err := st.SaveControllerPATRecord(in); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.LoadControllerPATRecord()
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if got.ID != in.ID || !got.ExpiresAt.Equal(in.ExpiresAt) || !got.MintedAt.Equal(in.MintedAt) {
		t.Errorf("got %+v, want %+v", got, in)
	}
	if len(got.Requested) != 2 || got.Requested[0] != "agent:manage" || len(got.Granted) != 2 {
		t.Errorf("scopes did not round-trip: %+v", got)
	}
	// Not a secret, but it names the token's id and lives beside a secret, so
	// it gets the same mode as everything else in the state dir.
	fi, err := os.Stat(st.ControllerPATRecord())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("record perm = %#o, want 0600", perm)
	}
}

// A PAT minted by a lever older than the record is a token nobody can vouch
// for: absent must be distinguishable from a record that says little.
func TestPATRecordAbsentIsNotFound(t *testing.T) {
	st := ForConfig(t.TempDir())
	rec, found, err := st.LoadControllerPATRecord()
	if err != nil || found {
		t.Fatalf("absent record: found=%v err=%v rec=%+v", found, err, rec)
	}
	if _, found, _ := st.LoadRemotePATRecord(); found {
		t.Fatal("absent remote record must not be found")
	}
}

func TestPATRecordControllerAndRemoteAreSeparateFiles(t *testing.T) {
	st := ForConfig(t.TempDir())
	if err := st.SaveRemotePATRecord(PATRecord{ID: "remote"}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := st.LoadControllerPATRecord(); found {
		t.Fatal("a remote record must not read as the controller's")
	}
	got, found, err := st.LoadRemotePATRecord()
	if err != nil || !found || got.ID != "remote" {
		t.Fatalf("remote record: found=%v err=%v rec=%+v", found, err, got)
	}
}

func TestPATRecordGarbageIsAnError(t *testing.T) {
	st := ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.ControllerPATRecord(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.LoadControllerPATRecord(); err == nil {
		t.Fatal("a corrupt record must be an error, not an absent one")
	}
}
