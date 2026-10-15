package hubapi

import (
	"context"
	"testing"
)

const capsPath = "GET /api/v1/agents?limit=50&projectId=" + leverUUID + "&view=compact"

func TestProjectAgentCapabilitiesReadsTheCompactView(t *testing.T) {
	f := &fakeDoer{replies: map[string]reply{
		capsPath: {status: 200, body: `{"agents":[{"slug":"a","_capabilities":{"actions":["read"]}}],
		  "_capabilities":{"actions":["create","list"]},"nextCursor":"c2"}`},
		capsPath + "&cursor=c2": {status: 200, body: `{"agents":[{"slug":"b","_capabilities":{"actions":["read","stop"]}}],
		  "_capabilities":{"actions":["list"]}}`},
	}}
	d := &capDoer{fakeDoer: f}
	got, err := (&Client{T: d}).ProjectAgentCapabilities(context.Background(), leverUUID)
	if err != nil {
		t.Fatalf("ProjectAgentCapabilities: %v", err)
	}
	if len(got.Scope) != 2 || len(got.Agents) != 2 || len(got.Agents["b"]) != 2 {
		t.Fatalf("got %+v", got)
	}
	for k, c := range d.caps {
		if c != AgentListReplyCap {
			t.Errorf("%s ran with cap %d, want the agent list cap", k, c)
		}
	}
}

func TestProjectAgentCapabilitiesFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"no scope capabilities": `{"agents":[]}`,
		"an agent without":      `{"agents":[{"slug":"a"}],"_capabilities":{"actions":["list"]}}`,
	} {
		f := &fakeDoer{replies: map[string]reply{capsPath: {status: 200, body: body}}}
		if _, err := (&Client{T: f}).ProjectAgentCapabilities(context.Background(), leverUUID); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	loop := &fakeDoer{replies: map[string]reply{
		capsPath:               {status: 200, body: `{"agents":[],"_capabilities":{"actions":[]},"nextCursor":"x"}`},
		capsPath + "&cursor=x": {status: 200, body: `{"agents":[],"_capabilities":{"actions":[]},"nextCursor":"x"}`},
	}}
	if _, err := (&Client{T: loop}).ProjectAgentCapabilities(context.Background(), leverUUID); err == nil {
		t.Error("a hub that never stops paging must fail, not return a partial set")
	}
	if len(loop.calls) != agentPageCap {
		t.Errorf("made %d calls, want the %d-page limit", len(loop.calls), agentPageCap)
	}
}
