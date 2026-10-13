package state

import (
	"os"
	"slices"
	"testing"
	"time"
)

func TestHubExperimentsRecordRoundTrips(t *testing.T) {
	st := ForConfig(t.TempDir())
	if _, found, err := st.LoadHubExperimentsRecord(); err != nil || found {
		t.Fatalf("absent record: found=%v err=%v", found, err)
	}
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	in := HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts"}, Revision: 4, PinnedAt: at}
	if err := st.SaveHubExperimentsRecord(in); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.LoadHubExperimentsRecord()
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if got.NotApplicable || !slices.Equal(got.Off, in.Off) || got.Revision != 4 || !got.PinnedAt.Equal(at) {
		t.Fatalf("got %+v, want %+v", got, in)
	}
	fi, err := os.Stat(st.HubExperiments())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("record perm = %#o, want 0600", perm)
	}

	if err := st.SaveHubExperimentsRecord(HubExperimentsRecord{NotApplicable: true, PinnedAt: at}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.LoadHubExperimentsRecord(); !got.NotApplicable || len(got.Off) != 0 {
		t.Fatalf("not-applicable record = %+v", got)
	}
}

func TestHubExperimentsRecordCorruptIsAnError(t *testing.T) {
	st := ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.HubExperiments(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.LoadHubExperimentsRecord(); err == nil {
		t.Fatal("a corrupt record must not read as absent")
	}
}
