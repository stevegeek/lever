package host

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

func workerInstanceDir(t *testing.T) string {
	t.Helper()
	dir := writeInstance(t, managerYAML+scratchWorkerYAML)
	if err := os.MkdirAll(filepath.Join(dir, "workspace", "workers", "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestWorkerPurgeDeletesRecordNotWorkspace: purge deletes the scion record and
// the staged ticket, never the HostWorkspace (the work product), and writes
// nothing under it.
func TestWorkerPurgeDeletesRecordNotWorkspace(t *testing.T) {
	dir := workerInstanceDir(t)
	t.Chdir(dir)

	hostWorkspace := filepath.Join(dir, "workspace", "workers", "scratch")
	product := filepath.Join(hostWorkspace, "result.txt")
	if err := os.WriteFile(product, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	// purge's scion client must keep its HubTokenSource, or it deletes
	// anonymously, and the hub refuses it.
	if err := state.ForConfig(dir).SaveControllerPAT("pat-worker-purge"); err != nil {
		t.Fatal(err)
	}

	f := scionOKRunner()
	f.Script("sh", proc.Result{})
	sb := &stubBackend{runner: f}
	root := stubRoot(sb)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"worker", "purge", "scratch", "--force"})

	if err := root.Execute(); err != nil {
		t.Fatalf("purge: %v (%s)", err, out.String())
	}
	// The ticket dir and XDG_RUNTIME_DIR come from the run user's uid, so purge
	// reads it before building the jail runner.
	if !sb.resolvedRunUser || sb.runnerBeforeUser {
		t.Fatalf("purge: resolved=%v, runner before the user=%v", sb.resolvedRunUser, sb.runnerBeforeUser)
	}

	// scion delete, then the guest ticket directory, and nothing else.
	if len(f.Calls) != 2 {
		t.Fatalf("expected two jail calls (delete, ticket removal), got %+v", f.Calls)
	}
	call := f.Calls[0]
	if call.Name != "scion" || len(call.Args) < 2 || call.Args[0] != "delete" || call.Args[1] != "scratch" {
		t.Fatalf("expected `scion delete scratch ...`, got %+v", call)
	}
	if got := call.Env["SCION_HUB_TOKEN"]; got != "pat-worker-purge" {
		t.Fatalf("delete env SCION_HUB_TOKEN = %q, want %q (HubTokenSource dropped)", got, "pat-worker-purge")
	}
	if !containsArg(call.Args, "/lever") {
		t.Fatalf("delete must target the instance project /lever, got args %v", call.Args)
	}
	if rm := f.Calls[1]; rm.Name != "sh" || !strings.Contains(rm.Argv(), "lever/tickets/$1") || rm.Args[len(rm.Args)-1] != "scratch" {
		t.Fatalf("expected the guest ticket removal for scratch, got %+v", rm)
	}

	if _, err := os.Stat(hostWorkspace); err != nil {
		t.Fatalf("HostWorkspace must survive purge, stat err = %v", err)
	}
	if b, err := os.ReadFile(product); err != nil || string(b) != "keep me" {
		t.Fatalf("work product must survive purge: err=%v content=%q", err, string(b))
	}
	if entries, _ := os.ReadDir(hostWorkspace); len(entries) != 1 {
		t.Fatalf("purge must write nothing under the workspace, got %v", entries)
	}
}

// TestWorkerPurgeRequiresForce: without --force purge calls nothing and
// removes nothing.
func TestWorkerPurgeRequiresForce(t *testing.T) {
	dir := workerInstanceDir(t)
	t.Chdir(dir)

	f := proc.NewFakeRunner() // no scripts: any jail call errors loudly
	root := stubRoot(&stubBackend{runner: f})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"worker", "purge", "scratch"})

	if err := root.Execute(); err == nil {
		t.Fatal("purge without --force must error")
	}
	if len(f.Calls) != 0 {
		t.Fatalf("purge without --force must not touch the jail, got %+v", f.Calls)
	}
}

func TestWorkerPurgeUnknownWorker(t *testing.T) {
	dir := workerInstanceDir(t)
	t.Chdir(dir)

	f := proc.NewFakeRunner()
	root := stubRoot(&stubBackend{runner: f})
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

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
