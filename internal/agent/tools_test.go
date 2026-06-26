package agent

import (
	"context"
	"testing"
)

func TestListToolsReturnsRegisteredTools(t *testing.T) {
	env := testBroker(t)
	regDB(t, env)                 // registers the "db" tool in the registry
	id := enrolManager(t, env.CA) // any enrolled identity with mTLS access
	client, err := id.Client()
	if err != nil {
		t.Fatal(err)
	}
	tools, err := ListTools(context.Background(), env.Server.URL, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0] != "db" {
		t.Fatalf("tools = %v, want [db]", tools)
	}
}
