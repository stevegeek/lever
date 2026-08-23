package hubapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const agentsPath = "GET /api/v1/agents?limit=500&projectId=" + leverUUID

// agentsScript answers the project lookup plus one page of agent records.
func agentsScript(body string) *fakeDoer {
	return &fakeDoer{
		replies: map[string]reply{
			"GET /api/v1/projects": {status: 200, body: projectsBody},
			agentsPath:             {status: 200, body: body},
		},
	}
}

func TestAgentsReadsTheStoredRole(t *testing.T) {
	f := agentsScript(`{"agents":[
	  {"slug":"assistant","name":"assistant","appliedConfig":{"agentRole":"baseline"}},
	  {"slug":"scratch","name":"scratch","appliedConfig":{"image":"x"}},
	  {"slug":"legacy","name":"legacy"}
	],"totalCount":3}`)
	got, err := (&Client{T: f}).Agents(context.Background(), "lever", "hub")
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d agents, want 3", len(got))
	}
	want := map[string]string{"assistant": "baseline", "scratch": "", "legacy": ""}
	for _, a := range got {
		if w, ok := want[a.Slug]; !ok {
			t.Errorf("unexpected agent %q", a.Slug)
		} else if a.Role != w {
			t.Errorf("agent %q role = %q, want %q", a.Slug, a.Role, w)
		}
	}
}

// A hub that renamed the list field must not read as "this project has no
// agents" — that would let the pre-role guard pass vacuously, which is the one
// failure it exists to catch. Same rationale as SharedDirs' required key.
func TestAgentsRequiresTheAgentsField(t *testing.T) {
	f := agentsScript(`{"items":[],"totalCount":0}`)
	_, err := (&Client{T: f}).Agents(context.Background(), "lever", "hub")
	if err == nil {
		t.Fatal("expected an error when the response has no \"agents\" field")
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want an APIError (the hub answered), got %T: %v", err, err)
	}
}

func TestAgentsAcceptsAnEmptyList(t *testing.T) {
	f := agentsScript(`{"agents":[],"totalCount":0}`)
	got, err := (&Client{T: f}).Agents(context.Background(), "lever", "hub")
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d agents, want 0", len(got))
	}
}

func TestAgentsFollowsTheCursor(t *testing.T) {
	f := agentsScript(`{"agents":[{"slug":"a"}],"nextCursor":"c2","totalCount":2}`)
	f.replies[agentsPath+"&cursor=c2"] = reply{status: 200,
		body: `{"agents":[{"slug":"b","appliedConfig":{"agentRole":"full"}}],"totalCount":2}`}
	got, err := (&Client{T: f}).Agents(context.Background(), "lever", "hub")
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(got) != 2 || got[0].Slug != "a" || got[1].Slug != "b" || got[1].Role != "full" {
		t.Fatalf("cursor page was not merged: %+v", got)
	}
}

// A hub that never stops handing back a cursor must fail loudly. Returning the
// pages collected so far would be a silent truncation, and the caller treats a
// missing agent as "nothing to check".
func TestAgentsFailsRatherThanTruncate(t *testing.T) {
	f := agentsScript(`{"agents":[{"slug":"a"}],"nextCursor":"loop","totalCount":1}`)
	f.replies[agentsPath+"&cursor=loop"] = reply{status: 200,
		body: `{"agents":[{"slug":"a"}],"nextCursor":"loop","totalCount":1}`}
	_, err := (&Client{T: f}).Agents(context.Background(), "lever", "hub")
	if err == nil {
		t.Fatal("expected an error when the hub keeps paging forever")
	}
	if !strings.Contains(err.Error(), "page") {
		t.Errorf("error should say paging gave up, got %v", err)
	}
}

func TestAgentsSurfacesForbidden(t *testing.T) {
	f := agentsScript("")
	f.replies[agentsPath] = reply{status: 403, body: `{"error":"forbidden"}`}
	_, err := (&Client{T: f}).Agents(context.Background(), "lever", "hub")
	assertAPIStatus(t, err, 403)
}

func TestAgentsSurfacesTransportFailure(t *testing.T) {
	f := agentsScript("")
	f.replies[agentsPath] = reply{err: fmt.Errorf("jail is down")}
	_, err := (&Client{T: f}).Agents(context.Background(), "lever", "hub")
	if err == nil {
		t.Fatal("expected an error")
	}
	var ae *APIError
	if errors.As(err, &ae) {
		t.Fatalf("a transport failure must not be an APIError, got %v", err)
	}
}

func TestAgentsSurfacesUnknownProject(t *testing.T) {
	_, err := (&Client{T: agentsScript(`{"agents":[]}`)}).
		Agents(context.Background(), "absent", "http://127.0.0.1:8080")
	if err == nil {
		t.Fatal("expected an error for a project the hub does not list")
	}
	if !strings.Contains(err.Error(), "http://127.0.0.1:8080") {
		t.Errorf("error should name the hub endpoint, got %v", err)
	}
}

func yesRoles(context.Context) (bool, error) { return true, nil }
func noRoles(context.Context) (bool, error)  { return false, nil }

func TestVerifyAgentRoleRefusesAnUnrolledRecord(t *testing.T) {
	c := &Client{T: agentsScript(`{"agents":[{"slug":"assistant"}]}`)}
	err := VerifyAgentRole(context.Background(), yesRoles, c, "lever", "assistant")
	if err == nil {
		t.Fatal("an unrolled record on a roles-aware scion must be refused")
	}
	for _, want := range []string{"assistant", "FULL", "baseline", "LOST"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q, got:\n%v", want, err)
		}
	}
}

func TestVerifyAgentRoleAllowsAStoredRole(t *testing.T) {
	c := &Client{T: agentsScript(`{"agents":[{"slug":"assistant","appliedConfig":{"agentRole":"baseline"}}]}`)}
	if err := VerifyAgentRole(context.Background(), yesRoles, c, "lever", "assistant"); err != nil {
		t.Fatalf("a record that stores a role must pass: %v", err)
	}
}

// On a pre-roles scion there is nothing to widen, and the hub is not even read.
func TestVerifyAgentRoleSkipsPreRolesScion(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{}}
	if err := VerifyAgentRole(context.Background(), noRoles, &Client{T: f}, "lever", "assistant"); err != nil {
		t.Fatalf("pre-#1089 scion must pass: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("the hub must not be read when roles do not exist, got %v", f.calls)
	}
}

// An absent record is the create path's business, not this guard's.
func TestVerifyAgentRoleAllowsAnAbsentRecord(t *testing.T) {
	c := &Client{T: agentsScript(`{"agents":[]}`)}
	if err := VerifyAgentRole(context.Background(), yesRoles, c, "lever", "assistant"); err != nil {
		t.Fatalf("no record => nothing to keep: %v", err)
	}
}

func TestVerifyAgentRoleFailsClosedOnAnUnreadableRecord(t *testing.T) {
	f := agentsScript("")
	f.replies[agentsPath] = reply{err: fmt.Errorf("jail is down")}
	if err := VerifyAgentRole(context.Background(), yesRoles, &Client{T: f}, "lever", "assistant"); err == nil {
		t.Fatal("an unreadable record must fail closed, not pass")
	}
}

func TestVerifyAgentRoleFailsClosedOnAnUnanswerableProbe(t *testing.T) {
	probeErr := func(context.Context) (bool, error) { return false, fmt.Errorf("scion missing") }
	c := &Client{T: agentsScript(`{"agents":[]}`)}
	if err := VerifyAgentRole(context.Background(), probeErr, c, "lever", "assistant"); err == nil {
		t.Fatal("an unanswerable capability probe must fail closed")
	}
}

func TestAgentIDResolvesTheHubUUID(t *testing.T) {
	c := &Client{T: agentsScript(`{"agents":[
	  {"id":"uuid-1","slug":"assistant"},
	  {"id":"uuid-2","slug":"scratch"}
	]}`)}
	got, err := c.AgentID(context.Background(), "lever", "hub", "scratch")
	if err != nil {
		t.Fatalf("AgentID: %v", err)
	}
	if got != "uuid-2" {
		t.Fatalf("AgentID = %q, want uuid-2", got)
	}
}

// An unknown agent must be an error, never "". A caller that read "" as "no
// filter" would show one agent every other agent's events.
func TestAgentIDRefusesAnUnknownAgent(t *testing.T) {
	c := &Client{T: agentsScript(`{"agents":[{"id":"uuid-1","slug":"assistant"}]}`)}
	if _, err := c.AgentID(context.Background(), "lever", "hub", "ghost"); err == nil {
		t.Fatal("an agent the hub does not list must be an error")
	}
}

func TestAgentIDRefusesARecordWithNoID(t *testing.T) {
	c := &Client{T: agentsScript(`{"agents":[{"slug":"scratch"}]}`)}
	if _, err := c.AgentID(context.Background(), "lever", "hub", "scratch"); err == nil {
		t.Fatal("a record with no id must be an error, not an empty filter")
	}
}
