package jail

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/proc"
)

const procStat = "cpu  1 2 3 4\nintr 5\nctxt 6\nbtime 1760000000\nprocesses 7\n"

func TestContainerStartedBeforeBoot(t *testing.T) {
	const inspect = "podman inspect --type container --format {{.State.StartedAt.Unix}} lever--hello"
	cases := []struct {
		name    string
		started string
		stat    string
		want    bool
		wantErr bool
	}{
		{"started in an earlier boot", "1759990000\n", procStat, true, false},
		{"started in this boot", "1760000100\n", procStat, false, false},
		{"never started (zero time)", "-62135596800\n", procStat, false, false},
		{"odd answer", "yesterday\n", procStat, false, true},
		{"no btime", "1759990000\n", "cpu 1 2\n", false, true},
		{"junk btime", "1759990000\n", "btime soon\n", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script(inspect, proc.Result{Stdout: c.started})
			f.Script("cat /proc/stat", proc.Result{Stdout: c.stat})
			got, err := ContainerStartedBeforeBoot(context.Background(), f, "lever--hello")
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got %t, %v; want %t, err %t", got, err, c.want, c.wantErr)
			}
		})
	}
}

func TestContainerStartedBeforeBootRefuses(t *testing.T) {
	f := proc.NewFakeRunner()
	for _, ref := range []string{"", "  ", "--all"} {
		if _, err := ContainerStartedBeforeBoot(context.Background(), f, ref); err == nil {
			t.Errorf("ref %q must be refused", ref)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("a refused ref must run nothing: %+v", f.Calls)
	}
	// podman failing (no such container, no podman) is an error, never a yes.
	got, err := ContainerStartedBeforeBoot(context.Background(), f, "lever--hello")
	if err == nil || got || !errors.Is(err, proc.ErrUnscripted) {
		t.Errorf("got %t, %v", got, err)
	}
}

func TestContainerCreatedBeforeBoot(t *testing.T) {
	const inspect = "podman inspect --type container --format {{.Created.Unix}} lever--hello"
	for _, c := range []struct {
		name    string
		created string
		want    bool
		wantErr bool
	}{
		{"created in an earlier boot", "1759990000\n", true, false},
		{"created in this boot", "1760000100\n", false, false},
		{"zero time", "-62135596800\n", false, false},
		{"odd answer", "soon\n", false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script(inspect, proc.Result{Stdout: c.created})
			f.Script("cat /proc/stat", proc.Result{Stdout: procStat})
			got, err := ContainerCreatedBeforeBoot(context.Background(), f, "lever--hello")
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got %t, %v; want %t, err %t", got, err, c.want, c.wantErr)
			}
		})
	}
	f := proc.NewFakeRunner()
	if _, err := ContainerCreatedBeforeBoot(context.Background(), f, "--all"); err == nil || len(f.Calls) != 0 {
		t.Fatalf("a flag-like ref must be refused before podman runs: %v %+v", err, f.Calls)
	}
}

func TestContainerStrandedCreated(t *testing.T) {
	const created = "podman inspect --type container --format {{.Created.Unix}} lever--hello"
	for _, c := range []struct {
		name  string
		state string
		made  string
		want  bool
	}{
		{"created in an ended boot", "created", "1759990000\n", true},
		{"created in this boot", "created", "1760000100\n", false},
		{"exited", "exited", "1759990000\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script(stateFormat+"lever--hello", proc.Result{Stdout: testID + " lever--hello " + c.state + " false false\n"})
			f.Script(created, proc.Result{Stdout: c.made})
			f.Script("cat /proc/stat", proc.Result{Stdout: procStat})
			got, err := ContainerStrandedCreated(context.Background(), f, "lever--hello")
			if err != nil || got != c.want {
				t.Fatalf("got %t, %v; want %t", got, err, c.want)
			}
		})
	}
}

// A failed podman inspect or an unreadable boot time is an error, never a yes.
func TestContainerCreatedBeforeBootErrors(t *testing.T) {
	f := proc.NewFakeRunner()
	if got, err := ContainerCreatedBeforeBoot(context.Background(), f, "lever--hello"); err == nil || got {
		t.Fatalf("podman failing: got %t, %v", got, err)
	}
	f = proc.NewFakeRunner()
	f.Script("podman inspect --type container --format {{.Created.Unix}} lever--hello", proc.Result{Stdout: "1759990000\n"})
	if got, err := ContainerCreatedBeforeBoot(context.Background(), f, "lever--hello"); err == nil || got {
		t.Fatalf("/proc/stat unreadable: got %t, %v", got, err)
	}
	f.Script("cat /proc/stat", proc.Result{Stdout: "cpu 1 2\n"})
	if got, err := ContainerCreatedBeforeBoot(context.Background(), f, "lever--hello"); err == nil || got {
		t.Fatalf("no btime: got %t, %v", got, err)
	}
}

func TestRemoveStrandedCreated(t *testing.T) {
	const made = "podman inspect --type container --format {{.Created.Unix}} lever--hello"
	for _, c := range []struct {
		name, state, created string
		removed              bool
	}{
		{"stranded", "created", "1759990000\n", true},
		{"created this boot", "created", "1760000100\n", false},
		{"exited", "exited", "1759990000\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := proc.NewFakeRunner()
			f.Script(stateFormat+"lever--hello", proc.Result{Stdout: testID + " lever--hello " + c.state + " false false\n"})
			f.Script(made, proc.Result{Stdout: c.created})
			f.Script("cat /proc/stat", proc.Result{Stdout: procStat})
			f.Script("podman rm "+testID, proc.Result{})
			got, err := ContainerStrandedCreated(context.Background(), f, "lever--hello")
			if err != nil || got != c.removed {
				t.Fatalf("stranded: %t, %v", got, err)
			}
			removed, err := RemoveStrandedCreated(context.Background(), f, "lever--hello")
			if err != nil || removed != c.removed {
				t.Fatalf("got %t, %v; want %t", removed, err, c.removed)
			}
			rm := false
			for _, call := range f.Calls {
				if strings.Contains(call.Name+" "+strings.Join(call.Args, " "), "podman rm") {
					rm = true
					if strings.Contains(call.Name+" "+strings.Join(call.Args, " "), "-f") {
						t.Fatal("never --force")
					}
				}
			}
			if rm != c.removed {
				t.Fatalf("podman rm run: %t, want %t", rm, c.removed)
			}
		})
	}
}
