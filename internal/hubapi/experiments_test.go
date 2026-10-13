package hubapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// adminExperimentsBody is the shape scion b41cd6fe answers GET/PUT
// /api/v1/admin/experiments with, trimmed to the fields lever reads plus one
// it ignores.
const adminExperimentsBody = `{"revision":3,"malformed":false,"experiments":[
  {"name":"hub.artifacts","title":"Artifacts","override":false,"enabled":false},
  {"name":"hub.conduit","override":null,"enabled":true},
  {"name":"web.agent_edit","override":true,"enabled":true}],
  "unknown_overrides":{},"updated_at":null,"updated_by":null}`

func TestAdminExperimentsDecodes(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"GET /api/v1/admin/experiments": {status: 200, body: adminExperimentsBody}}}
	got, err := (&Client{T: f}).AdminExperiments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 3 || got.Malformed || len(got.Items) != 3 {
		t.Fatalf("got %+v", got)
	}
	a, ok := got.Lookup("hub.artifacts")
	if !ok || a.Override == nil || *a.Override || a.Enabled {
		t.Fatalf("hub.artifacts = %+v, %v; want override false, disabled", a, ok)
	}
	c, ok := got.Lookup("hub.conduit")
	if !ok || c.Override != nil || !c.Enabled {
		t.Fatalf("hub.conduit = %+v, %v; want no override, enabled", c, ok)
	}
	if _, ok := got.Lookup("hub.nope"); ok {
		t.Fatal("Lookup found an unregistered name")
	}
}

func TestAdminExperimentsErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		r      reply
		is     error
		status int
	}{
		{"older scion has no route", reply{status: 404, body: "404 page not found"}, ErrNoExperiments, 0},
		{"refused", reply{status: 403, body: `{"error":{"code":"forbidden"}}`}, nil, 403},
		{"no revision", reply{status: 200, body: `{"malformed":false,"experiments":[]}`}, nil, 0},
		{"no malformed flag", reply{status: 200, body: `{"revision":1,"experiments":[]}`}, nil, 0},
		{"no experiments list", reply{status: 200, body: `{"revision":1,"malformed":false}`}, nil, 0},
		{"not json", reply{status: 200, body: `<html>`}, nil, 0},
	} {
		f := &fakeDoer{replies: map[string]reply{"GET /api/v1/admin/experiments": tc.r}}
		_, err := (&Client{T: f}).AdminExperiments(context.Background())
		if err == nil {
			t.Errorf("%s: want an error", tc.name)
			continue
		}
		if tc.is != nil && !errors.Is(err, tc.is) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.is)
		}
		var ae *APIError
		if tc.is == nil && !errors.As(err, &ae) {
			t.Errorf("%s: err = %v, want an APIError", tc.name, err)
		}
		if tc.status != 0 && (ae == nil || ae.Status != tc.status) {
			t.Errorf("%s: err = %v, want status %d", tc.name, err, tc.status)
		}
	}
}

// The PUT body is scion's adminExperimentsPutRequest: only the names sent, and
// the revision the caller read.
func TestSetExperimentOverridesBody(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"PUT /api/v1/admin/experiments": {status: 200, body: adminExperimentsBody}}}
	got, err := (&Client{T: f}).SetExperimentOverrides(context.Background(), map[string]bool{"hub.conduit": false}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 3 {
		t.Fatalf("revision = %d, want the response's 3", got.Revision)
	}
	var body struct {
		Overrides        map[string]*bool `json:"overrides"`
		ExpectedRevision *int64           `json:"expected_revision"`
	}
	if err := json.Unmarshal([]byte(f.bodies["PUT /api/v1/admin/experiments"]), &body); err != nil {
		t.Fatal(err)
	}
	if body.ExpectedRevision == nil || *body.ExpectedRevision != 2 {
		t.Fatalf("expected_revision = %v, want 2", body.ExpectedRevision)
	}
	if len(body.Overrides) != 1 || body.Overrides["hub.conduit"] == nil || *body.Overrides["hub.conduit"] {
		t.Fatalf("overrides = %v, want only hub.conduit=false", body.Overrides)
	}
}

func TestSetExperimentOverridesErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    reply
		is   error
	}{
		{"revision conflict", reply{status: 409, body: `{"error":{"code":"revision_conflict","message":"modified concurrently"}}`}, ErrExperimentsConflict},
		{"older scion has no route", reply{status: 404, body: "404 page not found"}, ErrNoExperiments},
		{"malformed store", reply{status: 409, body: `{"error":{"code":"experiments_malformed"}}`}, nil},
		{"unregistered name", reply{status: 400, body: `{"error":{"code":"validation_failed"}}`}, nil},
	} {
		f := &fakeDoer{replies: map[string]reply{"PUT /api/v1/admin/experiments": tc.r}}
		_, err := (&Client{T: f}).SetExperimentOverrides(context.Background(), map[string]bool{"hub.conduit": false}, 1)
		if err == nil {
			t.Errorf("%s: want an error", tc.name)
			continue
		}
		if tc.is != nil && !errors.Is(err, tc.is) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.is)
		}
		if tc.is == nil && (errors.Is(err, ErrExperimentsConflict) || errors.Is(err, ErrNoExperiments)) {
			t.Errorf("%s: err = %v, want a plain APIError", tc.name, err)
		}
	}
	if _, err := (&Client{T: &fakeDoer{}}).SetExperimentOverrides(context.Background(), nil, 0); err == nil {
		t.Fatal("an empty override set must not be sent (scion refuses it)")
	}
}

func TestRouteServed(t *testing.T) {
	transport := errors.New("connection refused")
	for _, tc := range []struct {
		name    string
		r       reply
		served  bool
		wantErr bool
		api     bool
	}{
		{"off or absent", reply{status: 404, body: `{"error":{"code":"not_found","message":"route not found"}}`}, false, false, false},
		{"older scion", reply{status: 404, body: "404 page not found"}, false, false, false},
		{"on", reply{status: 200, body: `{"keys":[]}`}, true, false, false},
		{"on, bad query", reply{status: 400, body: `{}`}, true, false, false},
		{"on, refused", reply{status: 403, body: `{}`}, true, false, false},
		{"on, keys unavailable", reply{status: 503, body: `{}`}, true, false, false},
		{"credential rejected", reply{status: 401, body: `{}`}, false, true, true},
		{"hub down", reply{err: transport}, false, true, false},
	} {
		f := &fakeDoer{replies: map[string]reply{"GET /p": tc.r}}
		served, err := (&Client{T: f}).RouteServed(context.Background(), "/p")
		if served != tc.served || (err != nil) != tc.wantErr {
			t.Errorf("%s: served=%v err=%v, want served=%v err=%v", tc.name, served, err, tc.served, tc.wantErr)
		}
		var ae *APIError
		if tc.wantErr && errors.As(err, &ae) != tc.api {
			t.Errorf("%s: err = %v, APIError want %v", tc.name, err, tc.api)
		}
	}
}
