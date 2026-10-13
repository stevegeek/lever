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

// TestDisableHubLoginRemovesTheBlockWhenTheForwarderIsAlreadyGone: the script
// removes the binary before the settings edit, so a failed edit, an
// out-of-band `rm` or a wiped /usr/local/bin all leave FOUND 0 with the block
// still there. Convergence must not depend on a step that already
// succeeded: the block must still end up gone.
func TestDisableHubLoginRemovesTheBlockWhenTheForwarderIsAlreadyGone(t *testing.T) {
	// The fixture is what EnsureHubLogin actually writes.
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
