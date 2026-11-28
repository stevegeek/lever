package host

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/state"
)

// fakeExperimentsHub answers scion's experiments admin route the way b41cd6fe
// does (merge on PUT, compare-and-set on expected_revision) and the probe
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
	// defaultOn are experiments on without an override, as scion's
	// deployment default turns hub.attachment_artifacts on.
	defaultOn map[string]bool
	// served are routes answered 200 whatever the experiments say, as
	// e8160a10 serves the graduated thread groups.
	served []string
	calls  []string
	puts   []map[string]*bool
}

func newFakeExperimentsHub() *fakeExperimentsHub {
	return &fakeExperimentsHub{registered: []string{"federation.project_visitors", "hub.artifacts", "hub.chat_thread_groups", "hub.conduit", "web.agent_edit"}, overrides: map[string]bool{}}
}

func (h *fakeExperimentsHub) enabled(name string) bool {
	if h.malformed && name != "web.agent_edit" {
		return false
	}
	if v, ok := h.overrides[name]; ok {
		return v
	}
	return h.defaultOn[name]
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
	if method == http.MethodGet && slices.Contains(h.served, path) {
		return 200, []byte(`{}`), nil
	}
	notFound := []byte(`{"error":{"code":"not_found","message":"route not found"}}`)
	for _, e := range pinnedOffExperiments {
		if e.probe != "" && method == http.MethodGet && path == e.probe {
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
			wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts", "hub.chat_thread_groups", "federation.project_visitors"}, Revision: 1}, wantPuts: 1},
		{name: "already pinned writes nothing", setup: func(h *fakeExperimentsHub) {
			h.overrides = map[string]bool{"hub.conduit": false, "hub.artifacts": false, "hub.chat_thread_groups": false, "federation.project_visitors": false}
			h.revision = 7
		}, wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts", "hub.chat_thread_groups", "federation.project_visitors"}, Revision: 7}},
		{name: "an admin turned conduit on; other overrides kept", setup: func(h *fakeExperimentsHub) {
			h.overrides = map[string]bool{"hub.conduit": true, "hub.artifacts": false, "hub.chat_thread_groups": false,
				"federation.project_visitors": false, "web.agent_edit": true}
			h.revision = 2
		}, wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts", "hub.chat_thread_groups", "federation.project_visitors"}, Revision: 3}, wantPuts: 1,
			wantKept: map[string]bool{"web.agent_edit": true}},
		{name: "older scion", setup: func(h *fakeExperimentsHub) { h.noRoute = true },
			wantRec: state.HubExperimentsRecord{NotApplicable: true}},
		{name: "hub registers only conduit", setup: func(h *fakeExperimentsHub) { h.registered = []string{"hub.conduit"} },
			wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit"}, Revision: 1}, wantPuts: 1},
		{name: "hub registers none", setup: func(h *fakeExperimentsHub) { h.registered = []string{"web.agent_edit"} },
			wantRec: state.HubExperimentsRecord{}},
		{name: "an older scion registers two", setup: func(h *fakeExperimentsHub) {
			h.registered = []string{"hub.artifacts", "hub.conduit", "web.agent_edit"}
		}, wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts"}, Revision: 1}, wantPuts: 1},
		{name: "federation on, which no probe sees", setup: func(h *fakeExperimentsHub) {
			h.overrides = map[string]bool{"hub.conduit": false, "hub.artifacts": false, "hub.chat_thread_groups": false,
				"federation.project_visitors": true}
			h.revision = 4
		}, wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts", "hub.chat_thread_groups", "federation.project_visitors"}, Revision: 5}, wantPuts: 1},
		{name: "e8160a10: thread groups graduated, attachment artifacts on by default", setup: newScionExperiments,
			wantRec: state.HubExperimentsRecord{Off: newScionPinned, Revision: 1}, wantPuts: 1},
		{name: "malformed store", setup: func(h *fakeExperimentsHub) { h.malformed = true }, wantErr: errExperimentsMalformed},
		{name: "one concurrent write is retried", setup: func(h *fakeExperimentsHub) { h.conflicts = 1 },
			wantRec: state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts", "hub.chat_thread_groups", "federation.project_visitors"}, Revision: 2}, wantPuts: 2},
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
				!sameScopeSet(rec.Off, tc.wantRec.Off) || !rec.PinnedAt.Equal(now) ||
				(!rec.NotApplicable && !sameScopeSet(rec.Asked, pinnedNames())) {
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

// A corrupt pin record fails the apply rather than read as never pinned.
func TestEnsureHubExperimentsOffCorruptRecord(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.HubExperiments(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newFakeExperimentsHub()
	_, err := ensureHubExperiments(context.Background(), experimentsPin{admin: &hubapi.Client{T: h}, st: st,
		known: knowsExperiments(true), now: time.Now, log: func(string, ...any) {}, warn: func(string, ...any) {}})
	if err == nil || len(h.calls) != 0 {
		t.Fatalf("err = %v calls = %v, want a failure before any hub call", err, h.calls)
	}
}

func TestEnsureHubExperimentsOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		// liveBefore turns conduit on at the hub before the apply.
		liveBefore bool
		// restartKeeps leaves conduit served after the restart, as a hub whose
		// pin did not take.
		restartKeeps bool
		restartErr   error
		probeBefore  error
		probeAfter   error
		putStatus    int
		malformed    bool
		scionUnsure  bool
		olderScion   bool
		wantPut      bool
		wantRestart  bool
		wantRecord   bool
		wantWarn     bool
		wantErr      string
		wantCalls    bool
	}{
		{name: "off: pinned and proved", wantPut: true, wantRecord: true, wantCalls: true},
		{name: "older scion: nothing", olderScion: true, liveBefore: true},
		{name: "scion unsure, off", scionUnsure: true, wantCalls: true},
		{name: "scion unsure, live fails closed", scionUnsure: true, liveBefore: true, wantErr: "cannot pin it off", wantCalls: true},
		{name: "no answer from the probe fails closed", probeBefore: errors.New("refused"), wantErr: "could not tell", wantCalls: true},
		{name: "live: pinned off and the hub restarted", liveBefore: true, wantPut: true, wantRestart: true, wantRecord: true, wantCalls: true},
		{name: "pin refused", putStatus: 403, wantPut: true, wantErr: "pinning", wantCalls: true},
		{name: "malformed store, nothing live: a warning", malformed: true, wantWarn: true, wantCalls: true},
		{name: "malformed store, live: fails", malformed: true, liveBefore: true, wantErr: "pinning", wantCalls: true},
		{name: "restart failed", liveBefore: true, restartErr: errors.New("boom"), wantPut: true, wantRestart: true, wantRecord: true,
			wantErr: "restarting the hub", wantCalls: true},
		{name: "still live after the restart", liveBefore: true, restartKeeps: true, wantPut: true, wantRestart: true, wantRecord: true,
			wantErr: "still on", wantCalls: true},
		{name: "unconfirmed after the pin", probeAfter: errors.New("refused"), wantPut: true, wantRecord: true,
			wantErr: "could not confirm", wantCalls: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := state.ForConfig(t.TempDir())
			known := knowsExperiments(!tc.olderScion)
			if tc.scionUnsure {
				known = func(context.Context, string) (bool, error) { return false, errors.New("no scion") }
			}
			h := newFakeExperimentsHub()
			if tc.liveBefore {
				h.overrides["hub.conduit"] = true
			}
			h.probeErr, h.putStatus, h.malformed = tc.probeBefore, tc.putStatus, tc.malformed
			if tc.malformed && tc.liveBefore {
				// A malformed store resolves experiments off at the hub, but the
				// probe is what lever trusts.
				h.probeStatus = 200
			}
			restarts, warns := 0, 0
			// The probes after the pin see probeAfter, or, after a restart that
			// kept it, conduit still served.
			afterPin := func() {
				h.probeErr = tc.probeAfter
				if tc.restartKeeps {
					h.probeStatus = 200
				}
			}
			p := experimentsPin{
				admin: &hubapi.Client{T: &secondProbeHub{fakeExperimentsHub: h, after: afterPin}}, st: st, known: known, now: time.Now,
				restartHub: func(context.Context) error { restarts++; return tc.restartErr },
				log:        func(string, ...any) {},
				warn:       func(string, ...any) { warns++ },
			}
			_, err := ensureHubExperiments(context.Background(), p)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if (h.putCount() > 0) != tc.wantPut || (restarts > 0) != tc.wantRestart || (warns > 0) != tc.wantWarn {
				t.Fatalf("puts=%d restarts=%d warns=%d, want put=%v restart=%v warn=%v (%v)",
					h.putCount(), restarts, warns, tc.wantPut, tc.wantRestart, tc.wantWarn, h.calls)
			}
			if (len(h.calls) > 0) != tc.wantCalls {
				t.Fatalf("hub calls = %v, want calls=%v", h.calls, tc.wantCalls)
			}
			if _, found, _ := st.LoadHubExperimentsRecord(); found != tc.wantRecord {
				t.Fatalf("record found = %v, want %v", found, tc.wantRecord)
			}
			for _, put := range h.puts {
				for n, v := range put {
					if v == nil || *v {
						t.Fatalf("PUT sent %s=%v; lever only ever writes false", n, v)
					}
				}
			}
		})
	}
}

// secondProbeHub runs after right before the second probe round: the probes
// from then on are the ones after the pin (and any restart).
type secondProbeHub struct {
	*fakeExperimentsHub
	after  func()
	rounds int
}

func (a *secondProbeHub) Do(ctx context.Context, method, path string) (int, []byte, error) {
	return a.DoBody(ctx, method, path, nil)
}

func (a *secondProbeHub) DoBody(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	if method == http.MethodGet && path == pinnedOffExperiments[0].probe {
		if a.rounds++; a.rounds == 2 {
			a.after()
		}
	}
	return a.fakeExperimentsHub.DoBody(ctx, method, path, body)
}

func TestEnsureHubExperimentsConduitOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// liveBefore: conduit already on and served.
		liveBefore bool
		// policy answers conduitPolicyLive, one entry per call (the last
		// repeats); nil func when noCheck.
		policy      []error
		noCheck     bool
		olderScion  bool
		wantOn      bool
		wantRestart int
		wantErr     string
		wantWarn    bool
		wantTrueOut bool // the hub ends with conduit overridden on
	}{
		{name: "policy live: on, hub restarted", policy: []error{nil}, wantOn: true, wantRestart: 1, wantTrueOut: true},
		{name: "already on: no restart", liveBefore: true, policy: []error{nil}, wantOn: true, wantTrueOut: true},
		{name: "policy not live: stays off", policy: []error{errors.New("not lever's")}, wantWarn: true},
		{name: "policy not live, conduit was on: pinned off and restarted", liveBefore: true, policy: []error{errors.New("not lever's")},
			wantWarn: true, wantRestart: 1},
		{name: "no way to check the policy: stays off", noCheck: true, wantWarn: true},
		{name: "policy lost in the restart: backed off", policy: []error{nil, errors.New("gone")}, wantRestart: 2,
			wantErr: "lever pinned conduit off again"},
		{name: "no hub experiments: stays off", olderScion: true, policy: []error{nil}, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := state.ForConfig(t.TempDir())
			h := newFakeExperimentsHub()
			if tc.liveBefore {
				h.overrides["hub.conduit"] = true
			}
			restarts, warns, checks := 0, 0, 0
			p := experimentsPin{
				admin: &hubapi.Client{T: h}, st: st, known: knowsExperiments(!tc.olderScion), now: time.Now,
				restartHub: func(context.Context) error { restarts++; return nil },
				conduitOn:  true,
				log:        func(string, ...any) {},
				warn:       func(string, ...any) { warns++ },
			}
			if !tc.noCheck {
				p.conduitPolicyLive = func(context.Context) error {
					e := tc.policy[min(checks, len(tc.policy)-1)]
					checks++
					return e
				}
			}
			on, err := ensureHubExperiments(context.Background(), p)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if on != tc.wantOn || restarts != tc.wantRestart || (warns > 0) != tc.wantWarn {
				t.Fatalf("on=%v restarts=%d warns=%d; want on=%v restarts=%d warn=%v (%v)", on, restarts, warns, tc.wantOn, tc.wantRestart, tc.wantWarn, h.calls)
			}
			if h.overrides["hub.conduit"] != tc.wantTrueOut {
				t.Fatalf("hub.conduit override = %v, want %v", h.overrides["hub.conduit"], tc.wantTrueOut)
			}
			for _, put := range h.puts {
				for n, v := range put {
					if v != nil && *v && n != "hub.conduit" {
						t.Fatalf("PUT turned %s on", n)
					}
				}
			}
			for _, n := range []string{"hub.artifacts", "hub.chat_thread_groups", "federation.project_visitors"} {
				if !tc.olderScion && h.overrides[n] {
					t.Fatalf("%s left on", n)
				}
			}
			rec, found, _ := st.LoadHubExperimentsRecord()
			if tc.olderScion {
				return
			}
			if !found || slices.Contains(rec.On, "hub.conduit") != tc.wantTrueOut || slices.Contains(rec.Off, "hub.conduit") == tc.wantTrueOut {
				t.Fatalf("record = %+v (found %v)", rec, found)
			}
		})
	}
}

func TestPinExperimentsConduitOnTouchesOnlyConduit(t *testing.T) {
	h := newFakeExperimentsHub()
	h.overrides["web.agent_edit"] = true
	rec, err := pinExperiments(context.Background(), &hubapi.Client{T: h}, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.On, []string{"hub.conduit"}) || slices.Contains(rec.Off, "hub.conduit") || len(rec.Off) != 3 {
		t.Fatalf("record = %+v", rec)
	}
	if !h.overrides["hub.conduit"] || !h.overrides["web.agent_edit"] || h.overrides["hub.artifacts"] {
		t.Fatalf("overrides = %v", h.overrides)
	}
	h = newFakeExperimentsHub()
	h.registered = []string{"hub.artifacts"}
	if _, err := pinExperiments(context.Background(), &hubapi.Client{T: h}, time.Now(), true); err == nil {
		t.Fatal("a hub without hub.conduit cannot have it turned on")
	}
}

// TestPrePinExperiments: before a restart lever pins every experiment off
// and keeps conduit on only where `lever ssh` wants it and the hub serves it
// already, so the one restart takes the pin and hub-identity has none left
// to restart for; it never turns conduit on, and writes nothing on a hub
// that has no experiments or that it cannot ask.
func TestPrePinExperiments(t *testing.T) {
	now := func() time.Time { return time.Unix(0, 0) }
	for _, c := range []struct {
		name               string
		on                 []string
		conduitWanted      bool
		has                bool
		probeErr           error
		wantErr            bool
		wantOn             []string
		wantPuts, wantSave bool
	}{
		{name: "stray experiments go off", on: []string{"hub.artifacts", "hub.conduit"}, has: true, wantPuts: true, wantSave: true},
		{name: "conduit wanted and served stays on", on: []string{"hub.conduit", "hub.artifacts"}, conduitWanted: true, has: true, wantOn: []string{"hub.conduit"}, wantPuts: true, wantSave: true},
		{name: "conduit wanted but not served is not turned on", conduitWanted: true, has: true, wantPuts: true, wantSave: true},
		{name: "no experiments at the jail's scion", on: []string{"hub.artifacts"}, has: false},
		{name: "a probe that gets no answer writes nothing", on: []string{"hub.artifacts"}, has: true, probeErr: errors.New("jail exec"), wantErr: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newFakeExperimentsHub()
			for _, n := range c.on {
				h.overrides[n] = true
			}
			h.probeErr = c.probeErr
			st := state.ForConfig(t.TempDir())
			if err := os.MkdirAll(st.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			err := prePinExperiments(context.Background(), experimentsPin{admin: &hubapi.Client{T: h}, st: st, known: knowsExperiments(c.has),
				now: now, conduitOn: c.conduitWanted, log: func(string, ...any) {}, warn: func(string, ...any) {}})
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v", err)
			}
			if (h.putCount() > 0) != c.wantPuts {
				t.Fatalf("puts = %v", h.puts)
			}
			rec, found, _ := st.LoadHubExperimentsRecord()
			if found != c.wantSave {
				t.Fatalf("record found = %v", found)
			}
			for _, n := range h.registered {
				if !c.wantPuts {
					break
				}
				if want := slices.Contains(c.wantOn, n); h.enabled(n) != want {
					t.Errorf("%s enabled = %v, want %v", n, h.enabled(n), want)
				}
			}
			if c.wantSave && !slices.Equal(rec.On, c.wantOn) {
				t.Fatalf("record on = %v, want %v", rec.On, c.wantOn)
			}
		})
	}
}

// newScionPinned are the pinned experiments scion e8160a10 registers:
// hub.chat_thread_groups is graduated there.
var newScionPinned = []string{"hub.conduit", "hub.artifacts", "federation.project_visitors", "hub.attachment_artifacts",
	"hub.port_view", "hub.conduit_deliver", "hub.agent_plugins", "hub.agent_delegation", "hub.async_agent_start"}

// newScionExperiments makes h answer as scion e8160a10: thread groups are
// no longer registered and their route is always served, and
// hub.attachment_artifacts is on by the deployment default.
func newScionExperiments(h *fakeExperimentsHub) {
	h.registered = append(slices.Clone(newScionPinned), "web.agent_edit")
	h.defaultOn = map[string]bool{"hub.attachment_artifacts": true}
	h.served = []string{"/api/v1/thread-groups"}
}

func TestEnsureHubExperimentsOnGraduatedThreadGroups(t *testing.T) {
	h := newFakeExperimentsHub()
	newScionExperiments(h)
	st := state.ForConfig(t.TempDir())
	restarts := 0
	on, err := ensureHubExperiments(context.Background(), experimentsPin{
		admin: &hubapi.Client{T: h}, st: st, known: knowsExperiments(true),
		now:        func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
		restartHub: func(context.Context) error { restarts++; return nil },
		log:        func(string, ...any) {}, warn: func(f string, a ...any) { t.Errorf("warn: "+f, a...) },
	})
	if err != nil || on {
		t.Fatalf("on=%v err=%v (%v)", on, err, h.calls)
	}
	if restarts != 0 {
		t.Fatalf("restarts = %d: no probed experiment was on", restarts)
	}
	for _, n := range newScionPinned {
		if v, ok := h.overrides[n]; !ok || v {
			t.Fatalf("%s override = %v, %v; want pinned off", n, v, ok)
		}
	}
	if _, ok := h.overrides[threadGroupsExperiment]; ok {
		t.Fatal("lever wrote an override for the graduated thread groups")
	}
	rec, found, _ := st.LoadHubExperimentsRecord()
	if !found || !slices.Equal(rec.Off, newScionPinned) || !slices.Contains(rec.Asked, threadGroupsExperiment) {
		t.Fatalf("record = %+v (found %v)", rec, found)
	}
	r := checkHubExperiments(context.Background(), st, false, knowsExperiments(true), &hubapi.Client{T: h})
	if !r.ok || r.fix != "" || !strings.Contains(r.detail, "chat thread groups are always on in this scion") {
		t.Fatalf("doctor row = %+v", r)
	}
}

// d34ffbcPinned are the pinned experiments scion d34ffbc1 registers.
var d34ffbcPinned = append(slices.Clone(newScionPinned), "hub.triggers", "hub.github_triggers", "hub.exec_schedules",
	"hub.exec_containers", "hub.system_one", "hub.code_span_mentions", "hub.agent_secrets", "lifecycle.idle_suspend", "hub.suspend_screen",
	"hub.message_policies", "web.decisions", "hub.runtime_orphan_reconcile", "hub.reincarnate_harness_switch", "hub.custom_default_user_role")

// On scion d34ffbc1 every new experiment is pinned off, hub.suspend_screen
// against its default on; only a probed one that was on restarts the hub.
func TestEnsureHubExperimentsOnD34ffbc1(t *testing.T) {
	for _, tc := range []struct {
		name         string
		on           map[string]bool
		wantRestarts int
	}{
		{"defaults", nil, 0},
		{"triggers on", map[string]bool{"hub.triggers": true}, 1},
		{"unprobed ones on", map[string]bool{"lifecycle.idle_suspend": true, "hub.agent_secrets": true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeExperimentsHub()
			newScionExperiments(h)
			h.registered = append(slices.Clone(d34ffbcPinned), "web.agent_edit")
			h.defaultOn["hub.suspend_screen"] = true
			maps.Copy(h.overrides, tc.on)
			st := state.ForConfig(t.TempDir())
			restarts := 0
			on, err := ensureHubExperiments(context.Background(), experimentsPin{
				admin: &hubapi.Client{T: h}, st: st, known: knowsExperiments(true),
				now:        func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) },
				restartHub: func(context.Context) error { restarts++; return nil },
				log:        func(string, ...any) {}, warn: func(f string, a ...any) { t.Errorf("warn: "+f, a...) },
			})
			if err != nil || on || restarts != tc.wantRestarts {
				t.Fatalf("on=%v err=%v restarts=%d (%v)", on, err, restarts, h.calls)
			}
			for _, n := range d34ffbcPinned {
				if v, ok := h.overrides[n]; !ok || v || h.enabled(n) {
					t.Fatalf("%s override = %v, %v; want pinned off", n, v, ok)
				}
			}
			if _, ok := h.overrides["web.agent_edit"]; ok {
				t.Fatal("lever pinned an experiment it does not list")
			}
			if !slices.Contains(h.calls, "GET /api/v1/hooks/00000000-0000-0000-0000-000000000000") {
				t.Fatalf("hub.triggers was not probed: %v", h.calls)
			}
			rec, found, _ := st.LoadHubExperimentsRecord()
			if !found || !slices.Equal(rec.Off, d34ffbcPinned) {
				t.Fatalf("record = %+v (found %v)", rec, found)
			}
			if r := checkHubExperiments(context.Background(), st, false, knowsExperiments(true), &hubapi.Client{T: h}); !r.ok || r.fix != "" {
				t.Fatalf("doctor row = %+v", r)
			}
		})
	}
}

// d6d6abcPinned are the pinned-off experiments scion d6d6abc7 registers.
var d6d6abcPinned = append(slices.Clone(d34ffbcPinned), "hub.command_requests", "hub.template_drift_warning",
	"hub.agent_move_bundle", "tokenomics.usage_ledger", "tokenomics.token_budgets", "tokenomics.trajectory_capture",
	"lifecycle.never_suspend", "hub.artifacts_transfer", "hub.decision_retention")

// ddb48b0Pinned are the pinned-off experiments scion ddb48b02 registers.
var ddb48b0Pinned = append(slices.Clone(d6d6abcPinned), "hub.mcp_server", "hub.large_uploads",
	"agent_policy.telemetry_enforcement", "agent_policy.limit_ceilings", "agent_policy.duration_backstop",
	"hub.artifacts_review_requests", "hub.flat_runtime_brokers")

// On scion ddb48b02 the new experiments are pinned off, against the defaults
// ddb48b02 turns on, and the access constraints lever caps remote users with
// (from d6d6abc7) are pinned on, against its default off and against an
// admin's override off; an experiment lever does not list keeps its own state.
func TestEnsureHubExperimentsOnDdb48b02(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   map[string]bool
	}{
		{"defaults", nil},
		{"constraints overridden off", map[string]bool{accessConstraintsExperiment: false}},
		{"new ones on", map[string]bool{"hub.command_requests": true, "tokenomics.usage_ledger": true, "hub.mcp_server": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeExperimentsHub()
			newScionExperiments(h)
			h.registered = append(slices.Clone(ddb48b0Pinned), accessConstraintsExperiment, "web.terminal_key_bar", "web.agent_edit")
			// ddb48b02's defaults: a probed experiment served on (conduit's
			// relay runs) costs one restart after the pin.
			for _, n := range []string{"hub.suspend_screen", "hub.conduit", "hub.artifacts", "hub.agent_plugins", "hub.flat_runtime_brokers"} {
				h.defaultOn[n] = true
			}
			maps.Copy(h.overrides, tc.on)
			st := state.ForConfig(t.TempDir())
			restarts := 0
			on, err := ensureHubExperiments(context.Background(), experimentsPin{
				admin: &hubapi.Client{T: h}, st: st, known: knowsExperiments(true),
				now:        func() time.Time { return time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC) },
				restartHub: func(context.Context) error { restarts++; return nil },
				log:        func(string, ...any) {}, warn: func(f string, a ...any) { t.Errorf("warn: "+f, a...) },
			})
			if err != nil || on || restarts != 1 {
				t.Fatalf("on=%v err=%v restarts=%d (%v)", on, err, restarts, h.calls)
			}
			for _, n := range ddb48b0Pinned {
				if v, ok := h.overrides[n]; !ok || v || h.enabled(n) {
					t.Fatalf("%s override = %v, %v; want pinned off", n, v, ok)
				}
			}
			if v, ok := h.overrides[accessConstraintsExperiment]; !ok || !v || !h.enabled(accessConstraintsExperiment) {
				t.Fatalf("access constraints override = %v, %v; want pinned on", v, ok)
			}
			for _, n := range []string{"web.terminal_key_bar", "web.agent_edit"} {
				if _, ok := h.overrides[n]; ok {
					t.Fatalf("lever pinned %s, which it does not list", n)
				}
			}
			rec, found, _ := st.LoadHubExperimentsRecord()
			if !found || !slices.Equal(rec.Off, ddb48b0Pinned) || !slices.Equal(rec.On, []string{accessConstraintsExperiment}) ||
				!slices.Contains(rec.Asked, accessConstraintsExperiment) {
				t.Fatalf("record = %+v (found %v)", rec, found)
			}
			if r := checkHubExperiments(context.Background(), st, false, knowsExperiments(true), &hubapi.Client{T: h}); !r.ok || r.fix != "" {
				t.Fatalf("doctor row = %+v", r)
			}
		})
	}
}

// A scion before d6d6abc7 does not register access constraints and serves
// their routes ungated: lever writes no override for it.
func TestPinExperimentsSkipsUnregisteredPinnedOn(t *testing.T) {
	h := newFakeExperimentsHub()
	newScionExperiments(h)
	h.registered = slices.Clone(d34ffbcPinned)
	rec, err := pinExperimentsOff(context.Background(), &hubapi.Client{T: h}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.overrides[accessConstraintsExperiment]; ok || len(rec.On) != 0 {
		t.Fatalf("override written for an unregistered experiment: %v, record on %v", h.overrides, rec.On)
	}
}
