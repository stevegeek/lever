package hubapi

import (
	"context"
	"reflect"
	"testing"
)

func TestRegisterProject(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"POST /api/v1/projects/register": {status: 201, body: `{"project":{
	  "id":"p-1","name":"lever","slug":"lever","ownerId":"u-c","createdBy":"u-c","defaultRuntimeBrokerId":"b-1"},"created":true}`}}}
	p, created, err := (&Client{T: f}).RegisterProject(context.Background(), "p-1", "lever", "/lever")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	want := Project{ID: "p-1", Name: "lever", Slug: "lever", OwnerID: "u-c", CreatedBy: "u-c", DefaultRuntimeBrokerID: "b-1"}
	if p != want {
		t.Fatalf("got %+v, want %+v", p, want)
	}
	if got := decodeBody(t, f, "POST /api/v1/projects/register"); !reflect.DeepEqual(got, map[string]any{"id": "p-1", "name": "lever", "path": "/lever"}) {
		t.Fatalf("body = %v", got)
	}
}

func TestRegisterProjectExisting(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"POST /api/v1/projects/register": {status: 200, body: `{"project":{"id":"p-1","name":"lever"},"created":false}`}}}
	_, created, err := (&Client{T: f}).RegisterProject(context.Background(), "p-1", "lever", "")
	if err != nil || created {
		t.Fatalf("created=%v err=%v", created, err)
	}
}

// The hub also matches an existing project by slug; lever must not go on to
// bind or mint against a project it did not name.
func TestRegisterProjectRefusesAnotherProject(t *testing.T) {
	cases := map[string]reply{
		"other id":     {status: 200, body: `{"project":{"id":"p-other","name":"lever"},"created":false}`},
		"no project":   {status: 200, body: `{"created":true}`},
		"no created":   {status: 200, body: `{"project":{"id":"p-1"}}`},
		"refused":      {status: 403, body: `{"error":"forbidden"}`},
		"undecodable":  {status: 200, body: `not json`},
		"null project": {status: 200, body: `{"project":null,"created":true}`},
	}
	for name, r := range cases {
		f := &fakeDoer{replies: map[string]reply{"POST /api/v1/projects/register": r}}
		if _, _, err := (&Client{T: f}).RegisterProject(context.Background(), "p-1", "lever", ""); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, _, err := (&Client{T: &fakeDoer{}}).RegisterProject(context.Background(), "", "lever", ""); err == nil {
		t.Error("an empty id must fail before any request")
	}
}

func TestGetProject(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		"GET /api/v1/projects/p-1":  {status: 200, body: `{"id":"p-1","name":"lever","slug":"lever","ownerId":"u","_capabilities":{}}`},
		"GET /api/v1/projects/gone": {status: 404, body: `{}`},
		"GET /api/v1/projects/bad":  {status: 403, body: `{}`},
	}}
	c := &Client{T: f}
	p, found, err := c.GetProject(context.Background(), "p-1")
	if err != nil || !found || p.OwnerID != "u" {
		t.Fatalf("got %+v found=%v err=%v", p, found, err)
	}
	if _, found, err := c.GetProject(context.Background(), "gone"); err != nil || found {
		t.Fatalf("404: found=%v err=%v", found, err)
	}
	_, _, err = c.GetProject(context.Background(), "bad")
	assertAPIStatus(t, err, 403)
}

func TestRuntimeBrokers(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"GET /api/v1/runtime-brokers": {status: 200,
		body: `{"brokers":[{"id":"b-1","name":"lever","status":"online","connectionState":"connected"}],"totalCount":1}`}}}
	got, err := (&Client{T: f}).RuntimeBrokers(context.Background())
	if err != nil || !reflect.DeepEqual(got, []RuntimeBroker{{ID: "b-1", Name: "lever", Status: "online"}}) {
		t.Fatalf("got %+v err=%v", got, err)
	}
	f.replies["GET /api/v1/runtime-brokers"] = reply{status: 200, body: `[]`}
	if _, err := (&Client{T: f}).RuntimeBrokers(context.Background()); err == nil {
		t.Fatal("a list without brokers must fail")
	}
}

func TestProjectProviders(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{"GET /api/v1/projects/p-1/providers": {status: 200,
		body: `{"providers":[{"projectId":"p-1","brokerId":"b-1","brokerName":"lever","status":"online","agentLimit":0}]}`}}}
	got, err := (&Client{T: f}).ProjectProviders(context.Background(), "p-1")
	if err != nil || !reflect.DeepEqual(got, []RuntimeBroker{{ID: "b-1", Name: "lever", Status: "online"}}) {
		t.Fatalf("got %+v err=%v", got, err)
	}
	f.replies["GET /api/v1/projects/p-1/providers"] = reply{status: 200, body: `{"items":[]}`}
	if _, err := (&Client{T: f}).ProjectProviders(context.Background(), "p-1"); err == nil {
		t.Fatal("a list without providers must fail")
	}
}
