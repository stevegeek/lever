package guest

import (
	"context"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// writtenSettings returns the settings content the guest was sent (the stdin
// of the write script) and how many writes were attempted.
func writtenSettings(f *proc.FakeRunner) (written string, writes int) {
	for _, c := range f.Calls {
		if len(c.Args) > 0 && c.Args[len(c.Args)-1] == writeScionSettingsScript {
			writes++
			written = c.Stdin
		}
	}
	return written, writes
}

// TestDisableHubLoginRemovesTheBlockWhenTheForwarderIsAlreadyGone pins the
// composition of DisableHubLogin's two halves, which the pure-function tests
// around it cannot see.
//
// The settings edit used to be gated on the guest script reporting FOUND 1 —
// and the script `rm -f`s the binary BEFORE that edit runs. So a settings write
// that failed once (a transient guest error, the machine going away
// mid-command) made the failure PERMANENT: every later apply found no binary,
// returned early, and left the oidc_login block advertising a login for a
// provider that no longer exists, with no lever verb that removes it. The same
// state arrives via an out-of-band `rm` of the binary, or a guest rebuild that
// wipes /usr/local/bin while the home survives.
//
// Convergence must not depend on a step that already succeeded: FOUND 0 and a
// settings file that still holds the block must still end with the block gone.
func TestDisableHubLoginRemovesTheBlockWhenTheForwarderIsAlreadyGone(t *testing.T) {
	// The fixture is what EnsureHubLogin actually writes, so this proves the
	// round trip rather than a hand-made approximation of it.
	settings, _, err := hubSettingsConverged([]byte("# top\nversion: 1\nserver:\n  user_access_mode: open\n"), testHubLogin(), false)
	if err != nil {
		t.Fatalf("hubSettingsConverged: %v", err)
	}
	if !strings.Contains(string(settings), "oidc_login") {
		t.Fatalf("fixture has no oidc_login block to remove:\n%s", settings)
	}

	f := proc.NewFakeRunner()
	// The forwarder binary is already gone — the script's early exit.
	f.Script("orb -u root -m m bash -c", proc.Result{Stdout: "FOUND 0\n"})
	// ...but the hub's settings still carry the block lever wrote.
	f.Script("orb -m m /bin/bash -c", proc.Result{Stdout: "LEGACY 0\n" + string(settings)})
	// The settings write itself (content on the stdin of the guest script).
	f.Script("orb -m m bash -c", proc.Result{})
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}

	if _, err := g.DisableHubLogin(context.Background()); err != nil {
		t.Fatalf("DisableHubLogin: %v", err)
	}
	written, writes := writtenSettings(f)
	if writes == 0 {
		t.Fatal("no settings write was attempted: with the forwarder binary gone, the oidc_login block is now unreachable forever")
	}
	if written == "" {
		t.Fatal("the settings write carried no content, so the assertions below would pass vacuously")
	}
	if strings.Contains(written, "oidc_login") {
		t.Fatalf("the oidc_login block survived turning remote access off:\n%s", written)
	}
	if strings.Contains(written, operatorDisplayName) {
		t.Fatalf("lever's display_name survived beside the removed block:\n%s", written)
	}
	// Removal is surgical: the operator's own settings are not collateral.
	for _, keep := range []string{"# top", "version: 1", "user_access_mode: open"} {
		if !strings.Contains(written, keep) {
			t.Fatalf("removal took %q with it:\n%s", keep, written)
		}
	}
}
