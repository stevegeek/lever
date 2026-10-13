package host

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// provisionRemoteHubLogin writes what `lever apply` leaves for the proxy's
// hub login: the assertion key, the instance id and the hub identity record.
func provisionRemoteHubLogin(t *testing.T, st state.State, users map[string]string) {
	t.Helper()
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hubauth.EnsureKey(st.HubAuthKey()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureInstanceID(); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveHubIdentity(state.HubIdentity{Version: state.HubIdentityVersion, Mode: state.HubModeFresh, Users: users}); err != nil {
		t.Fatal(err)
	}
}

func fixedClock() hubauth.Clock { return hubauth.Clock{} }

func TestRemoteHubLoginRefusesToStartWithoutItsFiles(t *testing.T) {
	app := loadInstance(t, remoteBase+"  allowed_users: [op@example.com]\n")
	for _, tc := range []struct {
		name   string
		remove func(st state.State) string
	}{
		{"no key", func(st state.State) string { return st.HubAuthKey() }},
		{"no instance id", func(st state.State) string { return st.InstanceIDFile() }},
		{"no hub identity", func(st state.State) string { return st.HubIdentityFile() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := state.ForConfig(t.TempDir())
			provisionRemoteHubLogin(t, st, map[string]string{"op@example.com": "uid-op"})
			if err := os.Remove(tc.remove(st)); err != nil {
				t.Fatal(err)
			}
			clockRead := false
			_, err := loadRemoteHubLogin(app, st, func() hubauth.Clock { clockRead = true; return fixedClock() })
			if err == nil || !strings.Contains(err.Error(), "run `lever apply`") {
				t.Fatalf("error = %v, want a refusal that says to run lever apply", err)
			}
			if clockRead {
				t.Fatal("the guest clock was read before the files were checked")
			}
		})
	}
}

func TestRemoteHubLoginSignsOnlyAllowedLoginsAndLooksUpTheirIDs(t *testing.T) {
	app := loadInstance(t, remoteBase+"  allowed_users: [Op@Example.com, other@example.com]\n")
	st := state.ForConfig(t.TempDir())
	provisionRemoteHubLogin(t, st, map[string]string{"op@example.com": "uid-op"})
	hl, err := loadRemoteHubLogin(app, st, fixedClock)
	if err != nil {
		t.Fatalf("loadRemoteHubLogin: %v", err)
	}
	if a, err := hl.sign("op@example.com"); err != nil || a == "" {
		t.Fatalf("sign(allowed) = %q, %v", a, err)
	}
	for _, email := range []string{"stranger@example.com", hubauth.AdminEmail, hubauth.ControllerEmail} {
		if _, err := hl.sign(email); err == nil {
			t.Fatalf("signed for %s, which is not an allowed remote login", email)
		}
	}
	if id, ok := hl.hubUserID("OP@example.com"); !ok || id != "uid-op" {
		t.Fatalf("hubUserID(op) = %q, %v", id, ok)
	}
	if _, ok := hl.hubUserID("other@example.com"); ok {
		t.Fatal("a login with no recorded hub user has an id")
	}
}

func TestRemoteHubLoginUnnamedOperator(t *testing.T) {
	app := loadInstance(t, remoteBase)
	st := state.ForConfig(t.TempDir())
	provisionRemoteHubLogin(t, st, nil)
	hl, err := loadRemoteHubLogin(app, st, fixedClock)
	if err != nil {
		t.Fatalf("loadRemoteHubLogin: %v", err)
	}
	if _, err := hl.sign("lever-operator@lever.local"); err != nil {
		t.Fatalf("the placeholder operator cannot be signed for: %v", err)
	}
}

func TestGuestClockReadsTheGuestThroughTheJailPrefix(t *testing.T) {
	host := time.Unix(1_000_000, 0)
	r := proc.NewFakeRunner()
	r.Script("orb -m lever-x date -u +%s", proc.Result{Stdout: "1000030\n"})
	var warn bytes.Buffer
	c := guestClock(context.Background(), func() []string { return []string{"orb", "-m", "lever-x"} }, r, func() time.Time { return host }, &warn)
	if c.Offset != 30*time.Second {
		t.Fatalf("offset = %v, want 30s", c.Offset)
	}
	if warn.Len() != 0 {
		t.Fatalf("warned on a good read: %s", warn.String())
	}
}

func TestGuestClockFallsBackToTheHostClock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix []string
		res    proc.Result
		fail   bool
	}{
		{"no jail", nil, proc.Result{}, false},
		{"garbage", []string{"orb"}, proc.Result{Stdout: "Tue Oct 13\n"}, false},
		{"command fails", []string{"orb"}, proc.Result{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := proc.NewFakeRunner()
			if !tc.fail {
				r.Script("orb date", tc.res)
			}
			var warn bytes.Buffer
			c := guestClock(context.Background(), func() []string { return tc.prefix }, r, time.Now, &warn)
			if c.Offset != 0 {
				t.Fatalf("offset = %v, want 0", c.Offset)
			}
			if !strings.Contains(warn.String(), "could not read the guest clock") {
				t.Fatalf("no warning: %q", warn.String())
			}
		})
	}
}
