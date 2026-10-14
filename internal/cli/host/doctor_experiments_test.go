package host

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/state"
)

func TestCheckHubExperiments(t *testing.T) {
	pinned := &state.HubExperimentsRecord{Off: pinnedOffNames(), Asked: pinnedOffNames(), Revision: 2, PinnedAt: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	for _, tc := range []struct {
		name    string
		known   scopeKnownFunc
		rec     *state.HubExperimentsRecord
		hub     func(h *fakeExperimentsHub)
		wantOK  bool
		warn    bool
		wantSub string
	}{
		{name: "pinned and off", known: knowsExperiments(true), rec: pinned, wantOK: true, wantSub: "pinned off by lever on 2026-10-13T12:00:00Z"},
		{name: "federation is read from the record", known: knowsExperiments(true), rec: pinned, wantOK: true,
			wantSub: "(hub.conduit, hub.artifacts, hub.chat_thread_groups); federation.project_visitors pinned off on the same apply, which no route can show"},
		{name: "conduit live", known: knowsExperiments(true), rec: pinned,
			hub: func(h *fakeExperimentsHub) { h.overrides["hub.conduit"] = true }, wantSub: "hub.conduit on at the hub"},
		{name: "artifacts live, never pinned", known: knowsExperiments(true),
			hub: func(h *fakeExperimentsHub) { h.overrides["hub.artifacts"] = true }, wantSub: "hub.artifacts on at the hub"},
		{name: "live although the jail's scion says it has none", known: knowsExperiments(false),
			hub: func(h *fakeExperimentsHub) { h.overrides["hub.conduit"] = true }, wantSub: "hub.conduit on"},
		{name: "never pinned", known: knowsExperiments(true), wantOK: true, warn: true, wantSub: "off by scion's default only"},
		{name: "recorded not applicable, scion now has them", known: knowsExperiments(true),
			rec: &state.HubExperimentsRecord{NotApplicable: true}, wantOK: true, warn: true, wantSub: "not pinned them off"},
		{name: "older scion", known: knowsExperiments(false), wantOK: true, wantSub: "has no hub experiments"},
		{name: "older scion, recorded not applicable", known: knowsExperiments(false),
			rec: &state.HubExperimentsRecord{NotApplicable: true}, wantOK: true, wantSub: "has no hub experiments"},
		{name: "hub registers none", known: knowsExperiments(true), rec: &state.HubExperimentsRecord{Asked: pinnedOffNames(), Revision: 1},
			wantOK: true, wantSub: "registers none"},
		{name: "an older lever's pin", known: knowsExperiments(true),
			rec:    &state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts"}, Revision: 2},
			wantOK: true, warn: true, wantSub: "never looked for hub.chat_thread_groups, federation.project_visitors"},
		{name: "older scion registers only two", known: knowsExperiments(true),
			rec:    &state.HubExperimentsRecord{Off: []string{"hub.conduit", "hub.artifacts"}, Asked: pinnedOffNames(), Revision: 2},
			wantOK: true, wantSub: "pinned off by lever"},
		{name: "thread groups live", known: knowsExperiments(true), rec: pinned,
			hub: func(h *fakeExperimentsHub) { h.overrides["hub.chat_thread_groups"] = true }, wantSub: "hub.chat_thread_groups on at the hub"},
		{name: "hub down", known: knowsExperiments(true), rec: pinned,
			hub: func(h *fakeExperimentsHub) { h.probeErr = errors.New("connection refused") }, wantOK: true, wantSub: "not checked"},
		{name: "controller PAT refused", known: knowsExperiments(true), rec: pinned,
			hub: func(h *fakeExperimentsHub) { h.probeStatus = 401 }, wantSub: "could not probe"},
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
			r := checkHubExperiments(context.Background(), st, tc.known, &hubapi.Client{T: h})
			if r.name != "scion conduit and artifacts" {
				t.Fatalf("row name = %q", r.name)
			}
			if r.ok != tc.wantOK || !strings.Contains(r.detail, tc.wantSub) {
				t.Fatalf("row = %+v, want ok=%v detail~%q", r, tc.wantOK, tc.wantSub)
			}
			if isWarn := r.ok && r.fix != ""; isWarn != tc.warn {
				t.Fatalf("warning = %v, want %v: %+v", isWarn, tc.warn, r)
			}
			if !r.ok && r.fix == "" {
				t.Fatalf("a failed row needs a fix: %+v", r)
			}
			if strings.Contains(r.detail, " on at the hub") && !strings.Contains(r.fix, "`lever apply`") {
				t.Fatalf("live fix = %q, want the apply repair", r.fix)
			}
		})
	}
}

func TestCheckHubExperimentsCorruptRecord(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.HubExperiments(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := checkHubExperiments(context.Background(), st, knowsExperiments(true), &hubapi.Client{T: newFakeExperimentsHub()})
	if r.ok || !strings.Contains(r.fix, "hub-experiments.json") {
		t.Fatalf("row = %+v, want a failure naming the record", r)
	}
}

type runtimeHub struct {
	status int
	body   string
	err    error
}

func (h runtimeHub) Do(_ context.Context, method, path string) (int, []byte, error) {
	if method != "GET" || path != "/api/v1/system/runtime" {
		return 404, nil, nil
	}
	return h.status, []byte(h.body), h.err
}

func TestCheckHubRuntime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		hub     runtimeHub
		wantOK  bool
		wantSub string
	}{
		{"podman", runtimeHub{status: 200, body: `{"detected":"podman","configured":"podman","available":true}`}, true, "podman"},
		{"switched by an agent", runtimeHub{status: 200, body: `{"configured":"docker"}`}, false, `"docker", not podman`},
		{"unset", runtimeHub{status: 200, body: `{"configured":""}`}, false, `"", not podman`},
		{"refused", runtimeHub{status: 401, body: `{}`}, false, "could not read it"},
		{"hub down", runtimeHub{err: errors.New("connection refused")}, true, "not checked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := checkHubRuntime(context.Background(), &hubapi.Client{T: tc.hub})
			if r.name != "hub container runtime" || r.ok != tc.wantOK || !strings.Contains(r.detail, tc.wantSub) {
				t.Fatalf("row = %+v, want ok=%v detail~%q", r, tc.wantOK, tc.wantSub)
			}
			if !r.ok && r.fix == "" {
				t.Fatalf("a failed row needs a fix: %+v", r)
			}
		})
	}
}
