package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/lever/internal/backend"
	"github.com/stevegeek/lever/internal/config"
	leverexec "github.com/stevegeek/lever/internal/exec"
)

// workerInstanceDir writes a canonical lever.yaml declaring one worker ("scratch")
// with a real tree/workers/scratch subdir, and returns the instance dir.
func workerInstanceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workspace", "workers", "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name: demo\nbackend: orbstack\ntree: workspace\nbroker:\n  llm_auth: subscription\nmanager:\n  image: img:1\nworkers:\n  - name: scratch\n    dir: workers/scratch\n"
	if err := os.WriteFile(filepath.Join(dir, config.CanonicalName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestWorkerPurgeDeletesRecordNotWorkspace is the RISK-GATE test: purge deletes
// the scion record and the staged bootstrap, but NEVER the HostWorkspace (the
// worker's work product).
func TestWorkerPurgeDeletesRecordNotWorkspace(t *testing.T) {
	dir := workerInstanceDir(t)
	t.Chdir(dir)

	hostWorkspace := filepath.Join(dir, "workspace", "workers", "scratch")
	bootstrapDir := filepath.Join(hostWorkspace, ".lever")
	if err := os.MkdirAll(bootstrapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(bootstrapDir, "bootstrap.json")
	if err := os.WriteFile(bootstrap, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A work-product file that MUST survive the purge.
	product := filepath.Join(hostWorkspace, "result.txt")
	if err := os.WriteFile(product, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	f := leverexec.NewFakeRunner()
	f.Script("scion", leverexec.Result{Stdout: "ok"})
	sb := &stubBackend{runner: f}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"worker", "purge", "scratch", "--force"})

	if err := root.Execute(); err != nil {
		t.Fatalf("purge: %v (%s)", err, out.String())
	}

	// scion delete scratch -g /lever ... was called.
	if len(f.Calls) != 1 {
		t.Fatalf("expected exactly one scion call (delete), got %+v", f.Calls)
	}
	call := f.Calls[0]
	if call.Name != "scion" || len(call.Args) < 2 || call.Args[0] != "delete" || call.Args[1] != "scratch" {
		t.Fatalf("expected `scion delete scratch ...`, got %+v", call)
	}
	// project is the mount root, passed via -g.
	if !containsArg(call.Args, "/lever") {
		t.Fatalf("delete must target the instance project /lever, got args %v", call.Args)
	}

	// The staged bootstrap is gone.
	if _, err := os.Stat(bootstrap); !os.IsNotExist(err) {
		t.Fatalf("bootstrap.json must be removed, stat err = %v", err)
	}
	// The HostWorkspace and its work product survive.
	if _, err := os.Stat(hostWorkspace); err != nil {
		t.Fatalf("HostWorkspace must survive purge, stat err = %v", err)
	}
	if b, err := os.ReadFile(product); err != nil || string(b) != "keep me" {
		t.Fatalf("work product must survive purge: err=%v content=%q", err, string(b))
	}
}

// TestWorkerPurgeRequiresForce proves the destructive guard: without --force,
// purge refuses and does NOT call scion or remove anything.
func TestWorkerPurgeRequiresForce(t *testing.T) {
	dir := workerInstanceDir(t)
	t.Chdir(dir)

	bootstrapDir := filepath.Join(dir, "workspace", "workers", "scratch", ".lever")
	if err := os.MkdirAll(bootstrapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(bootstrapDir, "bootstrap.json")
	if err := os.WriteFile(bootstrap, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := leverexec.NewFakeRunner() // no scripts: any scion call errors loudly
	sb := &stubBackend{runner: f}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"worker", "purge", "scratch"})

	if err := root.Execute(); err == nil {
		t.Fatal("purge without --force must error")
	}
	if len(f.Calls) != 0 {
		t.Fatalf("purge without --force must not call scion, got %+v", f.Calls)
	}
	if _, err := os.Stat(bootstrap); err != nil {
		t.Fatalf("bootstrap.json must survive a refused purge, stat err = %v", err)
	}
}

// TestWorkerPurgeUnknownWorker rejects a name not declared in config.
func TestWorkerPurgeUnknownWorker(t *testing.T) {
	dir := workerInstanceDir(t)
	t.Chdir(dir)

	f := leverexec.NewFakeRunner()
	sb := &stubBackend{runner: f}
	root := NewRootWithBackend(func(string, string) (backend.Backend, error) { return sb, nil })
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"worker", "purge", "ghost", "--force"})

	if err := root.Execute(); err == nil {
		t.Fatal("purge of an undeclared worker must error")
	}
	if len(f.Calls) != 0 {
		t.Fatalf("unknown worker must not call scion, got %+v", f.Calls)
	}
}

// containsArg reports whether want appears anywhere in args.
func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
