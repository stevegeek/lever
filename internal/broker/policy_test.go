package broker

import "testing"

func samplePolicy() Policy {
	return Policy{
		ManagerIdentity: "manager",
		Grants:          map[string][]string{"scratch": {"qmd", "github.api"}},
		Routes: []Route{
			{Operation: "qmd", PathPrefix: "/mcp/qmd/", Backend: "http://127.0.0.1:3101"},
			{Operation: "github.api", PathPrefix: "/proxy/github/", Backend: "https://api.github.com", SecretName: "github"},
		},
		Secrets: map[string]string{"github": "ghp_xxx"},
	}
}

func TestGrantsFor(t *testing.T) {
	p := samplePolicy()
	got, ok := p.GrantsFor("scratch")
	if !ok || len(got) != 2 {
		t.Fatalf("GrantsFor(scratch) = %v, %v", got, ok)
	}
	if _, ok := p.GrantsFor("nope"); ok {
		t.Error("unexpected grants for unknown grove")
	}
}

func TestRouteForPathLongestPrefix(t *testing.T) {
	p := samplePolicy()
	r, ok := p.RouteForPath("/mcp/qmd/tools/call")
	if !ok || r.Operation != "qmd" {
		t.Fatalf("RouteForPath = %+v, %v", r, ok)
	}
	if _, ok := p.RouteForPath("/unknown"); ok {
		t.Error("unexpected route for unknown path")
	}
}

func TestSecretLookup(t *testing.T) {
	p := samplePolicy()
	if v, ok := p.Secret("github"); !ok || v != "ghp_xxx" {
		t.Fatalf("Secret(github) = %q, %v", v, ok)
	}
}
