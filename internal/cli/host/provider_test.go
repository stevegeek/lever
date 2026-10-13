package host

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// A required reason opens the window when nothing needs minting, and the
// window links through its hub link.
func TestDevAuthWindowOpensForARequiredReason(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-controller")
	f := patMintRunner("pat-new")
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{Require: "the project has no broker"}); err != nil {
		t.Fatal(err)
	}
	for _, argv := range []string{argvScionServerStart, argvScionHubLink, argvScionServerStop} {
		if n := countCalls(f.Calls, func(c proc.Call) bool { return callHasPrefix(c, argv) }); n == 0 {
			t.Errorf("the window did not run %q: %+v", argv, f.Calls)
		}
	}
	if f.Called(proc.ArgvPrefix("scion", "hub", "token", "create")) {
		t.Fatal("a required window minted a token nothing asked for")
	}
	if tok, _ := st.LoadControllerPAT(); tok != "pat-controller" {
		t.Fatalf("controller PAT = %q, want it kept", tok)
	}
}

// Beside running containers a required reason fails the apply with the repair.
func TestDevAuthWindowRequiredReasonBesideContainers(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-controller")
	f := patMintRunner("pat-new")
	f.Script(argvPodmanPs, proc.Result{Stdout: "assistant\n"})
	err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{Require: "the project has no broker"})
	if err == nil || !strings.Contains(err.Error(), "the project has no broker") || !strings.Contains(err.Error(), "`lever stop`, then `lever up`") {
		t.Fatalf("err = %v, want the reason and the repair", err)
	}
	if f.Called(proc.ArgvPrefix("scion", "server")) {
		t.Fatalf("the window touched the hub beside running containers: %+v", f.Calls)
	}
}

// providerHub answers the project list and its providers list; linked flips
// when the window runs.
type providerHub struct {
	listed    bool
	providers int
	status    int
}

func (h *providerHub) Do(_ context.Context, method, path string) (int, []byte, error) {
	switch {
	case path == "/api/v1/projects":
		if !h.listed {
			return 200, []byte(`{"projects":[]}`), nil
		}
		return 200, []byte(`{"projects":[{"id":"p1","name":"lever","slug":"lever"}]}`), nil
	case path == "/api/v1/projects/p1/providers":
		if h.status != 0 {
			return h.status, []byte(`{}`), nil
		}
		if h.providers == 0 {
			return 200, []byte(`{"providers":[]}`), nil
		}
		return 200, []byte(`{"providers":[{"brokerId":"b1","brokerName":"jail"}]}`), nil
	}
	return 404, nil, nil
}

func TestEnsureProjectProvider(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hub         providerHub
		windowLinks bool
		windowErr   error
		wantWindow  bool
		wantLinked  bool
		wantErr     string
	}{
		{name: "linked already", hub: providerHub{listed: true, providers: 1}},
		{name: "project not on the hub yet", hub: providerHub{}},
		{name: "none: the window links it", hub: providerHub{listed: true}, windowLinks: true, wantWindow: true, wantLinked: true},
		{name: "window refused", hub: providerHub{listed: true}, windowErr: errors.New("containers are running"), wantWindow: true, wantErr: "containers are running"},
		{name: "still none after the window", hub: providerHub{listed: true}, wantWindow: true, wantLinked: true, wantErr: "no runtime broker linked after"},
		{name: "providers unreadable", hub: providerHub{listed: true, status: 403}, wantErr: "HTTP 403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.hub
			windows, starts, waits := 0, 0, 0
			linked, err := ensureProjectProvider(context.Background(), providerRepair{
				live:    &hubapi.Client{T: &h},
				project: "lever",
				window: func(_ context.Context, reason string) error {
					windows++
					if !strings.Contains(reason, "no runtime broker") {
						t.Errorf("window reason = %q", reason)
					}
					if tc.windowLinks {
						h.providers = 1
					}
					return tc.windowErr
				},
				startHub:   func(context.Context) error { starts++; return nil },
				waitBroker: func(context.Context) error { waits++; return nil },
				log:        func(string, ...any) {},
			})
			if (windows > 0) != tc.wantWindow || linked != tc.wantLinked {
				t.Fatalf("windows=%d linked=%v, want window=%v linked=%v", windows, linked, tc.wantWindow, tc.wantLinked)
			}
			if tc.wantWindow && tc.windowErr == nil && (starts != 1 || waits != 1) {
				t.Fatalf("starts=%d waits=%d after the window, want 1 each", starts, waits)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
