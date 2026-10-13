package host

import (
	"context"
	"errors"
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

func fixedClock() (hubauth.Clock, error) { return hubauth.Clock{}, nil }

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
			_, err := loadRemoteHubLogin(app, st, func() (hubauth.Clock, error) { clockRead = true; return fixedClock() })
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
	c, err := guestClock(context.Background(), func() []string { return []string{"orb", "-m", "lever-x"} }, r, func() time.Time { return host })
	if err != nil || c.Offset != 30*time.Second {
		t.Fatalf("offset = %v, %v; want 30s", c.Offset, err)
	}
}

func TestGuestClockFailsWithoutAReading(t *testing.T) {
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
			if _, err := guestClock(context.Background(), func() []string { return tc.prefix }, r, time.Now); err == nil ||
				!strings.Contains(err.Error(), "could not read the guest clock") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// The clock is read for every sign-in, never for a login the signer refuses,
// and a failed read refuses the sign-in instead of signing on the host clock.
func TestRemoteHubLoginReadsTheClockPerSignIn(t *testing.T) {
	app := loadInstance(t, remoteBase+"  allowed_users: [op@example.com]\n")
	st := state.ForConfig(t.TempDir())
	provisionRemoteHubLogin(t, st, map[string]string{"op@example.com": "uid-op"})
	reads := 0
	var readErr error
	hl, err := loadRemoteHubLogin(app, st, func() (hubauth.Clock, error) { reads++; return hubauth.Clock{}, readErr })
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := hl.sign("op@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := hl.sign("stranger@example.com"); err == nil {
		t.Fatal("signed for a stranger")
	}
	if reads != 2 {
		t.Fatalf("clock read %d times, want once per allowed sign-in", reads)
	}
	readErr = errors.New("could not read the guest clock: boom")
	if a, err := hl.sign("op@example.com"); err == nil || a != "" {
		t.Fatalf("signed without a guest clock: %q %v", a, err)
	}
}
