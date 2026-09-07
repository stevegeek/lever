package jail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

// A worker's enrolment ticket must never appear on the HOST command line
// (`ps`-readable by every local user) and never in the instance tree (readable
// by the manager). It travels on the child's stdin behind a fixed staging
// script; the worker NAME is the one positional, config-validated upstream.
func TestStageWorkerTicketKeepsPayloadOffHostArgv(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if err := StageWorkerTicket(context.Background(), jr, "scratch", []byte(`{"ticket":"deadbeef"}`)); err != nil {
		t.Fatalf("StageWorkerTicket: %v", err)
	}
	if len(host.Calls) != 1 {
		t.Fatalf("want exactly one guest call, got %d", len(host.Calls))
	}
	call := host.Calls[0]
	joined := strings.Join(append([]string{call.Name}, call.Args...), " ")
	if strings.Contains(joined, "deadbeef") {
		t.Fatalf("ticket on the host argv: %q", joined)
	}
	if call.Stdin != `{"ticket":"deadbeef"}` {
		t.Fatalf("stdin = %q, want the payload", call.Stdin)
	}
	if !strings.HasSuffix(joined, "sh -c "+stageWorkerTicketScript+" _ scratch") {
		t.Fatalf("argv %q must run the fixed staging script with the worker as its positional", joined)
	}
	if !strings.Contains(stageWorkerTicketScript, "umask 077") {
		t.Fatal("the staging script must write under umask 077")
	}
}

func TestStageWorkerTicketRefusesBadInput(t *testing.T) {
	host := proc.NewFakeRunner()
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	for _, name := range []string{"", "../x", "a/b", "A", "a b", "-x", "$HOME"} {
		if err := StageWorkerTicket(context.Background(), jr, name, []byte("x")); err == nil {
			t.Fatalf("worker %q: want an error", name)
		}
	}
	if err := StageWorkerTicket(context.Background(), jr, "ok", nil); err == nil {
		t.Fatal("empty payload: want an error")
	}
	if len(host.Calls) != 0 {
		t.Fatalf("no guest call may run for refused input, got %d", len(host.Calls))
	}
}

func TestWorkerTicketPaths(t *testing.T) {
	if got := WorkerTicketDir("501", "scratch"); got != "/run/user/501/lever/tickets/scratch" {
		t.Fatalf("WorkerTicketDir = %q", got)
	}
	if got := WorkerTicketFile("501", "scratch"); got != "/run/user/501/lever/tickets/scratch/bootstrap.json" {
		t.Fatalf("WorkerTicketFile = %q", got)
	}
	// The script and the path helper must name the same guest location.
	if !strings.Contains(stageWorkerTicketScript, `$XDG_RUNTIME_DIR/lever/tickets/`) {
		t.Fatalf("staging script %q does not write under $XDG_RUNTIME_DIR/lever/tickets", stageWorkerTicketScript)
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatalf("lstat %s: %v", p, err)
	}
	return fi.Mode()
}

// The real script: private directories, a 0600 file, an overwrite on
// re-stage, and a planted symlink replaced rather than followed.
func TestStageWorkerTicketScriptWithRealShell(t *testing.T) {
	dir := t.TempDir()
	r := proc.RealRunner{}
	stage := func(payload string) error {
		res, err := r.RunStdin(context.Background(), strings.NewReader(payload),
			map[string]string{"XDG_RUNTIME_DIR": dir}, "sh", "-c", stageWorkerTicketScript, "_", "scratch")
		if err != nil {
			return errString(err, res.Stderr)
		}
		return nil
	}
	if err := stage(`{"ticket":"one"}`); err != nil {
		t.Fatalf("stage: %v", err)
	}
	file := filepath.Join(dir, "lever", "tickets", "scratch", "bootstrap.json")
	if got := mode(t, file).Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
	for _, d := range []string{filepath.Join(dir, "lever"), filepath.Join(dir, "lever", "tickets"), filepath.Dir(file)} {
		if got := mode(t, d).Perm(); got != 0o700 {
			t.Fatalf("dir %s mode = %o, want 700", d, got)
		}
	}
	if b, _ := os.ReadFile(file); string(b) != `{"ticket":"one"}` {
		t.Fatalf("content = %q", b)
	}
	// Re-stage overwrites (a resume stages a fresh ticket over a spent one).
	if err := stage(`{"ticket":"two"}`); err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != `{"ticket":"two"}` {
		t.Fatalf("content after re-stage = %q", b)
	}
	// A link planted at the file is removed, never written through.
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, file); err != nil {
		t.Fatal(err)
	}
	if err := stage(`{"ticket":"three"}`); err != nil {
		t.Fatalf("stage over link: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("link target was written through: %q", b)
	}
	if mode(t, file)&os.ModeSymlink != 0 {
		t.Fatal("staged path is still a symlink")
	}
}

func TestRemoveWorkerTicketScriptWithRealShell(t *testing.T) {
	dir := t.TempDir()
	r := proc.RealRunner{}
	env := map[string]string{"XDG_RUNTIME_DIR": dir}
	res, err := r.RunStdin(context.Background(), strings.NewReader("x"), env, "sh", "-c", stageWorkerTicketScript, "_", "scratch")
	if err != nil {
		t.Fatalf("stage: %v (%s)", err, res.Stderr)
	}
	if res, err := r.Run(context.Background(), env, "sh", "-c", removeWorkerTicketScript, "_", "scratch"); err != nil {
		t.Fatalf("remove: %v (%s)", err, res.Stderr)
	}
	if _, err := os.Lstat(filepath.Join(dir, "lever", "tickets", "scratch")); !os.IsNotExist(err) {
		t.Fatalf("ticket dir still present: %v", err)
	}
	// Idempotent: a second remove is not an error.
	if _, err := r.Run(context.Background(), env, "sh", "-c", removeWorkerTicketScript, "_", "scratch"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}

// RemoveWorkerTicket runs the fixed script through the jail runner with the
// worker as its positional, and refuses a name that is not a valid worker.
func TestRemoveWorkerTicketArgv(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if err := RemoveWorkerTicket(context.Background(), jr, "scratch"); err != nil {
		t.Fatalf("RemoveWorkerTicket: %v", err)
	}
	if len(host.Calls) != 1 {
		t.Fatalf("want one guest call, got %d", len(host.Calls))
	}
	if got := host.Calls[0].Argv(); !strings.HasSuffix(got, "sh -c "+removeWorkerTicketScript+" _ scratch") {
		t.Fatalf("argv = %q", got)
	}
	if err := RemoveWorkerTicket(context.Background(), jr, "../x"); err == nil {
		t.Fatal("bad name must be refused")
	}
	if len(host.Calls) != 1 {
		t.Fatal("a refused name must not reach the guest")
	}
}

func TestWorkerTicketScriptsRefuseWithoutRuntimeDir(t *testing.T) {
	r := proc.RealRunner{}
	if _, err := r.RunStdin(context.Background(), strings.NewReader("x"),
		map[string]string{"XDG_RUNTIME_DIR": ""}, "sh", "-c", stageWorkerTicketScript, "_", "scratch"); err == nil {
		t.Fatal("stage script must fail without XDG_RUNTIME_DIR")
	}
	if _, err := r.Run(context.Background(),
		map[string]string{"XDG_RUNTIME_DIR": ""}, "sh", "-c", removeWorkerTicketScript, "_", "scratch"); err == nil {
		t.Fatal("remove script must fail without XDG_RUNTIME_DIR")
	}
}

type shellErr struct {
	err    error
	stderr string
}

func (e shellErr) Error() string               { return e.err.Error() + ": " + e.stderr }
func errString(err error, stderr string) error { return shellErr{err, stderr} }
