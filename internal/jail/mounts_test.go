package jail

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

func TestContainerMountTargets(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{Stdout: `[{"Type":"bind","Source":"/lever/workers/a","Destination":"/workspace","RW":true},` +
		`{"Type":"bind","Source":"/run/user/501/lever/tickets/a","Destination":"/run/lever","RW":false}]` + "\n"})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	got, err := ContainerMountTargets(context.Background(), jr, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/workspace", "/run/lever"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	argv := host.Calls[0].Argv()
	for _, want := range []string{"XDG_RUNTIME_DIR=/run/user/501", "podman inspect --type container --format {{json .Mounts}} abc123"} {
		if !contains(argv, want) {
			t.Fatalf("argv %q lacks %q", argv, want)
		}
	}
	for _, bad := range []string{"", " ", "--all"} {
		if _, err := ContainerMountTargets(context.Background(), jr, bad); err == nil {
			t.Fatalf("id %q must be refused", bad)
		}
	}
	if len(host.Calls) != 1 {
		t.Fatal("a refused id must not reach the guest")
	}
}

// failingRunner answers every call with a fixed stderr and error, the way a
// podman exit 125 reaches the jail runner.
type failingRunner struct{ stderr string }

func (f failingRunner) Run(_ context.Context, _ map[string]string, _ string, _ ...string) (proc.Result, error) {
	return proc.Result{Stderr: f.stderr, Code: 125}, errors.New("exit status 125")
}
func (f failingRunner) RunIn(ctx context.Context, _ string, env map[string]string, name string, args ...string) (proc.Result, error) {
	return f.Run(ctx, env, name, args...)
}
func (f failingRunner) RunStdin(ctx context.Context, _ io.Reader, env map[string]string, name string, args ...string) (proc.Result, error) {
	return f.Run(ctx, env, name, args...)
}

func TestContainerMountTargetsNoSuchContainer(t *testing.T) {
	jr := New(Config{Host: failingRunner{`Error: no such container "lever--worker"`}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if _, err := ContainerMountTargets(context.Background(), jr, "lever--worker"); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("err = %v, want ErrNoContainer", err)
	}
	if got := ContainerName("lever", "worker"); got != "lever--worker" {
		t.Fatalf("ContainerName = %q", got)
	}
	// Any other podman failure is a plain error, not "no container".
	jr = New(Config{Host: failingRunner{"Error: cannot connect to podman"}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if _, err := ContainerMountTargets(context.Background(), jr, "lever--worker"); err == nil || errors.Is(err, ErrNoContainer) {
		t.Fatalf("err = %v, want a plain inspect error", err)
	}
}

func TestContainerMountTargetsBadJSON(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{Stdout: "not json"})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if _, err := ContainerMountTargets(context.Background(), jr, "abc"); err == nil {
		t.Fatal("want a parse error")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestContainerEnvValue(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{Stdout: "SCION_TELEMETRY_ENABLED=false\n"})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	v, set, err := ContainerEnvValue(context.Background(), jr, "lever--mgr", "SCION_TELEMETRY_ENABLED")
	if err != nil || !set || v != "false" {
		t.Fatalf("ContainerEnvValue = %q, %v, %v", v, set, err)
	}
	// The filtering runs in the guest: the argv carries the script and the
	// key, and nothing asks podman for the whole env on the host side.
	argv := host.Calls[0].Argv()
	for _, want := range []string{"XDG_RUNTIME_DIR=/run/user/501", "sh -c", "lever--mgr SCION_TELEMETRY_ENABLED", `grep -E "^$2="`} {
		if !contains(argv, want) {
			t.Fatalf("argv %q lacks %q", argv, want)
		}
	}

	host = proc.NewFakeRunner()
	host.Script("orb", proc.Result{Stdout: ""})
	jr = New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if _, set, err := ContainerEnvValue(context.Background(), jr, "abc", "SCION_TELEMETRY_ENABLED"); err != nil || set {
		t.Fatalf("an absent variable: set=%v err=%v", set, err)
	}
	for _, bad := range [][2]string{{"", "K"}, {"--all", "K"}, {"abc", "K; rm -rf /"}, {"abc", ""}} {
		if _, _, err := ContainerEnvValue(context.Background(), jr, bad[0], bad[1]); err == nil {
			t.Fatalf("ref %q key %q must be refused", bad[0], bad[1])
		}
	}
	if len(host.Calls) != 1 {
		t.Fatal("a refused argument must not reach the guest")
	}

	jr = New(Config{Host: failingRunner{`Error: no such container "x"`}, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if _, _, err := ContainerEnvValue(context.Background(), jr, "x", "K"); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("err = %v, want ErrNoContainer", err)
	}
}

// TestContainerEnvScriptFiltersInTheGuest runs the guest script against a
// stub podman, so the grep, the exit status and the "one line only" promise
// are proven rather than asserted from the argv.
func TestContainerEnvScriptFiltersInTheGuest(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\n" +
		`case "$*" in *missing*) echo 'Error: no such container' >&2; exit 125;; esac` + "\n" +
		"printf 'ANTHROPIC_API_KEY=sk-secret\\nSCION_TELEMETRY_ENABLED=false\\nSCION_TELEMETRY_ENABLED_X=1\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(ref string) (string, error) {
		cmd := exec.Command("sh", "-c", containerEnvScript, "sh", ref, "SCION_TELEMETRY_ENABLED")
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		out, err := cmd.Output()
		return string(out), err
	}
	out, err := run("mgr")
	if err != nil || out != "SCION_TELEMETRY_ENABLED=false\n" {
		t.Fatalf("script output = %q, %v", out, err)
	}
	if _, err := run("missing"); err == nil {
		t.Fatal("podman's failure was swallowed")
	}
}
