package guest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	leverexec "github.com/stevegeek/lever/internal/exec"
)

func TestParseScionStateMarkerAndEntries(t *testing.T) {
	out := "MARKER 1\nENTRY lever__c857bb16 /lever\nENTRY scratch__b3b56fb7 /lever/workers/scratch\n"
	st := parseScionState(out)
	if !st.MarkerPresent {
		t.Fatal("MARKER 1 must parse as present")
	}
	if len(st.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(st.Entries), st.Entries)
	}
	if st.Entries[0].Name != "lever__c857bb16" || st.Entries[0].WorkspacePath != "/lever" {
		t.Fatalf("entry 0 wrong: %+v", st.Entries[0])
	}
	if st.Entries[1].WorkspacePath != "/lever/workers/scratch" {
		t.Fatalf("entry 1 workspace wrong: %+v", st.Entries[1])
	}
}

func TestParseScionStateMarkerAbsent(t *testing.T) {
	// The bad-teardown signature: registered, but the in-tree marker is gone.
	st := parseScionState("MARKER 0\nENTRY lever__c857bb16 /lever\n")
	if st.MarkerPresent {
		t.Fatal("MARKER 0 must parse as absent")
	}
	if len(st.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(st.Entries))
	}
}

func TestParseScionStateNoEntries(t *testing.T) {
	st := parseScionState("MARKER 1\n")
	if !st.MarkerPresent || len(st.Entries) != 0 {
		t.Fatalf("marker present, no entries; got present=%v entries=%d", st.MarkerPresent, len(st.Entries))
	}
}

func TestParseScionStateIgnoresJunk(t *testing.T) {
	// Malformed/short lines are skipped, not fatal (fail-safe: "nothing stale").
	st := parseScionState("\nENTRY only-two-fields\ngarbage line here\nMARKER\nENTRY a /x\n")
	if len(st.Entries) != 1 || st.Entries[0].Name != "a" {
		t.Fatalf("only the well-formed ENTRY should survive; got %+v", st.Entries)
	}
	if st.MarkerPresent {
		t.Fatal("a bare MARKER line (no value) must not read as present")
	}
}

// TestRemoveScionProjectConfigsIssuesThroughUserPrefix proves the removal
// script is emitted through the machine-only UserPrefix (mirroring
// ReadScionProjectState's transport) and that the script's rm loop targets the
// given workspace path.
func TestRemoveScionProjectConfigsIssuesThroughUserPrefix(t *testing.T) {
	for _, shape := range prefixShapes("lever-x") {
		t.Run(shape.name, func(t *testing.T) {
			f := leverexec.NewFakeRunner()
			f.Script(strings.Join(shape.userPrefix, " "), leverexec.Result{})
			g := Guest{Host: f, UserPrefix: shape.userPrefix}

			if err := g.RemoveScionProjectConfigs(context.Background(), "/lever/workers/scratch"); err != nil {
				t.Fatalf("RemoveScionProjectConfigs: %v", err)
			}
			if len(f.Calls) != 1 {
				t.Fatalf("expected 1 call, got %d: %+v", len(f.Calls), f.Calls)
			}
			call := f.Calls[0]
			wantPrefix := append(append([]string{}, shape.userPrefix[1:]...), "bash", "-lc")
			if call.Name != shape.userPrefix[0] || !equalPrefix(call.Args, wantPrefix) {
				t.Fatalf("call = %+v, want name %q then prefix %v", call, shape.userPrefix[0], wantPrefix)
			}
			script := call.Args[len(call.Args)-1]
			if !strings.Contains(script, "'/lever/workers/scratch'") {
				t.Errorf("script missing quoted target workspace path: %q", script)
			}
			if !strings.Contains(script, "project-configs") || !strings.Contains(script, "workspace_path:") {
				t.Errorf("script missing project-configs glob or workspace_path grep: %q", script)
			}
			if !strings.Contains(script, "rm -rf") {
				t.Errorf("script missing the rm -rf removal: %q", script)
			}
		})
	}
}

// TestRemoveScionProjectConfigsErrorsOnGuestFailure proves it surfaces (not
// swallows) a failure of the guest command itself, mirroring
// ReadScionProjectState's error handling.
func TestRemoveScionProjectConfigsErrorsOnGuestFailure(t *testing.T) {
	f := leverexec.NewFakeRunner() // no Script registered ⇒ unscripted-command error
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-x"}}

	if err := g.RemoveScionProjectConfigs(context.Background(), "/lever"); err == nil {
		t.Fatal("expected an error when the guest command fails, got nil")
	}
}

// writeProjectConfig creates $HOME/.scion/project-configs/<name>/.scion/settings.yaml
// with the given workspace_path (empty ⇒ no workspace_path line at all).
func writeProjectConfig(t *testing.T, home, name, workspacePath string) string {
	t.Helper()
	dir := filepath.Join(home, ".scion", "project-configs", name, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "project_id: " + name + "\n"
	if workspacePath != "" {
		body += "workspace_path: " + workspacePath + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".scion", "project-configs", name)
}

// TestScionConfigRemoveScriptDeletesOnlyMatches runs the ACTUAL bash body
// (shared with prod via scionConfigRemoveScript) against a real temp
// filesystem, because substring assertions can't prove a destructive `rm -rf`
// deletes the right dirs and only those. It verifies exact-match scoping, the
// glob-anchored dirname derivation, that no-workspace_path entries are spared,
// and idempotency.
func TestScionConfigRemoveScriptDeletesOnlyMatches(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	home := t.TempDir()

	// Two entries claim /lever (the accumulation this fix targets), one claims
	// the worker path, one has no workspace_path line at all.
	mgr1 := writeProjectConfig(t, home, "lever__aaaa1111", "/lever")
	mgr2 := writeProjectConfig(t, home, "lever__bbbb2222", "/lever")
	worker := writeProjectConfig(t, home, "worker__cccc3333", "/lever/workers/worker")
	noWP := writeProjectConfig(t, home, "legacy__dddd4444", "")

	run := func() {
		t.Helper()
		cmd := exec.Command("bash", "-lc", scionConfigRemoveScript("/lever"))
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
	}

	run()

	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if exists(mgr1) {
		t.Errorf("mgr1 (%s) should have been removed", mgr1)
	}
	if exists(mgr2) {
		t.Errorf("mgr2 (%s) should have been removed", mgr2)
	}
	if !exists(worker) {
		t.Errorf("worker (%s, workspace_path /lever/workers/worker) must survive an exact-match /lever removal", worker)
	}
	if !exists(noWP) {
		t.Errorf("no-workspace_path entry (%s) must survive", noWP)
	}

	// Idempotent: a second run removes nothing more and doesn't error.
	run()
	if exists(mgr1) || exists(mgr2) {
		t.Error("second run should keep the /lever entries removed")
	}
	if !exists(worker) || !exists(noWP) {
		t.Error("second run must not touch the surviving entries")
	}
}

// The four scenarios below pin scionProjectRegistered's exactly-one-valid-
// registration predicate: registered requires BOTH exactly one matching entry
// AND the in-tree marker. Pure-Go over backend.ScionProjectState (mirroring
// how internal/cli's checkScionProject is tested), since the underlying
// marker+entries read is already covered by TestParseScionState* above and by
// ReadScionProjectState's real production use — nothing new is parsed here.

func TestScionProjectRegisteredOneMatchingEntryPlusMarker(t *testing.T) {
	st := backend.ScionProjectState{
		MarkerPresent: true,
		Entries:       []backend.ScionProjectEntry{{Name: "lever__aaaa1111", WorkspacePath: "/lever"}},
	}
	if !scionProjectRegistered(st, "/lever") {
		t.Fatal("exactly one matching entry + marker present must be registered")
	}
}

func TestScionProjectRegisteredZeroEntries(t *testing.T) {
	st := backend.ScionProjectState{MarkerPresent: true}
	if scionProjectRegistered(st, "/lever") {
		t.Fatal("zero entries must not be registered")
	}
}

func TestScionProjectRegisteredDuplicateEntries(t *testing.T) {
	st := backend.ScionProjectState{
		MarkerPresent: true,
		Entries: []backend.ScionProjectEntry{
			{Name: "lever__aaaa1111", WorkspacePath: "/lever"},
			{Name: "lever__bbbb2222", WorkspacePath: "/lever"},
		},
	}
	if scionProjectRegistered(st, "/lever") {
		t.Fatal("two entries claiming the same workspace path must not be registered")
	}
}

func TestScionProjectRegisteredEntryWithoutMarker(t *testing.T) {
	// The bad-teardown signature: one entry claims the workspace, but the
	// in-tree marker is gone.
	st := backend.ScionProjectState{
		MarkerPresent: false,
		Entries:       []backend.ScionProjectEntry{{Name: "lever__aaaa1111", WorkspacePath: "/lever"}},
	}
	if scionProjectRegistered(st, "/lever") {
		t.Fatal("an entry without the in-tree marker must not be registered")
	}
}

// TestScionProjectRegisteredIgnoresOtherWorkspacePaths proves an entry for a
// DIFFERENT workspace (e.g. a worker's registration) doesn't count toward this
// workspace's check.
func TestScionProjectRegisteredIgnoresOtherWorkspacePaths(t *testing.T) {
	st := backend.ScionProjectState{
		MarkerPresent: true, // this workspace's own marker
		Entries:       []backend.ScionProjectEntry{{Name: "worker__cccc3333", WorkspacePath: "/lever/workers/worker"}},
	}
	if scionProjectRegistered(st, "/lever") {
		t.Fatal("an entry for a different workspace path must not count as this one's registration")
	}
}

// TestScionProjectRegisteredIssuesThroughUserPrefix proves ScionProjectRegistered
// reuses ReadScionProjectState's transport (same script, same UserPrefix, one
// call) rather than duplicating it, and resolves the predicate correctly over
// a live (fake) report.
func TestScionProjectRegisteredIssuesThroughUserPrefix(t *testing.T) {
	for _, shape := range prefixShapes("lever-x") {
		t.Run(shape.name, func(t *testing.T) {
			f := leverexec.NewFakeRunner()
			f.Script(strings.Join(shape.userPrefix, " "), leverexec.Result{Stdout: "MARKER 1\nENTRY lever__aaaa1111 /lever\n"})
			g := Guest{Host: f, UserPrefix: shape.userPrefix}

			ok, err := g.ScionProjectRegistered(context.Background(), "/lever")
			if err != nil {
				t.Fatalf("ScionProjectRegistered: %v", err)
			}
			if !ok {
				t.Fatal("expected registered=true for one matching entry + marker present")
			}
			if len(f.Calls) != 1 {
				t.Fatalf("expected 1 call (shared transport with ReadScionProjectState), got %d: %+v", len(f.Calls), f.Calls)
			}
		})
	}
}

// TestScionProjectRegisteredNotRegisteredOverTransport is the same transport
// proof for the negative case (no entries at all).
func TestScionProjectRegisteredNotRegisteredOverTransport(t *testing.T) {
	f := leverexec.NewFakeRunner()
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-x"}}
	f.Script(strings.Join(g.UserPrefix, " "), leverexec.Result{Stdout: "MARKER 1\n"})

	ok, err := g.ScionProjectRegistered(context.Background(), "/lever")
	if err != nil {
		t.Fatalf("ScionProjectRegistered: %v", err)
	}
	if ok {
		t.Fatal("expected registered=false with zero entries")
	}
}

// TestScionProjectRegisteredErrorsOnGuestFailure proves it surfaces (not
// swallows) a failure of the guest command itself, mirroring
// ReadScionProjectState's error handling — the register apply step relies on
// seeing a non-nil error to fail OPEN to the destructive path.
func TestScionProjectRegisteredErrorsOnGuestFailure(t *testing.T) {
	f := leverexec.NewFakeRunner() // no Script registered ⇒ unscripted-command error
	g := Guest{Host: f, UserPrefix: []string{"orb", "-m", "lever-x"}}

	if _, err := g.ScionProjectRegistered(context.Background(), "/lever"); err == nil {
		t.Fatal("expected an error when the guest command fails, got nil")
	}
}

// writeHubProjectConfig writes a project settings.yaml shaped like scion's own:
// a `hub:` block with an indented endpoint, plus a workspace_path.
func writeHubProjectConfig(t *testing.T, home, name, workspacePath, endpoint string) string {
	t.Helper()
	dir := filepath.Join(home, ".scion", "project-configs", name, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "schema_version: \"1\"\nhub:\n    enabled: true\n    linked: true\n"
	if endpoint != "" {
		body += "    endpoint: " + endpoint + "\n"
	}
	body += "    project_id: 9514fed3\ncli:\n    autohelp: true\n"
	if workspacePath != "" {
		body += "workspace_path: " + workspacePath + "\n"
	}
	p := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFileString(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestScionHubEndpointRepairScript runs the ACTUAL bash body against a real
// filesystem. The bug it closes: minting the controller PAT `hub link`s the
// project against a THROWAWAY hub, persisting that port here, and the
// register-project step skips its re-init when the registration is already
// sound — so `lever attach`, which execs scion bare, dialled a dead port.
func TestScionHubEndpointRepairScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	home := t.TempDir()
	const real = "http://127.0.0.1:8080"

	stale := writeHubProjectConfig(t, home, "lever__aaaa1111", "/lever", "http://127.0.0.1:48080")
	already := writeHubProjectConfig(t, home, "lever__bbbb2222", "/lever", real)
	otherWP := writeHubProjectConfig(t, home, "worker__cccc3333", "/lever/workers/worker", "http://127.0.0.1:48080")
	noEndpoint := writeHubProjectConfig(t, home, "legacy__dddd4444", "/lever", "")

	run := func() string {
		t.Helper()
		cmd := exec.Command("bash", "-lc", scionHubEndpointRepairScript("/lever", real))
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		return string(out)
	}

	out := run()

	if got := readFileString(t, stale); !strings.Contains(got, "    endpoint: "+real) {
		t.Errorf("stale endpoint not repaired:\n%s", got)
	}
	if got := readFileString(t, stale); strings.Contains(got, "48080") {
		t.Errorf("throwaway endpoint still present:\n%s", got)
	}
	// Indentation must survive: scion re-reads this as YAML.
	if got := readFileString(t, stale); !strings.Contains(got, "hub:\n    enabled: true") {
		t.Errorf("hub block shape broken:\n%s", got)
	}
	if !strings.Contains(out, "REPAIRED") {
		t.Errorf("a repair should be reported, got %q", out)
	}
	// A different workspace_path is not ours to touch.
	if got := readFileString(t, otherWP); !strings.Contains(got, "48080") {
		t.Errorf("an entry for another workspace must be left alone:\n%s", got)
	}
	// Already-correct and endpoint-less entries are untouched.
	if got := readFileString(t, already); strings.Count(got, "endpoint:") != 1 {
		t.Errorf("already-correct entry was rewritten:\n%s", got)
	}
	if got := readFileString(t, noEndpoint); strings.Contains(got, "endpoint:") {
		t.Errorf("an entry with no endpoint must not gain one:\n%s", got)
	}

	// Idempotent: a second run changes nothing and reports nothing.
	before := readFileString(t, stale)
	if out2 := run(); strings.Contains(out2, "REPAIRED") {
		t.Errorf("second run should be a no-op, got %q", out2)
	}
	if readFileString(t, stale) != before {
		t.Error("second run modified the file")
	}
}
