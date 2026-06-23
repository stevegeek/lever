package rules

import "testing"

// samplePolicy models an example: a manager that may delegate
// db.read to analyst/worker, an analyst that may self-obtain db.read, and a
// pure-executor worker with no obtain entries.
func samplePolicy() *Policy {
	p := NewPolicy()
	p.AllowDelegate("manager", "db", "read", "analyst", "worker")
	p.AllowObtain("analyst", "db", "read")
	// worker: registered implicitly by being a delegate recipient; no obtain.
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
