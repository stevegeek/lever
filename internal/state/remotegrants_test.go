package state

import (
	"os"
	"testing"
)

func TestRemoteGrantsRoundTripAndFailClosed(t *testing.T) {
	s := State{Dir: t.TempDir()}
	if _, err := s.LoadRemoteGrants(); err == nil {
		t.Fatal("a missing file must be an error")
	}
	if err := s.SaveRemoteGrants(RemoteGrants{Enabled: true, Logins: map[string]string{"a@b": "d1"}}); err != nil {
		t.Fatal(err)
	}
	g, err := s.LoadRemoteGrants()
	if err != nil || !g.Enabled || g.Logins["a@b"] != "d1" || g.Version != RemoteGrantsVersion {
		t.Fatalf("%+v %v", g, err)
	}
	if fi, err := os.Stat(s.RemoteGrantsFile()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi.Mode(), err)
	}
	for _, bad := range []string{"", "{", `{"version":2,"enabled":true}`} {
		if err := os.WriteFile(s.RemoteGrantsFile(), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.LoadRemoteGrants(); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}
