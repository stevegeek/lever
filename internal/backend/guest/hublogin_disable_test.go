package guest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/exec"
)

// settingsCapturingRunner is a FakeRunner that also reads the file
// writeScionSettings stages, so a test can assert on the CONTENT the guest
// would have received. The staged file is a host temp file the write script
// cats into the guest, and writeScionSettings removes it as soon as the Run
// returns — so it has to be read during the call, not after it.
type settingsCapturingRunner struct {
	*exec.FakeRunner
	written string // content of the settings file the guest was sent
	writes  int    // how many settings writes were attempted
}

func (r *settingsCapturingRunner) RunIn(ctx context.Context, dir string, env map[string]string, name string, args ...string) (exec.Result, error) {
	if p := stagedSettingsPath(name, args); p != "" {
		r.writes++
		if b, err := os.ReadFile(p); err == nil {
			r.written = string(b)
		}
	}
	return r.FakeRunner.RunIn(ctx, dir, env, name, args...)
}

// Run must be re-declared, not inherited: the embedded FakeRunner's Run calls
// ITS OWN RunIn, so a guest calling Run would bypass the capture above.
func (r *settingsCapturingRunner) Run(ctx context.Context, env map[string]string, name string, args ...string) (exec.Result, error) {
	return r.RunIn(ctx, "", env, name, args...)
}

// stagedSettingsPath returns the host-side temp file a settings-write call is
// streaming into the guest, or "" for any other call. It reads the path back
// out of writeScionSettingsScript's `cat '<path>' | ... ` head.
func stagedSettingsPath(name string, args []string) string {
	if name != "bash" || len(args) != 2 || args[0] != "-c" {
		return ""
	}
	_, rest, ok := strings.Cut(args[1], "cat '")
	if !ok || !strings.Contains(args[1], "lever-scion-settings-") {
		return ""
	}
	path, _, ok := strings.Cut(rest, "'")
	if !ok {
		return ""
	}
	return path
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
	settings, _, err := hubLoginSettings([]byte("# top\nversion: 1\nserver:\n  user_access_mode: open\n"), testHubLogin(), false)
	if err != nil {
		t.Fatalf("hubLoginSettings: %v", err)
	}
	if !strings.Contains(string(settings), "oidc_login") {
		t.Fatalf("fixture has no oidc_login block to remove:\n%s", settings)
	}

	f := exec.NewFakeRunner()
	// The forwarder binary is already gone — the script's early exit.
	f.Script("orb -u root -m m bash -c", exec.Result{Stdout: "FOUND 0\n"})
	// ...but the hub's settings still carry the block lever wrote.
	f.Script("orb -m m /bin/bash -c", exec.Result{Stdout: "LEGACY 0\n" + string(settings)})
	// The settings write itself (host `cat` piped into the guest).
	f.Script("bash -c", exec.Result{})
	r := &settingsCapturingRunner{FakeRunner: f}
	g := Guest{Host: r, UserPrefix: []string{"orb", "-m", "m"}, RootPrefix: []string{"orb", "-u", "root", "-m", "m"}}

	if _, err := g.DisableHubLogin(context.Background()); err != nil {
		t.Fatalf("DisableHubLogin: %v", err)
	}
	if r.writes == 0 {
		t.Fatal("no settings write was attempted: with the forwarder binary gone, the oidc_login block is now unreachable forever")
	}
	if r.written == "" {
		t.Fatal("the staged settings file could not be read, so the assertions below would pass vacuously")
	}
	if strings.Contains(r.written, "oidc_login") {
		t.Fatalf("the oidc_login block survived turning remote access off:\n%s", r.written)
	}
	if strings.Contains(r.written, operatorDisplayName) {
		t.Fatalf("lever's display_name survived beside the removed block:\n%s", r.written)
	}
	// Removal is surgical: the operator's own settings are not collateral.
	for _, keep := range []string{"# top", "version: 1", "user_access_mode: open"} {
		if !strings.Contains(r.written, keep) {
			t.Fatalf("removal took %q with it:\n%s", keep, r.written)
		}
	}
}
