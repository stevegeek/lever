package rules

import "testing"

// mayObtain is MayObtainRule's allow bit, for tests that do not care which
// rule matched.
func mayObtain(p *Policy, requester, boundTo, tool, op string) bool {
	_, ok := p.MayObtainRule(requester, boundTo, tool, op)
	return ok
}

// samplePolicy models an example: a manager that may delegate
// db.read to analyst/worker, an analyst that may self-obtain db.read, and a
// pure-executor worker with no obtain entries.
func samplePolicy() *Policy {
	p := NewPolicy()
	p.AllowDelegate("manager", "db", "read", "analyst", "worker")
	p.AllowObtain("analyst", "db", "read")
	// worker is a pure executor: it appears only as a delegate recipient, with
	// no obtain entry of its own.
	return p
}

func TestSelfObtainAllowed(t *testing.T) {
	p := samplePolicy()
	if !mayObtain(p, "analyst", "analyst", "db", "read") {
		t.Fatal("analyst should self-obtain db.read")
	}
}

func TestSelfObtainDeniedForExecutor(t *testing.T) {
	p := samplePolicy()
	if mayObtain(p, "worker", "worker", "db", "read") {
		t.Fatal("worker (no obtain entry) must not self-obtain db.read")
	}
}

func TestSelfObtainDeniedForUngrantedCapability(t *testing.T) {
	p := samplePolicy()
	if mayObtain(p, "analyst", "analyst", "db", "write") {
		t.Fatal("analyst granted db.read only, not db.write")
	}
}

func TestDelegateAllowedToListedRecipient(t *testing.T) {
	p := samplePolicy()
	if !mayObtain(p, "manager", "worker", "db", "read") {
		t.Fatal("manager should delegate db.read to worker")
	}
}

func TestDelegateDeniedToUnlistedRecipient(t *testing.T) {
	p := samplePolicy()
	if mayObtain(p, "manager", "stranger", "db", "read") {
		t.Fatal("manager must not delegate db.read to an unlisted recipient")
	}
}

func TestDelegateDeniedForNonDelegator(t *testing.T) {
	p := samplePolicy()
	if mayObtain(p, "analyst", "worker", "db", "read") {
		t.Fatal("analyst has no delegate grant; must not delegate to worker")
	}
}

func TestDelegateGrantDoesNotEnableSelfObtain(t *testing.T) {
	p := samplePolicy()
	// manager may DELEGATE db.read but has no OBTAIN entry -> cannot self-obtain.
	if mayObtain(p, "manager", "manager", "db", "read") {
		t.Fatal("a delegate grant must not satisfy a self-obtain")
	}
}

func TestObtainGrantDoesNotEnableDelegation(t *testing.T) {
	p := samplePolicy()
	// analyst may self-obtain db.read but has no DELEGATE entry.
	if mayObtain(p, "analyst", "worker", "db", "read") {
		t.Fatal("a self-obtain grant must not satisfy a delegation")
	}
}

func TestUnknownAgentDenied(t *testing.T) {
	p := samplePolicy()
	if mayObtain(p, "ghost", "ghost", "db", "read") {
		t.Fatal("unknown agent must be denied")
	}
}

func TestDelegateAccumulatesRecipients(t *testing.T) {
	p := NewPolicy()
	p.AllowDelegate("manager", "db", "read", "analyst")
	p.AllowDelegate("manager", "db", "read", "worker") // second call adds, not replaces
	if !mayObtain(p, "manager", "analyst", "db", "read") || !mayObtain(p, "manager", "worker", "db", "read") {
		t.Fatal("recipients should accumulate across AllowDelegate calls")
	}
}

func TestSelfDelegateEntryDoesNotEnableSelfObtain(t *testing.T) {
	p := NewPolicy()
	p.AllowDelegate("x", "db", "read", "x") // a delegate entry naming itself
	if mayObtain(p, "x", "x", "db", "read") {
		t.Fatal("a self-targeted delegate entry must not satisfy a self-obtain (self-actions route through obtain)")
	}
}

func TestMayObtainRuleNamesTheMatchedGrant(t *testing.T) {
	p := NewPolicy()
	p.AllowObtain("analyst", "db", "read")
	p.AllowDelegate("manager", "utilities", "*", "scratch")

	rule, ok := p.MayObtainRule("analyst", "analyst", "db", "read")
	if !ok || rule != "obtain:analyst:db.read" {
		t.Fatalf("self-obtain rule = %q, ok=%v; want obtain:analyst:db.read, true", rule, ok)
	}
	rule, ok = p.MayObtainRule("manager", "scratch", "utilities", "*")
	if !ok || rule != "delegate:manager->scratch:utilities.*" {
		t.Fatalf("delegate rule = %q, ok=%v; want delegate:manager->scratch:utilities.*, true", rule, ok)
	}
}

func TestMayObtainRuleFailsClosed(t *testing.T) {
	p := NewPolicy()
	p.AllowObtain("analyst", "db", "read")
	if rule, ok := p.MayObtainRule("analyst", "analyst", "db", "write"); ok || rule != "" {
		t.Fatalf("ungranted op: rule=%q ok=%v, want empty+false", rule, ok)
	}
	if rule, ok := p.MayObtainRule("analyst", "other", "db", "read"); ok || rule != "" {
		t.Fatalf("obtain grant must not authorize delegation: rule=%q ok=%v, want empty+false", rule, ok)
	}
	if rule, ok := p.MayObtainRule("nobody", "nobody", "db", "read"); ok || rule != "" {
		t.Fatalf("unknown agent: rule=%q ok=%v, want empty+false", rule, ok)
	}
}

// AddAgent replaces an agent's grants whole; RemoveAgent drops them.
func TestAddAndRemoveAgent(t *testing.T) {
	p := NewPolicy()
	p.AddAgent("s-aaaaaa", []Grant{{"db", "read"}}, []Delegation{{Tool: "db", Op: "read", To: []string{"worker"}}})
	if _, ok := p.MayObtainRule("s-aaaaaa", "s-aaaaaa", "db", "read"); !ok {
		t.Fatal("obtain")
	}
	if _, ok := p.MayObtainRule("s-aaaaaa", "worker", "db", "read"); !ok {
		t.Fatal("delegate")
	}
	p.AddAgent("s-aaaaaa", []Grant{{"db", "write"}}, nil)
	if _, ok := p.MayObtainRule("s-aaaaaa", "s-aaaaaa", "db", "read"); ok {
		t.Fatal("AddAgent kept an old grant")
	}
	p.RemoveAgent("s-aaaaaa")
	if _, ok := p.MayObtainRule("s-aaaaaa", "s-aaaaaa", "db", "write"); ok {
		t.Fatal("RemoveAgent kept a grant")
	}
}

// A "template:" recipient matches only what the resolver calls an active
// instance of that template, and never as a literal name.
func TestTemplateRecipient(t *testing.T) {
	p := NewPolicy()
	p.AllowDelegate("manager", "db", "read", TemplateRefPrefix+"session")
	if _, ok := p.MayObtainRule("manager", "s-aaaaaa", "db", "read"); ok {
		t.Fatal("matched with no resolver")
	}
	p.SetTemplateResolver(func(n string) (string, bool) {
		switch n {
		case "s-aaaaaa":
			return "session", true
		case "o-aaaaaa":
			return "other", true
		}
		return "", false
	})
	if rule, ok := p.MayObtainRule("manager", "s-aaaaaa", "db", "read"); !ok || rule == "" {
		t.Fatal("active instance refused")
	}
	for _, bound := range []string{"o-aaaaaa", "s-bbbbbb", TemplateRefPrefix + "session"} {
		if _, ok := p.MayObtainRule("manager", bound, "db", "read"); ok {
			t.Fatalf("%q matched", bound)
		}
	}
}

// Reads and writes may run at once (go test -race).
func TestPolicyConcurrent(t *testing.T) {
	p := NewPolicy()
	p.SetTemplateResolver(func(string) (string, bool) { return "", false })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			p.AddAgent("x", []Grant{{"db", "read"}}, nil)
			p.RemoveAgent("x")
		}
	}()
	for range 1000 {
		p.MayObtainRule("x", "x", "db", "read")
	}
	<-done
}
