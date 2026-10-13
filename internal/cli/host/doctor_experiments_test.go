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
	pinned := &state.HubExperimentsRecord{Off: pinnedOffNames(), Revision: 2, PinnedAt: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
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
		{name: "hub registers neither", known: knowsExperiments(true), rec: &state.HubExperimentsRecord{Revision: 1},
			wantOK: true, wantSub: "registers neither"},
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
			if strings.Contains(r.detail, " on at the hub") && !strings.Contains(r.fix, "`lever stop`, then `lever up`") {
				t.Fatalf("live fix = %q, want the stop/up repair", r.fix)
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
