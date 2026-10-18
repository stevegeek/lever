package brokerctl

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

func TestJailGuestBoot(t *testing.T) {
	const id = "0f4c3a9e-7b1d-4c2e-9a8f-5d6e7f8a9b0c"
	f := proc.NewFakeRunner()
	f.Script("cat "+guestBootIDPath, proc.Result{Stdout: id + "\n"})
	st := state.State{Dir: t.TempDir()}
	g := JailGuestBoot{R: f, State: st, Now: func() time.Time { return time.Unix(0, 0) }}

	got, err := g.Current(context.Background())
	if err != nil || got != id {
		t.Fatalf("Current = %q, %v", got, err)
	}
	if was, err := g.Recorded(); err != nil || was != "" {
		t.Fatalf("nothing recorded yet: %q, %v", was, err)
	}
	if err := g.Record(id); err != nil {
		t.Fatal(err)
	}
	if was, err := g.Recorded(); err != nil || was != id {
		t.Fatalf("Recorded = %q, %v", was, err)
	}
	fi, err := os.Stat(st.LifecycleFile())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("record file: %v %v", fi, err)
	}
}

func TestJailGuestBootRefusesOddValues(t *testing.T) {
	f := proc.NewFakeRunner()
	f.Script("cat "+guestBootIDPath, proc.Result{Stdout: "../../etc/passwd\n"})
	st := state.State{Dir: t.TempDir()}
	g := JailGuestBoot{R: f, State: st}
	if _, err := g.Current(context.Background()); err == nil {
		t.Fatal("a value that is not a boot id must be refused")
	}
	if err := os.WriteFile(st.LifecycleFile(), []byte(`{"version":1,"guest_boot":{"boot_id":"junk"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if was, err := g.Recorded(); err != nil || was != "" {
		t.Fatalf("a junk record reads as none: %q, %v", was, err)
	}
}
