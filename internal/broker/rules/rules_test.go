package rules

import "testing"

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
	if !p.MayObtain("analyst", "analyst", "db", "read") {
		t.Fatal("analyst should self-obtain db.read")
	}
}

func TestSelfObtainDeniedForExecutor(t *testing.T) {
	p := samplePolicy()
	if p.MayObtain("worker", "worker", "db", "read") {
		t.Fatal("worker (no obtain entry) must not self-obtain db.read")
	}
}

func TestSelfObtainDeniedForUngrantedCapability(t *testing.T) {
	p := samplePolicy()
	if p.MayObtain("analyst", "analyst", "db", "write") {
		t.Fatal("analyst granted db.read only, not db.write")
	}
}

func TestDelegateAllowedToListedRecipient(t *testing.T) {
	p := samplePolicy()
	if !p.MayObtain("manager", "worker", "db", "read") {
		t.Fatal("manager should delegate db.read to worker")
	}
}

func TestDelegateDeniedToUnlistedRecipient(t *testing.T) {
	p := samplePolicy()
	if p.MayObtain("manager", "stranger", "db", "read") {
		t.Fatal("manager must not delegate db.read to an unlisted recipient")
	}
}

func TestDelegateDeniedForNonDelegator(t *testing.T) {
	p := samplePolicy()
	if p.MayObtain("analyst", "worker", "db", "read") {
		t.Fatal("analyst has no delegate grant; must not delegate to worker")
	}
}

func TestDelegateGrantDoesNotEnableSelfObtain(t *testing.T) {
	p := samplePolicy()
	// manager may DELEGATE db.read but has no OBTAIN entry -> cannot self-obtain.
	if p.MayObtain("manager", "manager", "db", "read") {
		t.Fatal("a delegate grant must not satisfy a self-obtain")
	}
}

func TestObtainGrantDoesNotEnableDelegation(t *testing.T) {
	p := samplePolicy()
	// analyst may self-obtain db.read but has no DELEGATE entry.
	if p.MayObtain("analyst", "worker", "db", "read") {
		t.Fatal("a self-obtain grant must not satisfy a delegation")
	}
}

func TestUnknownAgentDenied(t *testing.T) {
	p := samplePolicy()
	if p.MayObtain("ghost", "ghost", "db", "read") {
		t.Fatal("unknown agent must be denied")
	}
}

func TestDelegateAccumulatesRecipients(t *testing.T) {
	p := NewPolicy()
	p.AllowDelegate("manager", "db", "read", "analyst")
	p.AllowDelegate("manager", "db", "read", "worker") // second call adds, not replaces
	if !p.MayObtain("manager", "analyst", "db", "read") || !p.MayObtain("manager", "worker", "db", "read") {
		t.Fatal("recipients should accumulate across AllowDelegate calls")
	}
}

func TestSelfDelegateEntryDoesNotEnableSelfObtain(t *testing.T) {
	p := NewPolicy()
	p.AllowDelegate("x", "db", "read", "x") // a delegate entry naming itself
	if p.MayObtain("x", "x", "db", "read") {
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
