package host

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/state"
)

// fakeExperimentsHub answers scion's experiments admin route the way b41cd6fe
// does (merge on PUT, compare-and-set on expected_revision) and the two probe
// routes (404 while an experiment is off).
type fakeExperimentsHub struct {
	registered []string
	overrides  map[string]bool
	revision   int64
	malformed  bool
	// noRoute makes every experiments route 404, as on an older scion.
	noRoute bool
	// conflicts answers this many PUTs with revision_conflict.
	conflicts int
	putStatus int
	// lie keeps every experiment enabled in the PUT response.
	lie bool
	// probeStatus overrides the probe answer for every probe route.
	probeStatus int
	probeErr    error
	calls       []string
	puts        []map[string]*bool
}

func newFakeExperimentsHub() *fakeExperimentsHub {
	return &fakeExperimentsHub{registered: []string{"hub.artifacts", "hub.conduit", "web.agent_edit"}, overrides: map[string]bool{}}
}

func (h *fakeExperimentsHub) enabled(name string) bool {
	if h.malformed && name != "web.agent_edit" {
		return false
	}
	return h.overrides[name]
}

func (h *fakeExperimentsHub) view() []byte {
	var items []map[string]any
	for _, n := range h.registered {
		var ov *bool
		if v, ok := h.overrides[n]; ok {
			vv := v
			ov = &vv
		}
		items = append(items, map[string]any{"name": n, "override": ov, "enabled": h.enabled(n) || h.lie})
	}
	b, _ := json.Marshal(map[string]any{"revision": h.revision, "malformed": h.malformed, "experiments": items})
	return b
}

func (h *fakeExperimentsHub) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return h.DoBody(ctx, method, path, nil)
}

func (h *fakeExperimentsHub) DoBody(_ context.Context, method, path string, body []byte) (int, []byte, error) {
	h.calls = append(h.calls, method+" "+path)
	notFound := []byte(`{"error":{"code":"not_found","message":"route not found"}}`)
	for _, e := range pinnedOffExperiments {
		if method == http.MethodGet && path == e.probe {
			if h.probeErr != nil {
				return 0, nil, h.probeErr
			}
			if h.probeStatus != 0 {
				return h.probeStatus, nil, nil
			}
			if h.noRoute || !h.enabled(e.name) {
				return 404, notFound, nil
			}
			return 200, []byte(`{}`), nil
		}
	}
	if path != "/api/v1/admin/experiments" || h.noRoute {
		return 404, []byte("404 page not found"), nil
	}
	switch method {
	case http.MethodGet:
		return 200, h.view(), nil
	case http.MethodPut:
		var req struct {
			Overrides        map[string]*bool `json:"overrides"`
			ExpectedRevision *int64           `json:"expected_revision"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.ExpectedRevision == nil {
			return 400, []byte(`{"error":{"code":"validation_failed"}}`), nil
		}
		h.puts = append(h.puts, req.Overrides)
		if h.putStatus != 0 {
			return h.putStatus, []byte(`{"error":{"code":"forbidden"}}`), nil
		}
		if h.malformed {
			return 409, []byte(`{"error":{"code":"experiments_malformed"}}`), nil
		}
		if h.conflicts > 0 {
			h.conflicts--
			h.revision++ // another writer
			return 409, []byte(`{"error":{"code":"revision_conflict"}}`), nil
		}
		if *req.ExpectedRevision != h.revision {
			return 409, []byte(`{"error":{"code":"revision_conflict"}}`), nil
		}
		for n, v := range req.Overrides {
			if !slices.Contains(h.registered, n) {
				return 400, []byte(`{"error":{"code":"validation_failed"}}`), nil
			}
			if v == nil {
				delete(h.overrides, n)
				continue
			}
			h.overrides[n] = *v
		}
		h.revision++
		return 200, h.view(), nil
	}
	return 405, nil, nil
}

func (h *fakeExperimentsHub) putCount() int { return len(h.puts) }

func knowsExperiments(has bool) scopeKnownFunc {
	return func(_ context.Context, scope string) (bool, error) {
		return scope == experimentsScope && has, nil
	}
}

func TestPinExperimentsOff(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		setup    func(h *fakeExperimentsHub)
		wantErr  error
		anyErr   bool
		wantRec  state.HubExperimentsRecord
		wantPuts int
		wantKept map[string]bool
	}{
		{name: "fresh hub", setup: func(*fakeExperimentsHub) {},
			wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts"}, Revision: 1}, wantPuts: 1},
		{name: "already pinned writes nothing", setup: func(h *fakeExperimentsHub) {
			h.overrides = map[string]bool{"hub.conduit": false, "hub.artifacts": false}
			h.revision = 7
		}, wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts"}, Revision: 7}},
		{name: "an admin turned conduit on; other overrides kept", setup: func(h *fakeExperimentsHub) {
			h.overrides = map[string]bool{"hub.conduit": true, "hub.artifacts": false, "web.agent_edit": true}
			h.revision = 2
		}, wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts"}, Revision: 3}, wantPuts: 1,
			wantKept: map[string]bool{"web.agent_edit": true}},
		{name: "older scion", setup: func(h *fakeExperimentsHub) { h.noRoute = true },
			wantRec: state.HubExperimentsRecord{NotApplicable: true}},
		{name: "hub registers only conduit", setup: func(h *fakeExperimentsHub) { h.registered = []string{"hub.conduit"} },
			wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit"}, Revision: 1}, wantPuts: 1},
		{name: "hub registers neither", setup: func(h *fakeExperimentsHub) { h.registered = []string{"web.agent_edit"} },
			wantRec: state.HubExperimentsRecord{}},
		{name: "malformed store", setup: func(h *fakeExperimentsHub) { h.malformed = true }, wantErr: errExperimentsMalformed},
		{name: "one concurrent write is retried", setup: func(h *fakeExperimentsHub) { h.conflicts = 1 },
			wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts"}, Revision: 2}, wantPuts: 2},
		{name: "persistent conflicts fail", setup: func(h *fakeExperimentsHub) { h.conflicts = 10 }, wantErr: hubapi.ErrExperimentsConflict, wantPuts: pinConflictRetries},
		{name: "refused write fails", setup: func(h *fakeExperimentsHub) { h.putStatus = 403 }, anyErr: true, wantPuts: 1},
		{name: "a hub still reporting it on fails", setup: func(h *fakeExperimentsHub) { h.lie = true }, anyErr: true, wantPuts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeExperimentsHub()
			tc.setup(h)
			rec, err := pinExperimentsOff(context.Background(), &hubapi.Client{T: h}, now)
			for _, put := range h.puts {
				for n, v := range put {
					if v == nil || *v {
						t.Fatalf("PUT sent %s=%v; lever only ever writes false", n, v)
					}
				}
			}
			if h.putCount() != tc.wantPuts {
				t.Fatalf("PUTs = %d, want %d (%v)", h.putCount(), tc.wantPuts, h.calls)
			}
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			case tc.anyErr:
				if err == nil {
					t.Fatal("want an error")
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			if rec.NotApplicable != tc.wantRec.NotApplicable || rec.Revision != tc.wantRec.Revision ||
				!sameScopeSet(rec.Off, tc.wantRec.Off) || !rec.PinnedAt.Equal(now) {
				t.Fatalf("record = %+v, want %+v", rec, tc.wantRec)
			}
			for n, v := range tc.wantKept {
				if got, ok := h.overrides[n]; !ok || got != v {
					t.Fatalf("override %s = %v,%v; want kept at %v", n, got, ok, v)
				}
			}
		})
	}
}

func TestCheckHubExperimentsForApply(t *testing.T) {
	down := errors.New("connection refused")
	pinned := &state.HubExperimentsRecord{Off: pinnedOffNames(), Revision: 1}
	for _, tc := range []struct {
		name       string
		known      scopeKnownFunc
		rec        *state.HubExperimentsRecord
		hub        func(h *fakeExperimentsHub)
		wantHas    bool
		wantReason string
		wantUrgent bool
		wantProbe  bool
	}{
		{name: "older scion: nothing, no probe", known: knowsExperiments(false)},
		{name: "scion unknown, no record: nothing", known: nil},
		{name: "scion unknown, pinned before", known: nil, rec: pinned, wantHas: true, wantProbe: true},
		{name: "scion cannot be asked, pinned before",
			known: func(context.Context, string) (bool, error) { return false, errors.New("no scion") },
			rec:   pinned, wantHas: true, wantProbe: true},
		{name: "not pinned yet", known: knowsExperiments(true), wantHas: true, wantReason: "not pinned off", wantProbe: true},
		{name: "recorded not applicable before a pin bump", known: knowsExperiments(true),
			rec: &state.HubExperimentsRecord{NotApplicable: true}, wantHas: true, wantReason: "not pinned off", wantProbe: true},
		{name: "pinned and off", known: knowsExperiments(true), rec: pinned, wantHas: true, wantProbe: true},
		{name: "pinned but live", known: knowsExperiments(true), rec: pinned,
			hub:     func(h *fakeExperimentsHub) { h.overrides["hub.conduit"] = true },
			wantHas: true, wantReason: "hub.conduit is on", wantUrgent: true, wantProbe: true},
		{name: "both live", known: knowsExperiments(true), rec: pinned,
			hub:     func(h *fakeExperimentsHub) { h.overrides["hub.conduit"], h.overrides["hub.artifacts"] = true, true },
			wantHas: true, wantReason: "hub.conduit and hub.artifacts is on", wantUrgent: true, wantProbe: true},
		{name: "hub down, pinned", known: knowsExperiments(true), rec: pinned,
			hub: func(h *fakeExperimentsHub) { h.probeErr = down }, wantHas: true, wantProbe: true},
		{name: "hub down, not pinned", known: knowsExperiments(true),
			hub:     func(h *fakeExperimentsHub) { h.probeErr = down },
			wantHas: true, wantReason: "not pinned off", wantProbe: true},
		{name: "PAT refused", known: knowsExperiments(true), rec: pinned,
			hub: func(h *fakeExperimentsHub) { h.probeStatus = 401 }, wantHas: true, wantProbe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := state.ForConfig(t.TempDir())
			if tc.rec != nil {
				if err := st.SaveHubExperimentsRecord(*tc.rec); err != nil {
					t.Fatal(err)
				}
			}
			h := newFakeExperimentsHub()
			if tc.hub != nil {
				tc.hub(h)
			}
			c, err := checkHubExperimentsForApply(context.Background(), &hubapi.Client{T: h}, st, tc.known)
			if err != nil {
				t.Fatal(err)
			}
			if c.has != tc.wantHas || c.urgent != tc.wantUrgent || (tc.wantReason == "") != (c.reason == "") ||
				!strings.Contains(c.reason, tc.wantReason) {
				t.Fatalf("check = %+v, want has=%v urgent=%v reason~%q", c, tc.wantHas, tc.wantUrgent, tc.wantReason)
			}
			if probed := len(h.calls) > 0; probed != tc.wantProbe {
				t.Fatalf("probed = %v, want %v (%v)", probed, tc.wantProbe, h.calls)
			}
		})
	}
}

func TestCheckHubExperimentsForApplyCorruptRecord(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.HubExperiments(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := checkHubExperimentsForApply(context.Background(), &hubapi.Client{T: newFakeExperimentsHub()}, st, knowsExperiments(true)); err == nil {
		t.Fatal("a corrupt record must fail, not read as never pinned")
	}
}

// argvShCurl is the jail curl of a hubapi.JailCurl request.
const argvShCurl = "sh -c exec curl"

// curlAnswer is the jail curl output for one status: body, newline, status.
func curlAnswer(status string) proc.Result { return proc.Result{Stdout: "{}\n" + status} }

// A first apply on a scion with experiments pins both off in the window it
// opens for that alone, and records it.
func TestEnsureControllerPATPinsExperimentsOff(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-controller")
	f := patMintRunner("unused")
	f.Script(argvShCurl, curlAnswer("404"))
	hub := newFakeExperimentsHub()
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{},
		patMintOpts{AdminHub: hub, ScopeKnown: knowsExperiments(true)}); err != nil {
		t.Fatal(err)
	}
	if hub.overrides["hub.conduit"] || hub.overrides["hub.artifacts"] {
		t.Fatalf("overrides = %v", hub.overrides)
	}
	if _, ok := hub.overrides["hub.conduit"]; !ok || hub.putCount() != 1 {
		t.Fatalf("no explicit override written: %v", hub.calls)
	}
	rec, found, err := st.LoadHubExperimentsRecord()
	if err != nil || !found || !sameScopeSet(rec.Off, pinnedOffNames()) {
		t.Fatalf("record = %+v found=%v err=%v", rec, found, err)
	}
	if f.Called(proc.ArgvPrefix("scion", "hub", "token", "create")) {
		t.Fatal("the experiments pin must not re-mint a current token")
	}
	if !f.Called(proc.ArgvContains("/api/v1/conduit/grant-keys")) {
		t.Fatalf("the live hub was not probed: %+v", f.Calls)
	}

	// Pinned and off: the next apply opens no window.
	n := len(f.Calls)
	hub.calls = nil
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{},
		patMintOpts{AdminHub: hub, ScopeKnown: knowsExperiments(true)}); err != nil {
		t.Fatal(err)
	}
	if countCalls(f.Calls[n:], func(c proc.Call) bool { return callHasPrefix(c, argvScionServerStop) }) > 0 {
		t.Fatalf("a window opened with nothing to do: %+v", f.Calls[n:])
	}
	if len(hub.calls) != 0 {
		t.Fatalf("admin calls with nothing to do: %v", hub.calls)
	}
}

// A live pinned-off experiment beside running containers fails the apply with
// the repair and touches nothing.
func TestEnsureControllerPATLiveExperimentBesideContainersFails(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-controller")
	if err := st.SaveHubExperimentsRecord(state.HubExperimentsRecord{Off: pinnedOffNames(), Revision: 1}); err != nil {
		t.Fatal(err)
	}
	f := patMintRunner("unused")
	f.Script(argvPodmanPs, proc.Result{Stdout: "assistant\n"})
	f.Script(argvShCurl, curlAnswer("200"))
	hub := newFakeExperimentsHub()
	err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{},
		patMintOpts{AdminHub: hub, ScopeKnown: knowsExperiments(true)})
	if err == nil || !strings.Contains(err.Error(), "hub.conduit and hub.artifacts is on") ||
		!strings.Contains(err.Error(), "`lever stop`, then `lever up`") || !strings.Contains(err.Error(), "assistant") {
		t.Fatalf("err = %v, want the live experiments, the containers and the repair", err)
	}
	if f.Called(proc.ArgvPrefix("scion", "server")) || len(hub.calls) != 0 {
		t.Fatalf("the window opened beside running containers: jail %+v, hub %v", f.Calls, hub.calls)
	}
}

// A pin that is only wanted (never made, nothing live) waits for a window
// without containers, with the usual warning.
func TestEnsureControllerPATExperimentsPinSkippedBesideContainers(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-controller")
	f := patMintRunner("unused")
	f.Script(argvPodmanPs, proc.Result{Stdout: "assistant\n"})
	f.Script(argvShCurl, curlAnswer("404"))
	warned, warn := collectWarnings()
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{},
		patMintOpts{AdminHub: newFakeExperimentsHub(), ScopeKnown: knowsExperiments(true), Warn: warn}); err != nil {
		t.Fatalf("a pin not yet made must not fail apply: %v", err)
	}
	if len(*warned) != 1 || !strings.Contains((*warned)[0], "skipped the dev-auth window") || !strings.Contains((*warned)[0], "not pinned off") {
		t.Fatalf("warnings = %q", *warned)
	}
}

// A pin the hub refuses fails the window, which restarts the live hub.
func TestEnsureControllerPATExperimentsPinFailureFailsTheWindow(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-controller")
	f := patMintRunner("unused")
	f.Script(argvShCurl, curlAnswer("404"))
	hub := newFakeExperimentsHub()
	hub.putStatus = 403
	restarted := 0
	err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{}, patMintOpts{
		AdminHub: hub, ScopeKnown: knowsExperiments(true),
		RestartHub: func(context.Context) error { restarted++; return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "pinning scion experiments off") || restarted != 1 {
		t.Fatalf("err = %v restarted = %d, want the pin failure and one restart", err, restarted)
	}
	if _, found, _ := st.LoadHubExperimentsRecord(); found {
		t.Fatal("a failed pin must not be recorded")
	}
}

// On a scion without experiments nothing about them runs: no probe, no admin
// call, no window.
func TestEnsureControllerPATOlderScionSkipsExperiments(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	seedPAT(t, st, "controller", "pat-controller")
	f := proc.NewFakeRunner()
	hub := newFakeExperimentsHub()
	if err := ensureControllerPAT(context.Background(), f, st, t.TempDir(), "/lever", remoteAccess{},
		patMintOpts{AdminHub: hub, ScopeKnown: knowsExperiments(false)}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 0 || len(hub.calls) != 0 {
		t.Fatalf("calls on an older scion: jail %+v, hub %v", f.Calls, hub.calls)
	}
}

func TestEnsureHubExperimentsOff(t *testing.T) {
	pinned := state.HubExperimentsRecord{Off: pinnedOffNames(), Revision: 1}
	for _, tc := range []struct {
		name string
		// liveBefore turns conduit on at the live hub before the check.
		liveBefore bool
		// windowFixes makes the window pin it off, as the real one does.
		windowFixes bool
		windowErr   error
		startErr    error
		probeAfter  error
		probeBefore error
		// scionUnsure: the jail's scion cannot be asked and nothing was
		// recorded.
		scionUnsure bool
		olderScion  bool
		wantWindow  bool
		wantStart   bool
		wantErr     string
		wantProbe   bool
	}{
		{name: "off: nothing to do", wantProbe: true},
		{name: "older scion: no probe", olderScion: true, liveBefore: true},
		{name: "scion unsure, off", scionUnsure: true, wantProbe: true},
		{name: "scion unsure, live fails closed", scionUnsure: true, liveBefore: true, windowFixes: true, wantProbe: true, wantErr: "cannot pin it off"},
		{name: "scion unsure, no answer from the probe", scionUnsure: true, probeBefore: errors.New("refused"), wantProbe: true, wantErr: "could not tell"},
		{name: "no answer from the probe fails closed", probeBefore: errors.New("refused"), wantErr: "could not tell", wantProbe: true},
		{name: "live: pinned off and restarted", liveBefore: true, windowFixes: true, wantWindow: true, wantStart: true, wantProbe: true},
		{name: "window refused", liveBefore: true, windowErr: errors.New("containers are running"), wantWindow: true, wantErr: "containers are running", wantProbe: true},
		{name: "restart failed", liveBefore: true, windowFixes: true, startErr: errors.New("boom"), wantWindow: true, wantStart: true, wantErr: "starting the hub", wantProbe: true},
		{name: "still live after the restart", liveBefore: true, wantWindow: true, wantStart: true, wantErr: "still on", wantProbe: true},
		{name: "unconfirmed after the restart", liveBefore: true, windowFixes: true, probeAfter: errors.New("refused"), wantWindow: true, wantStart: true, wantErr: "could not confirm", wantProbe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := state.ForConfig(t.TempDir())
			known := knowsExperiments(!tc.olderScion)
			if tc.scionUnsure {
				known = func(context.Context, string) (bool, error) { return false, errors.New("no scion") }
			} else if err := st.SaveHubExperimentsRecord(pinned); err != nil {
				t.Fatal(err)
			}
			h := newFakeExperimentsHub()
			h.overrides["hub.conduit"] = tc.liveBefore
			h.probeErr = tc.probeBefore
			windows, starts := 0, 0
			err := ensureHubExperimentsOff(context.Background(), experimentsRepair{
				live: &hubapi.Client{T: h}, st: st, known: known,
				window: func(context.Context) error {
					windows++
					if tc.windowFixes {
						h.overrides["hub.conduit"] = false
					}
					return tc.windowErr
				},
				startHub: func(context.Context) error {
					starts++
					h.probeErr = tc.probeAfter
					return tc.startErr
				},
				log: func(string, ...any) {},
			})
			if (windows > 0) != tc.wantWindow || (starts > 0) != tc.wantStart {
				t.Fatalf("windows=%d starts=%d, want window=%v start=%v", windows, starts, tc.wantWindow, tc.wantStart)
			}
			if tc.wantProbe != (len(h.calls) > 0) {
				t.Fatalf("probed = %v, want %v (%v)", len(h.calls) > 0, tc.wantProbe, h.calls)
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
