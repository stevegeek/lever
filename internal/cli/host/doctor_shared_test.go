package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/scion"
)

func TestCheckSharedFolders(t *testing.T) {
	app := &config.App{Name: "mgr", Workers: []config.Worker{{Name: "w", Dir: "workers/w"}, {Name: "r", Dir: "workers/r"}, {Name: "x", Dir: "workers/x"}},
		SharedFolders: []config.SharedFolder{{Path: "tools/releases", Writers: []string{"w"}, Readers: []string{"r"}}}}
	const project = "/lever"
	running := func(names ...string) agentLister {
		return func(context.Context, string) ([]scion.Agent, error) {
			var out []scion.Agent
			for _, n := range names {
				out = append(out, scion.Agent{Slug: n, ContainerID: "c-" + n, ContainerStatus: "Up 3 minutes"})
			}
			return out, nil
		}
	}
	ro := jail.Mount{Source: "/lever/tools/releases", Destination: "/shared/releases"}
	rw := jail.Mount{Source: "/lever/tools/releases", Destination: "/shared/releases", RW: true}
	inspectOf := func(m map[string][]jail.Mount) mountInspector {
		return func(_ context.Context, ref string) ([]jail.Mount, error) {
			got, ok := m[ref]
			if !ok {
				return nil, jail.ErrNoContainer
			}
			return got, nil
		}
	}
	liveOK := func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) { return nil, nil }
	var liveCalls []string
	liveRecord := func(_ context.Context, ref string, want []jail.LiveMount) ([]jail.LiveMountProblem, error) {
		for _, w := range want {
			liveCalls = append(liveCalls, ref+" "+w.Target+" "+w.Source)
		}
		return nil, nil
	}
	for _, tc := range []struct {
		name    string
		list    agentLister
		mounts  map[string][]jail.Mount
		live    liveMountChecker
		ok      bool
		warn    bool
		details []string
	}{
		{"as configured", running("mgr", "w", "r", "x"),
			map[string][]jail.Mount{"c-mgr": {ro}, "c-w": {rw}, "c-r": {ro}, "c-x": nil}, liveOK, true, false,
			[]string{"mgr, w, r"}},
		{"reader holds read-write", running("r"), map[string][]jail.Mount{"c-r": {rw}}, liveOK, false, false,
			[]string{"r:", "read-write", "read-only"}},
		{"unlisted worker holds it", running("x"), map[string][]jail.Mount{"c-x": {ro}}, liveOK, false, false,
			[]string{"x:", "no longer grants"}},
		{"other source", running("r"), map[string][]jail.Mount{"c-r": {{Source: "/lever/other", Destination: "/shared/releases"}}}, liveOK, false, false,
			[]string{"another source"}},
		{"missing mount warns", running("r"), map[string][]jail.Mount{"c-r": nil}, liveOK, true, true,
			[]string{"r:", "not mounted"}},
		{"live problem fails", running("r"), map[string][]jail.Mount{"c-r": {ro}},
			func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
				return []jail.LiveMountProblem{{Target: "/shared/releases", Reason: jail.LiveMountReplaced}}, nil
			}, false, false, []string{"replaced"}},
		{"live unreadable warns", running("r"), map[string][]jail.Mount{"c-r": {ro}},
			func(context.Context, string, []jail.LiveMount) ([]jail.LiveMountProblem, error) {
				return nil, errors.New("boom")
			}, true, true, []string{"live check could not run"}},
		{"nobody created yet", running(), nil, liveOK, true, false, []string{"no agent that mounts one"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := checkSharedFolders(context.Background(), project, app, tc.list, inspectOf(tc.mounts), nil, tc.live)
			if r.ok != tc.ok || (r.fix != "") != (tc.warn || !tc.ok) {
				t.Fatalf("ok=%v fix=%q, want ok=%v warn=%v; detail=%s", r.ok, r.fix, tc.ok, tc.warn, r.detail)
			}
			for _, d := range tc.details {
				if !strings.Contains(r.detail, d) {
					t.Fatalf("detail %q lacks %q", r.detail, d)
				}
			}
		})
	}
	// The live check covers read-only mounts only, from the folder in the
	// in-jail tree.
	liveCalls = nil
	checkSharedFolders(context.Background(), project, app, running("w", "r"), inspectOf(map[string][]jail.Mount{"c-w": {rw}, "c-r": {ro}}), nil, liveRecord)
	if len(liveCalls) != 1 || liveCalls[0] != "c-r /shared/releases /lever/tools/releases" {
		t.Fatalf("live calls = %q, want the reader's read-only mount only", liveCalls)
	}
	// Nothing configured and nothing mounted.
	none := &config.App{Name: "mgr"}
	if r := checkSharedFolders(context.Background(), project, none, running("mgr"), inspectOf(map[string][]jail.Mount{"c-mgr": nil}), nil, liveOK); !r.ok || r.detail != "none configured" {
		t.Fatalf("none: %+v", r)
	}
	// Folders removed from the config, a mount left behind: fails.
	if r := checkSharedFolders(context.Background(), project, none, running("mgr"), inspectOf(map[string][]jail.Mount{"c-mgr": {rw}}), nil, liveOK); r.ok {
		t.Fatalf("a leftover mount must fail: %+v", r)
	}
}
