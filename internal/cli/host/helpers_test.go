package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/config"
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
