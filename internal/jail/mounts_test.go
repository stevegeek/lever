package jail

import (
	"context"
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

func TestContainerMountTargetsBadJSON(t *testing.T) {
	host := proc.NewFakeRunner()
	host.Script("orb", proc.Result{Stdout: "not json"})
	jr := New(Config{Host: host, Prefix: orbPrefix("lever-x", "u"), UID: "501"})
	if _, err := ContainerMountTargets(context.Background(), jr, "abc"); err == nil {
		t.Fatal("want a parse error")
	}
}

func contains(s, sub string) bool { return len(sub) == 0 || len(s) >= len(sub) && (indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
