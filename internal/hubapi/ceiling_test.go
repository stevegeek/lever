package hubapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const leverSettings = "/api/v1/projects/" + leverUUID + "/settings"

// The settings route is a whole-object PUT: every field absent from the body
// is deleted from the project. So the write must carry back everything the
// read returned, changed only in the two role fields.
func TestEnsureAgentRoleCeilingRoundTripsTheOtherSettings(t *testing.T) {
	f := &fakeDoer{
		replies: map[string]reply{
			"GET /api/v1/projects": {status: 200, body: projectsBody},
			"PUT " + leverSettings: {status: 200, body: `{}`},
		},
		seq: map[string][]reply{
			"GET " + leverSettings: {
				{status: 200, body: `{"defaultTemplate":"lever","defaultMaxTurns":40,"maxAgentRole":"","defaultAgentRole":"full"}`},
				{status: 200, body: `{"defaultTemplate":"lever","defaultMaxTurns":40,"maxAgentRole":"baseline","defaultAgentRole":"baseline"}`},
			},
		},
	}
	changed, err := (&Client{T: f}).EnsureAgentRoleCeiling(context.Background(), "lever", "hub", "baseline")
	if err != nil {
		t.Fatalf("EnsureAgentRoleCeiling: %v", err)
	}
	if !changed {
		t.Fatal("want changed=true when the ceiling was written")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies["PUT "+leverSettings]), &sent); err != nil {
		t.Fatalf("PUT body is not JSON: %v: %q", err, f.bodies["PUT "+leverSettings])
	}
	if sent["defaultTemplate"] != "lever" || sent["defaultMaxTurns"] != float64(40) {
		t.Errorf("PUT must carry the other settings back unchanged, sent %v", sent)
	}
	if sent["maxAgentRole"] != "baseline" || sent["defaultAgentRole"] != "baseline" {
		t.Errorf("PUT must set both role fields, sent %v", sent)
	}
}

func TestEnsureAgentRoleCeilingIsIdempotent(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/projects": {status: 200, body: projectsBody},
		"GET " + leverSettings: {status: 200, body: `{"maxAgentRole":"baseline","defaultAgentRole":"baseline"}`},
	}}
	changed, err := (&Client{T: f}).EnsureAgentRoleCeiling(context.Background(), "lever", "hub", "baseline")
	if err != nil || changed {
		t.Fatalf("already set: changed=%v err=%v", changed, err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "PUT") {
			t.Fatalf("no PUT when nothing changes, got %v", f.calls)
		}
	}
}

// The verify read is the point, as for the shared-dir strip: a hub that
// answered 200 and stored nothing must not read as done.
func TestEnsureAgentRoleCeilingFailsWhenTheHubDidNotKeepIt(t *testing.T) {
	f := &fakeDoer{
		replies: map[string]reply{
			"GET /api/v1/projects": {status: 200, body: projectsBody},
			"GET " + leverSettings: {status: 200, body: `{"maxAgentRole":"","defaultAgentRole":""}`},
			"PUT " + leverSettings: {status: 200, body: `{}`},
		},
	}
	_, err := (&Client{T: f}).EnsureAgentRoleCeiling(context.Background(), "lever", "hub", "baseline")
	if err == nil {
		t.Fatal("a ceiling the hub did not keep must be an error")
	}
	assertAPIStatus(t, err, 200)
}

func TestEnsureAgentRoleCeilingSurfacesForbidden(t *testing.T) {
	f := &fakeDoer{
		replies: map[string]reply{
			"GET /api/v1/projects": {status: 200, body: projectsBody},
			"GET " + leverSettings: {status: 200, body: `{}`},
			"PUT " + leverSettings: {status: 403, body: `{"error":{"code":"forbidden"}}`},
		},
	}
	_, err := (&Client{T: f}).EnsureAgentRoleCeiling(context.Background(), "lever", "hub", "baseline")
	assertAPIStatus(t, err, 403)
}

// A Doer that cannot carry a body (a transport that predates the settings
// write) must fail loud rather than send an empty PUT — an empty body would
// wipe every project setting.
type bodilessDoer struct{ f *fakeDoer }

func (b bodilessDoer) Do(ctx context.Context, m, p string) (int, []byte, error) {
	return b.f.Do(ctx, m, p)
}

func TestEnsureAgentRoleCeilingNeedsABodyTransport(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/projects": {status: 200, body: projectsBody},
		"GET " + leverSettings: {status: 200, body: `{}`},
	}}
	_, err := (&Client{T: bodilessDoer{f}}).EnsureAgentRoleCeiling(context.Background(), "lever", "hub", "baseline")
	if err == nil {
		t.Fatal("a transport without a body path must be refused")
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "PUT") {
			t.Fatalf("no PUT may be attempted without a body, got %v", f.calls)
		}
	}
}

func TestAgentRoleCeilingReadsBothFields(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/projects": {status: 200, body: projectsBody},
		"GET " + leverSettings: {status: 200, body: `{"maxAgentRole":"baseline","defaultAgentRole":"readonly"}`},
	}}
	c, err := (&Client{T: f}).AgentRoleCeiling(context.Background(), "lever", "hub")
	if err != nil {
		t.Fatal(err)
	}
	if c.Max != "baseline" || c.Default != "readonly" {
		t.Errorf("got %+v", c)
	}
}
