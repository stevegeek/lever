package broker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/lever/internal/scion"
)

func stagedCN(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "bootstrap.json"))
	if err != nil {
		return ""
	}
	// crude but sufficient: the envelope carries the CN as a JSON string value.
	s := string(raw)
	for _, cn := range []string{"scratch", "test-manager"} {
		if strings.Contains(s, `"`+cn+`"`) {
			return cn
		}
	}
	return "?"
}

// A lapsed RUNNING worker: healed by stage + Suspend + Resume.
func TestHealLapsedRunningWorker(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "running", ContainerStatus: "Up 2 minutes"}},
	}}
	b, spec, _ := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "scratch")

	if cn := stagedCN(t, spec.BootstrapDir); cn != "scratch" {
		t.Fatalf("staged bootstrap CN = %q, want scratch", cn)
	}
	if len(rt.suspend) != 1 || rt.suspend[0] != "scratch" {
		t.Fatalf("suspend calls = %v, want [scratch]", rt.suspend)
	}
	if len(rt.resumed) != 1 || rt.resumed[0] != "scratch" {
		t.Fatalf("resume calls = %v, want [scratch]", rt.resumed)
	}
}

// A lapsed ERROR-phase worker: healed via ResumeForce (scion#895), no suspend.
func TestHealLapsedErrorWorkerUsesForce(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "error"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "scratch")

	if len(rt.suspend) != 0 {
		t.Fatalf("suspend calls = %v, want none for error phase", rt.suspend)
	}
	if len(rt.resumeForced) != 1 || rt.resumeForced[0] != "scratch" {
		t.Fatalf("resumeForce calls = %v, want [scratch]", rt.resumeForced)
	}
}

// A lapsed worker in a mid-transition phase (e.g. "starting"): NOT bounceable
// — no verb fires; the audit trail tells the operator to run `lever up`.
func TestHealUnbounceablePhaseAuditsOnly(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "starting"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "scratch")

	if len(rt.suspend)+len(rt.resumed)+len(rt.resumeForced) != 0 {
		t.Fatalf("verbs fired for unbounceable phase: suspend %v resume %v force %v",
			rt.suspend, rt.resumed, rt.resumeForced)
	}
}

// A lapsed suspended/stopped worker: plain Resume.
func TestHealLapsedSuspendedWorkerPlainResume(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "suspended"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "scratch")

	if len(rt.suspend) != 0 || len(rt.resumeForced) != 0 || len(rt.resumed) != 1 {
		t.Fatalf("verbs = suspend %v force %v resume %v, want plain resume only",
			rt.suspend, rt.resumeForced, rt.resumed)
	}
}

// The MANAGER lapses: ticket staged to the manager bootstrap dir under the
// manager cert CN, bounce addressed to the manager's scion SLUG.
func TestHealLapsedManager(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "appname", Phase: "running", ContainerStatus: "Up 5 minutes"}},
	}}
	b, _, managerDir := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "test-manager")

	if cn := stagedCN(t, managerDir); cn != "test-manager" {
		t.Fatalf("staged manager bootstrap CN = %q, want test-manager", cn)
	}
	if len(rt.suspend) != 1 || rt.suspend[0] != "appname" {
		t.Fatalf("suspend calls = %v, want [appname] (the scion slug, not the CN)", rt.suspend)
	}
	if len(rt.resumed) != 1 || rt.resumed[0] != "appname" {
		t.Fatalf("resume calls = %v, want [appname]", rt.resumed)
	}
}

// Gate: revoked identities are NEVER healed — expiry stays a kill-switch.
func TestHealRefusesRevoked(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "error"}},
	}}
	b, spec, _ := reenrolBroker(t, rt, "all")
	b.Revoke("scratch")
	b.healLapse(context.Background(), "scratch")

	if _, err := os.Stat(filepath.Join(spec.BootstrapDir, "bootstrap.json")); err == nil {
		t.Fatal("revoked identity must not get a staged ticket")
	}
	if len(rt.resumed)+len(rt.resumeForced)+len(rt.suspend) != 0 {
		t.Fatal("revoked identity must not be bounced")
	}
}

// Gate: unknown CNs (not the manager, not a configured worker) are ignored.
func TestHealIgnoresUnknownCN(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{}}
	b, _, _ := reenrolBroker(t, rt, "all")
	b.healLapse(context.Background(), "интруder")
	if len(rt.resumed)+len(rt.resumeForced)+len(rt.suspend) != 0 {
		t.Fatal("unknown CN must not trigger any runtime verb")
	}
}

// Gate: mode "manager" heals the manager but drops workers; mode "off" drops all.
func TestHealModeGate(t *testing.T) {
	mk := func(mode string) (*Broker, *fakeRuntime) {
		rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
			testInstanceProject: {
				{Slug: "scratch", Phase: "error"},
				{Slug: "appname", Phase: "error"},
			},
		}}
		b, _, _ := reenrolBroker(t, rt, mode)
		return b, rt
	}

	b, rt := mk("manager")
	b.healLapse(context.Background(), "scratch")
	if len(rt.resumeForced) != 0 {
		t.Fatal("mode=manager must not heal a worker")
	}
	b.healLapse(context.Background(), "test-manager")
	if len(rt.resumeForced) != 1 || rt.resumeForced[0] != "appname" {
		t.Fatalf("mode=manager must heal the manager, got %v", rt.resumeForced)
	}

	b, rt = mk("off")
	b.healLapse(context.Background(), "test-manager")
	b.healLapse(context.Background(), "scratch")
	if len(rt.resumeForced)+len(rt.resumed)+len(rt.suspend) != 0 {
		t.Fatal("mode=off must heal nothing")
	}
	if b.lapseFunc() != nil {
		t.Fatal("mode=off must not even install the lapse hook")
	}
}

// Cooldown + attempt cap: repeated FAILING heals for one CN run once per
// cooldown window, at most reenrolMaxAttempts per burst; a long-quiet CN
// starts a fresh burst; a SUCCESS resets the counter immediately.
func TestHealCooldownAndCap(t *testing.T) {
	rt := &fakeRuntime{staticPhases: true, agents: map[string][]scion.Agent{
		testInstanceProject: {{Slug: "scratch", Phase: "error"}},
	}}
	b, _, _ := reenrolBroker(t, rt, "all")
	now := time.Unix(1_700_000_000, 0)
	b.reenrolNow = func() time.Time { return now }
	rt.resumeForceErr = context.DeadlineExceeded // every heal attempt FAILS

	b.healLapse(context.Background(), "scratch")
	b.healLapse(context.Background(), "scratch") // within cooldown: dropped
	if len(rt.resumeForced) != 1 {
		t.Fatalf("attempts within cooldown = %d, want 1", len(rt.resumeForced))
	}

	for i := 0; i < 4; i++ { // each round passes the cooldown; total stays inside the reset window
		now = now.Add(reenrolCooldown + time.Minute)
		b.healLapse(context.Background(), "scratch")
	}
	if len(rt.resumeForced) != reenrolMaxAttempts {
		t.Fatalf("burst attempts = %d, want cap %d", len(rt.resumeForced), reenrolMaxAttempts)
	}

	// A long-quiet CN gets a fresh burst (the cap must not be sticky forever).
	now = now.Add(reenrolResetAfter + time.Minute)
	b.healLapse(context.Background(), "scratch")
	if len(rt.resumeForced) != reenrolMaxAttempts+1 {
		t.Fatalf("post-quiet attempts = %d, want %d", len(rt.resumeForced), reenrolMaxAttempts+1)
	}

	// A SUCCESS resets the counter: the next lapse after cooldown heals again.
	rt.resumeForceErr = nil
	now = now.Add(reenrolCooldown + time.Minute)
	b.healLapse(context.Background(), "scratch") // succeeds, resets tries
	now = now.Add(reenrolCooldown + time.Minute)
	b.healLapse(context.Background(), "scratch")
	if len(rt.resumeForced) != reenrolMaxAttempts+3 {
		t.Fatalf("post-success attempts = %d, want %d", len(rt.resumeForced), reenrolMaxAttempts+3)
	}
}
