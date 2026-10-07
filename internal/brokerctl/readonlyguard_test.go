package brokerctl

import (
	"context"
	"errors"
	"path"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

func TestCheckManagerReadOnly(t *testing.T) {
	const jp = "/lever"
	want := []config.TreeMount{{Rel: "assistant", ReadOnly: false}, {Rel: "assistant/tools", ReadOnly: true}}
	mount := func(rel string, rw bool) jail.Mount {
		return jail.Mount{Source: path.Join(jp, rel), Destination: path.Join(scion.ContainerWorkspace, rel), RW: rw}
	}
	full := []jail.Mount{mount("assistant", true), mount("assistant/tools", false)}
	notWritable := func(context.Context, string, string) (bool, error) { return false, nil }
	cases := []struct {
		name   string
		mounts []jail.Mount
		err    error
		probe  func(context.Context, string, string) (bool, error)
		want   string // "" = passes
	}{
		{"held", full, nil, notWritable, ""},
		{"cannot read", nil, errors.New("podman down"), notWritable, "cannot read manager"},
		{"no container", nil, jail.ErrNoContainer, notWritable, "cannot read manager"},
		{"entry missing", []jail.Mount{mount("assistant", true)}, nil, notWritable, "not mounted read-only"},
		{"entry writable", []jail.Mount{mount("assistant", true), mount("assistant/tools", true)}, nil, notWritable, "mounted read-write"},
		{"pin missing", []jail.Mount{mount("assistant/tools", false)}, nil, notWritable, "not pinned"},
		{"replaced on host", full, nil, func(context.Context, string, string) (bool, error) { return true, nil }, "replaced on the host"},
		{"not running", full, nil, func(context.Context, string, string) (bool, error) { return false, errors.New("container stopped") }, "cannot probe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkManagerReadOnly(context.Background(), "m", jp, "lever--m", want,
				func(context.Context) ([]jail.Mount, error) { return tc.mounts, tc.err }, tc.probe)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("want pass, got %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}
