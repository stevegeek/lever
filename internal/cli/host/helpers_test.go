package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/cli/clitest"
	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/testutil"
)

// managerYAML is the smallest manager block config.Load accepts; tests that
// resolve a manager (attach, msg, stop, worker) append it to an instance.
const managerYAML = "manager:\n  image: img:1\n"

// scratchWorkerYAML declares one worker, "scratch", under workers/scratch.
const scratchWorkerYAML = "workers:\n  - name: scratch\n    dir: workers/scratch\n"

// instanceYAML returns a minimal canonical lever.yaml body for app name on the
// orbstack backend (tree "workspace", subscription llm_auth) with extra
// appended raw, e.g. "remote:\n  enabled: true\n". config.Load does not need
// the tree directory to exist on disk.
func instanceYAML(name, extra string) string {
	return "name: " + name + "\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\n" + extra
}

// writeInstanceInto writes body as the canonical lever.yaml inside dir and
// returns its path.
func writeInstanceInto(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeInstanceNamed creates a fresh temp instance dir holding a canonical
// lever.yaml for app name with extra appended, and returns the dir.
func writeInstanceNamed(t *testing.T, name, extra string) string {
	t.Helper()
	dir := t.TempDir()
	writeInstanceInto(t, dir, instanceYAML(name, extra))
	return dir
}

// writeInstance is writeInstanceNamed for the default app name "demo".
func writeInstance(t *testing.T, extra string) string {
	t.Helper()
	return writeInstanceNamed(t, "demo", extra)
}

// loadInstance writes a "demo" instance with extra appended and loads it.
func loadInstance(t *testing.T, extra string) *config.App {
	t.Helper()
	app, err := config.Load(filepath.Join(writeInstance(t, extra), config.CanonicalName))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// inertApplyOpts is what tests build apply deps with. Never let a test spawn a
// real broker or proxy: os.Args[0] here is the TEST BINARY, and brokerServeCmd
// detaches the child with Setsid, so any spawn outlives the run unreaped — a
// full suite run once left 724 stray processes behind. `true` exits 0
// immediately, so cmd.Start() still succeeds and the code path under test is
// unchanged.
var inertApplyOpts = applyOpts{SelfExe: "/usr/bin/true"}

// Argv prefixes of the scion/sh calls the PAT mint window issues, named once
// so a command change in scion.Client or ensureControllerPAT is one edit.
const (
	argvScionServerStart = "scion server start"
	argvScionList        = "scion list" // waitHubReady's poll, run inside ServerStart
	argvScionInit        = "scion init"
	argvScionHubLink     = "scion hub link"
	argvScionServerStop  = "scion server stop"
	argvScionTokenCreate = "scion hub token create"
	argvShPrintf         = "sh -c printf" // $HOME resolution for the dev-token path
	argvShGuardedRm      = "sh -c if"     // the guarded removeJailFile rm
)

// scriptPATMintChain registers the throwaway-window call chain shared by every
// ensureControllerPAT test (server start → list poll → init → hub link →
// server stop → dev-token resolve/rm), everything except the "hub token
// create" calls themselves — those differ per test by --name/token, so each
// test scripts them via scriptTokenCreate or patMintRunner.
func scriptPATMintChain(f *proc.FakeRunner) {
	f.Script(argvScionServerStart, proc.Result{})
	f.Script(argvScionList, proc.Result{})
	f.Script(argvScionInit, proc.Result{})
	f.Script(argvScionHubLink, proc.Result{})
	f.Script(argvScionServerStop, proc.Result{})
	f.Script(argvShPrintf, proc.Result{Stdout: "/home/tester"})
	f.Script(argvShGuardedRm, proc.Result{})
}

// scriptTokenCreate registers a distinct "hub token create --project lever
// --name <name>" response so the fake runner can tell the controller and
// remote mints apart (they differ only by --name, which lands right after
// --project in the argv scion.Client.HubTokenCreate builds).
func scriptTokenCreate(f *proc.FakeRunner, name, token string) {
	f.Script(argvScionTokenCreate+" --project lever --name "+name, proc.Result{Stdout: "Token: " + token + "\n"})
}

// patMintRunner returns a FakeRunner scripted for one whole PAT mint window,
// answering every "hub token create" with token.
func patMintRunner(token string) *proc.FakeRunner {
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	f.Script(argvScionTokenCreate, proc.Result{Stdout: "Token: " + token + "\n"})
	return f
}

// scionOKRunner returns a FakeRunner whose blanket script answers every
// scion argv with "ok", for tests that do not intercept a verb.
func scionOKRunner() *proc.FakeRunner {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "ok"})
	return f
}

// stubRoot builds the host root command whose backend factory always
// returns sb.
func stubRoot(sb *stubBackend) *cobra.Command {
	return newRootWith(func(string, string) (backend.Backend, error) { return sb, nil })
}

// wantJailNotUp fails unless err is the passive verbs' jail-down failure and
// carries the `lever up` hint the operator is told to follow.
func wantJailNotUp(t testing.TB, err error) {
	t.Helper()
	testutil.WantErrIs(t, err, errJailNotUp)
	testutil.WantErrContaining(t, err, "lever up")
}

// wantSubcommands fails unless the host root wires a command named name
// carrying every sub.
func wantSubcommands(t *testing.T, name string, subs ...string) {
	t.Helper()
	for _, c := range newRootWith(defaultFactory).Commands() {
		if c.Name() != name {
			continue
		}
		got := clitest.Names(c)
		for _, s := range subs {
			if !got[s] {
				t.Fatalf("%s subcommands = %v, want %v", name, got, subs)
			}
		}
		return
	}
	t.Fatalf("`lever %s` not wired into the host root", name)
}
