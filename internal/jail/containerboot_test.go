package jail

import (
	"context"
	"errors"
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
