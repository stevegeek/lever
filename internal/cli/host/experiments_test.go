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
	err := ensureHubExperimentsOff(context.Background(), experimentsPin{admin: &hubapi.Client{T: h}, st: st,
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
			err := ensureHubExperimentsOff(context.Background(), p)
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
