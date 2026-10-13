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

// instanceYAML returns a minimal lever.yaml for app name on orbstack (tree
// "workspace", subscription llm_auth) with extra appended raw. The tree need
// not exist.
func instanceYAML(name, extra string) string {
	return "name: " + name + "\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\n" + extra
}

func writeInstanceInto(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, config.CanonicalName)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeInstanceNamed(t *testing.T, name, extra string) string {
	t.Helper()
	dir := t.TempDir()
	writeInstanceInto(t, dir, instanceYAML(name, extra))
	return dir
}

func writeInstance(t *testing.T, extra string) string {
	t.Helper()
	return writeInstanceNamed(t, "demo", extra)
}

func loadInstance(t *testing.T, extra string) *config.App {
	t.Helper()
	app, err := config.Load(filepath.Join(writeInstance(t, extra), config.CanonicalName))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// inertApplyOpts must be used by every test that builds apply deps: under
// `go test` os.Args[0] is the test binary, and a Setsid spawn outlives the run
// unreaped. `true` exits 0, so cmd.Start() still succeeds.
var inertApplyOpts = applyOpts{SelfExe: "/usr/bin/true"}

// Argv prefixes of the calls the PAT mint window issues, named once.
const (
	argvScionServerStart = "scion server start"
	argvScionList        = "scion list" // waitHubReady's poll, run inside ServerStart
	argvScionInit        = "scion init"
	argvScionHubLink     = "scion hub link"
	argvScionServerStop  = "scion server stop"
	argvScionTokenCreate = "scion hub token create"
	argvScionTokenRevoke = "scion hub token revoke"
	argvShPrintf         = "sh -c printf" // $HOME resolution for the dev-token path
	argvShGuardedRm      = "sh -c if"     // the guarded removeJailFile rm
	argvPodmanPs         = "podman ps"    // the window's running-container check
)

// scriptPATMintChain scripts the throwaway-window call chain shared by every
// ensureControllerPAT test, except "hub token create", which each test
// scripts via scriptTokenCreate or patMintRunner.
func scriptPATMintChain(f *proc.FakeRunner) {
	f.Script(argvPodmanPs, proc.Result{}) // no container running
	f.Script(argvScionServerStart, proc.Result{})
	f.Script(argvScionList, proc.Result{})
	f.Script(argvScionInit, proc.Result{})
	f.Script(argvScionHubLink, proc.Result{})
	f.Script(argvScionServerStop, proc.Result{})
	f.Script(argvShPrintf, proc.Result{Stdout: "/home/tester"})
	f.Script(argvShGuardedRm, proc.Result{})
}

// scriptTokenCreate scripts a token create for --name name, which lands right
// after --project in the argv.
func scriptTokenCreate(f *proc.FakeRunner, name, token string) {
	f.Script(argvScionTokenCreate+" --project lever --name "+name, proc.Result{Stdout: "Token: " + token + "\n"})
}

func patMintRunner(token string) *proc.FakeRunner {
	f := proc.NewFakeRunner()
	scriptPATMintChain(f)
	f.Script(argvScionTokenCreate, proc.Result{Stdout: "Token: " + token + "\n"})
	return f
}

func scionOKRunner() *proc.FakeRunner {
	f := proc.NewFakeRunner()
	f.Script("scion", proc.Result{Stdout: "ok"})
	return f
}

func stubRoot(sb *stubBackend) *cobra.Command {
	return newRootWith(func(string, string) (backend.Backend, error) { return sb, nil })
}

func wantJailNotUp(t testing.TB, err error) {
	t.Helper()
	testutil.WantErrIs(t, err, errJailNotUp)
	testutil.WantErrContaining(t, err, "lever up")
}

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
