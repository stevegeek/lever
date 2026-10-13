package host

import (
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/state"
)

func TestRemoteBoundUser(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	f := remoteBoundUser(st)

	// No record yet: nothing is bound.
	if id, ok := f("op"); ok || id != "" {
		t.Fatalf("no record: got %q,%v want \"\",false", id, ok)
	}

	email := config.HubEmailFor("op")
	if err := st.SaveRemoteRoleRecord(state.RemoteRoleRecord{
		Bound: map[string]string{email: "user-7"},
	}); err != nil {
		t.Fatal(err)
	}
	if id, ok := f("op"); !ok || id != "user-7" {
		t.Fatalf("bound login: got %q,%v want user-7,true", id, ok)
	}
	// A login present in config but not yet bound (first sign-in creates the row).
	if id, ok := f("pending"); ok || id != "" {
		t.Fatalf("unbound login: got %q,%v want \"\",false", id, ok)
	}
}
