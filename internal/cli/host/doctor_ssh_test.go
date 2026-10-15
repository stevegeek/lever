package host

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/apply"
	"github.com/stevegeek/lever/internal/conduit"
	"github.com/stevegeek/lever/internal/hubapi"
	scionpkg "github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

func TestCheckHubExperimentsConduitOnForSSH(t *testing.T) {
	st := state.ForConfig(t.TempDir())
	rec := state.HubExperimentsRecord{Off: []string{"hub.artifacts", "hub.chat_thread_groups", "federation.project_visitors"},
		On: []string{"hub.conduit"}, Asked: pinnedOffNames(), Revision: 2, PinnedAt: time.Now()}
	if err := st.SaveHubExperimentsRecord(rec); err != nil {
		t.Fatal(err)
	}
	h := newFakeExperimentsHub()
	h.overrides["hub.conduit"] = true
	r := checkHubExperiments(context.Background(), st, true, knowsExperiments(true), &hubapi.Client{T: h})
	if !r.ok || !strings.Contains(r.detail, "on for `lever ssh`") {
		t.Fatalf("ssh on, conduit recorded on: %+v", r)
	}
	r = checkHubExperiments(context.Background(), st, false, knowsExperiments(true), &hubapi.Client{T: h})
	if r.ok || !strings.Contains(r.detail, "hub.conduit on at the hub") {
		t.Fatalf("ssh off, conduit served: %+v", r)
	}
	h.overrides["hub.artifacts"] = true
	r = checkHubExperiments(context.Background(), st, true, knowsExperiments(true), &hubapi.Client{T: h})
	if r.ok || !strings.Contains(r.detail, "hub.artifacts on") {
		t.Fatalf("artifacts served with ssh on: %+v", r)
	}
}

func TestCheckSSH(t *testing.T) {
	app, st, _ := sshTestApp(t)
	ctx := context.Background()
	live := conduit.LiveHub{Running: true, Env: apply.HubEnv("i", "fp", app, true)}
	served := newFakeExperimentsHub()
	served.overrides["hub.conduit"] = true
	want := apply.ConduitPolicy(app).AgentValue()
	agents := func(context.Context, string) ([]scionpkg.Agent, error) {
		return []scionpkg.Agent{{Slug: "inst", ContainerID: "c-m"}, {Slug: "w1", ContainerID: "c-w"}}, nil
	}
	env := func(stale bool) envReader {
		return func(_ context.Context, ref, key string) (string, bool, error) {
			if key != conduit.AgentPolicyEnv {
				t.Fatalf("read %s", key)
			}
			if ref == "c-w" && stale {
				return "", false, nil
			}
			return want, true, nil
		}
	}
	d := func(sc fakeConduitScion, hub *fakeExperimentsHub, stale bool) sshDoctor {
		return sshDoctor{sc: sc, hc: &hubapi.Client{T: hub}, list: agents, env: env(stale), project: "/p", agents: []string{"inst", "w1"}}
	}
	saveOn := func(on bool) {
		rec := state.HubExperimentsRecord{}
		if on {
			rec.On = []string{"hub.conduit"}
		}
		if err := st.SaveHubExperimentsRecord(rec); err != nil {
			t.Fatal(err)
		}
	}

	app.SSH.Enabled = false
	if r := checkSSH(ctx, app, st, d(fakeConduitScion{}, served, false)); r.name != "" {
		t.Fatalf("ssh off adds a row: %+v", r)
	}
	app.SSH.Enabled = true
	if r := checkSSH(ctx, app, st, d(fakeConduitScion{controls: false}, served, false)); !r.ok || r.fix == "" || !strings.Contains(r.detail, "conduit controls") {
		t.Fatalf("no controls: %+v", r)
	}
	saveOn(false)
	if r := checkSSH(ctx, app, st, d(fakeConduitScion{controls: true, hub: live}, served, false)); r.fix == "" || !strings.Contains(r.detail, "not turned conduit on") {
		t.Fatalf("recorded off: %+v", r)
	}
	saveOn(true)
	if r := checkSSH(ctx, app, st, d(fakeConduitScion{controls: true, hub: conduit.LiveHub{Running: true}}, served, false)); r.ok {
		t.Fatalf("hub without the policy: %+v", r)
	}
	if r := checkSSH(ctx, app, st, d(fakeConduitScion{controls: true, hub: live}, newFakeExperimentsHub(), false)); r.ok {
		t.Fatalf("conduit not served: %+v", r)
	}
	if r := checkSSH(ctx, app, st, d(fakeConduitScion{controls: true, hub: live}, served, true)); !r.ok || r.fix == "" || !strings.Contains(r.detail, "under another policy: w1") {
		t.Fatalf("stale worker: %+v", r)
	}
	r := checkSSH(ctx, app, st, d(fakeConduitScion{controls: true, hub: live}, served, false))
	if !r.ok || r.fix != "" || !strings.Contains(r.detail, "denies ports 8462") || !strings.Contains(r.detail, "reachable: inst, w1") {
		t.Fatalf("all good: %+v", r)
	}
}
