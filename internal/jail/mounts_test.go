package jail

import (
	"context"
	"errors"
	"io"
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
